package ca

import (
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// Browsers accept ECDSA P-256 chains for TLS, not Ed25519; the UI CA and its leaves must be ECDSA.
func TestUICAIsBrowserCompatible(t *testing.T) {
	now := time.Now()
	ui, err := GenerateUI("cid", now)
	if err != nil {
		t.Fatal(err)
	}
	if ui.Cert.PublicKeyAlgorithm != x509.ECDSA || ui.Cert.SignatureAlgorithm != x509.ECDSAWithSHA256 || !ui.Cert.IsCA {
		t.Fatalf("UI CA: key %v sig %v isCA %v", ui.Cert.PublicKeyAlgorithm, ui.Cert.SignatureAlgorithm, ui.Cert.IsCA)
	}

	leaf, _, err := ui.IssueServer("node-1", "host-1", []net.IP{net.ParseIP("10.0.2.15")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.PublicKeyAlgorithm != x509.ECDSA || leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("UI leaf: key %v sig %v", leaf.PublicKeyAlgorithm, leaf.SignatureAlgorithm)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ui.Cert)
	for _, name := range []string{UIVIPHostname, "node-1", "host-1", "localhost", "10.0.2.15", "127.0.0.1"} {
		opts := x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if _, err := leaf.Verify(opts); err != nil {
			t.Errorf("leaf does not verify for %q: %v", name, err)
		}
	}
}

func TestUICASealRoundTrip(t *testing.T) {
	secret := make([]byte, 32)
	ui, err := GenerateUI("cid", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := SealUIKey(ui.Priv, secret)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := UnsealUIKey(sealed, secret)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Equal(ui.Priv) {
		t.Fatal("unsealed UI CA key differs")
	}
}
