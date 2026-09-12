package ca

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sort"
	"testing"
	"time"
)

// Certificate timeline for a cert issued at t0 with 30-day validity:
// renewed once it is 20 days old (10 days remain); expired at 30 days.
var (
	now    = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	later1 = now.Add(19 * 24 * time.Hour) // 11 days remain → no renew yet
	later2 = now.Add(21 * 24 * time.Hour) // 9 days remain → renew
	later3 = now.Add(31 * 24 * time.Hour) // expired
)

func newTestCA(t *testing.T, clusterID string) *CA {
	t.Helper()
	c, err := Generate(clusterID, now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return c
}

func TestGenerate(t *testing.T) {
	c := newTestCA(t, "test-cluster")
	if got := c.Cert.Subject.CommonName; got != "Expanse Cluster CA test-cluster" {
		t.Errorf("CN = %q", got)
	}
	if !c.Cert.IsCA {
		t.Error("cert not marked IsCA")
	}
	if c.Cert.PublicKeyAlgorithm != x509.Ed25519 {
		t.Errorf("alg = %v, want Ed25519", c.Cert.PublicKeyAlgorithm)
	}
	if valid := c.Cert.NotAfter.Sub(c.Cert.NotBefore); valid < 10*365*24*time.Hour {
		t.Errorf("validity = %v, want ≥ 10 years", valid)
	}
	// Self-signed: verifies against itself.
	if _, err := c.Cert.Verify(x509.VerifyOptions{Roots: (&Bundle{CAs: []*x509.Certificate{c.Cert}}).Pool(), CurrentTime: now}); err != nil {
		t.Errorf("self-verify: %v", err)
	}
}

func TestMarshalUnmarshalCert(t *testing.T) {
	c := newTestCA(t, "m")
	pemBytes := MarshalCert(c.Cert)
	got, err := UnmarshalCert(pemBytes)
	if err != nil {
		t.Fatalf("UnmarshalCert: %v", err)
	}
	if !got.Equal(c.Cert) {
		t.Error("round-trip not equal")
	}
	if _, err := UnmarshalCert([]byte("not pem")); err == nil {
		t.Error("expected error for non-PEM input")
	}
}

func TestIssueAndVerify(t *testing.T) {
	c := newTestCA(t, "v")
	cert, _, err := c.IssueNode("node-1", "host1", []net.IP{net.ParseIP("10.0.0.1")}, now)
	if err != nil {
		t.Fatalf("IssueNode: %v", err)
	}
	if cert.Subject.CommonName != "node-1" {
		t.Errorf("CN = %q, want node-1", cert.Subject.CommonName)
	}
	sans := append([]string{}, cert.DNSNames...)
	sort.Strings(sans)
	if len(sans) != 2 || sans[0] != "host1" || sans[1] != "node-1" {
		t.Errorf("DNS SANs = %v, want [host1 node-1]", sans)
	}
	if len(cert.IPAddresses) != 1 || !cert.IPAddresses[0].Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("IP SANs = %v", cert.IPAddresses)
	}
	if valid := cert.NotAfter.Sub(cert.NotBefore); valid < 30*24*time.Hour {
		t.Errorf("validity = %v, want ≥ 30 days", valid)
	}
	b, err := NewBundle(c.Cert)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	if err := b.VerifyNode(cert, now); err != nil {
		t.Errorf("VerifyNode: %v", err)
	}
	// Hostname == nodeID: no duplicate SAN.
	c2, _, _ := c.IssueNode("node-2", "node-2", nil, now)
	if len(c2.DNSNames) != 1 {
		t.Errorf("DNS SANs = %v, want only node-2", c2.DNSNames)
	}
}

func TestNeedsRenewal(t *testing.T) {
	c := newTestCA(t, "r")
	cert, _, _ := c.IssueNode("n", "h", nil, now)
	cases := []struct {
		at   time.Time
		want bool
		desc string
	}{
		{now.Add(24 * time.Hour), false, "1 day old"},
		{later1, false, "19 days old (11 days remain)"},
		{later2, true, "21 days old (9 days remain)"},
		{later3, true, "expired"},
	}
	for _, tc := range cases {
		if got := NeedsRenewal(cert, tc.at); got != tc.want {
			t.Errorf("NeedsRenewal(%s) = %v, want %v", tc.desc, got, tc.want)
		}
		if got := Expired(cert, tc.at); got != tc.at.After(cert.NotAfter) {
			t.Errorf("Expired(%s) = %v, want %v", tc.desc, got, !got)
		}
	}
}

func TestVerifyRejects(t *testing.T) {
	ca1 := newTestCA(t, "a")
	ca2 := newTestCA(t, "b")
	b1, _ := NewBundle(ca1.Cert)
	cert1, _, _ := ca1.IssueNode("n1", "h", nil, now)
	cert2, _, _ := ca2.IssueNode("n2", "h", nil, now)

	cases := []struct {
		desc string
		cert *x509.Certificate
		at   time.Time
	}{
		{"expired cert", cert1, later3},
		{"not-yet-valid cert", cert1, now.Add(-NodeCertValidity)},
		{"cert from wrong CA", cert2, now},
	}
	for _, tc := range cases {
		if err := b1.VerifyNode(tc.cert, tc.at); err == nil {
			t.Errorf("%s: VerifyNode accepted", tc.desc)
		}
	}
}

func TestBundleTwoCAs(t *testing.T) {
	ca1 := newTestCA(t, "a")
	ca2 := newTestCA(t, "b")
	cert1, _, _ := ca1.IssueNode("n1", "h", nil, now)
	cert2, _, _ := ca2.IssueNode("n2", "h", nil, now)

	// Single-CA bundle rejects the other CA's cert.
	b1, _ := NewBundle(ca1.Cert)
	if err := b1.VerifyNode(cert2, now); err == nil {
		t.Error("single-CA bundle accepted wrong-CA cert")
	}

	// Two-CA bundle accepts both (rotation window).
	b2, _ := NewBundle(ca1.Cert, ca2.Cert)
	for i, cert := range []*x509.Certificate{cert1, cert2} {
		if err := b2.VerifyNode(cert, now); err != nil {
			t.Errorf("two-CA bundle rejected cert %d: %v", i, err)
		}
	}
	if b2.Primary().Equal(ca2.Cert) {
		t.Error("first bundle CA should be primary")
	}

	// Non-CA certs are rejected from bundles.
	if _, err := NewBundle(cert1); err == nil {
		t.Error("NewBundle accepted a non-CA cert")
	}
	if _, err := NewBundle(); err == nil {
		t.Error("NewBundle accepted empty input")
	}
}

func TestParseCA(t *testing.T) {
	c := newTestCA(t, "p")
	pem := MarshalCert(c.Cert)
	got, err := ParseCA(c.Priv, pem)
	if err != nil {
		t.Fatalf("ParseCA: %v", err)
	}
	if !got.Cert.Equal(c.Cert) {
		t.Error("ParseCA cert mismatch")
	}
	// Wrong-size key.
	if _, err := ParseCA(c.Priv[:ed25519.SeedSize-1], pem); err == nil {
		t.Error("ParseCA accepted wrong-size key")
	}
	other := newTestCA(t, "q")
	if _, err := ParseCA(other.Priv, pem); err == nil {
		t.Error("ParseCA accepted mismatched key/cert pair")
	}
	// Non-Ed25519 certificate.
	_, eckey, _ := ed25519.GenerateKey(nil)
	_ = eckey
	if _, err := ParseCA(c.Priv, MarshalCert(ecdsaCACert(t))); err == nil {
		t.Error("ParseCA accepted non-Ed25519 cert")
	}
	if _, err := ParseCA(c.Priv, []byte("junk")); err == nil {
		t.Error("ParseCA accepted junk PEM")
	}
}

// ecdsaCACert creates a self-signed ECDSA CA cert (to exercise the
// non-Ed25519 rejection path).
func ecdsaCACert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ecdsa ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert
}

func TestSealUnsealKey(t *testing.T) {
	c := newTestCA(t, "s")
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	blob, err := SealKey(c.Priv, secret)
	if err != nil {
		t.Fatalf("SealKey: %v", err)
	}
	got, err := UnsealKey(blob, secret)
	if err != nil {
		t.Fatalf("UnsealKey: %v", err)
	}
	if !got.Equal(c.Priv) {
		t.Error("unsealed key does not match")
	}
	// Wrong secret → failure.
	bad := make([]byte, 32)
	bad[0] = 0xFF
	if _, err := UnsealKey(blob, bad); err == nil {
		t.Error("UnsealKey accepted wrong cluster secret")
	}
	// Tampered blob → failure.
	blob[10] ^= 0xFF
	if _, err := UnsealKey(blob, secret); err == nil {
		t.Error("UnsealKey accepted tampered blob")
	}
}

func TestKeyPEMRoundTrip(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	pem, err := KeyPEM(priv)
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	got, err := ParseKeyPEM(pem)
	if err != nil {
		t.Fatalf("ParseKeyPEM: %v", err)
	}
	if !got.Equal(priv) {
		t.Error("key round-trip mismatch")
	}
	if _, err := ParseKeyPEM([]byte("junk")); err == nil {
		t.Error("ParseKeyPEM accepted junk")
	}
}

// TestTLSHandshake exercises the full mTLS path: correct certs succeed,
// wrong-CA and unknown-CN certs fail.
func TestTLSHandshake(t *testing.T) {
	caA := newTestCA(t, "tls")
	caB := newTestCA(t, "other")
	bundle, _ := NewBundle(caA.Cert)

	mkClient := func(ca *CA, nodeID string) tls.Certificate {
		// Issued at real wall time so the handshake's built-in clock check passes.
		cert, key, err := ca.IssueNode(nodeID, nodeID, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
		if err != nil {
			t.Fatalf("IssueNode: %v", err)
		}
		return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	}
	serverCert := mkClient(caA, "server-1")

	known := func() []string { return SortedIDs([]string{"server-1", "client-1"}) }
	cfg := TLSConfig(bundle, serverCert, known)

	runPair := func(clientCert tls.Certificate, expectErr bool) {
		t.Helper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer ln.Close()
		h := make(chan error, 2)
		go func() {
			c, aerr := ln.Accept()
			if aerr != nil {
				h <- aerr
				return
			}
			s := tls.Server(c, cfg)
			h <- s.Handshake()
			c.Close()
		}()
		go func() {
			c, derr := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{clientCert},
				RootCAs:      bundle.Pool(),
			})
			if derr != nil {
				h <- derr
				return
			}
			h <- c.Handshake()
			c.Close()
		}()
		var first error
		for i := 0; i < 2; i++ {
			if e := <-h; e != nil && first == nil {
				first = e
			}
		}
		if expectErr && first == nil {
			t.Error("handshake succeeded, want failure")
		}
		if !expectErr && first != nil {
			t.Errorf("handshake failed: %v", first)
		}
	}

	runPair(mkClient(caA, "client-1"), false)        // good client
	runPair(mkClient(caA, "intruder"), true)         // unknown CN
	runPair(mkClient(caB, "client-1"), true)         // wrong CA
	runPair(tls.Certificate{Certificate: nil}, true) // no client cert
}

func TestVerifyPeerCNNoCert(t *testing.T) {
	cb := VerifyPeerCN(func() []string { return []string{"a"} })
	if err := cb(nil, nil); err == nil {
		t.Error("expected error when no cert presented")
	}
	if err := cb([][]byte{{0x01, 0x02}}, nil); err == nil {
		t.Error("expected error for unparseable cert")
	}
	c := newTestCA(t, "cn")
	cert, _, _ := c.IssueNode("a", "a", nil, now)
	if err := cb([][]byte{cert.Raw}, nil); err != nil {
		t.Errorf("valid CN rejected: %v", err)
	}
}

func TestSortedIDs(t *testing.T) {
	got := SortedIDs([]string{"c", "a", "b"})
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("SortedIDs = %v, want %v", got, want)
		}
	}
}
