package control_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/control"
)

// TestUIServerTLS covers the web UI's TLS config builder (D5: the
// node's own cluster-CA-issued cert, no client cert required — unlike
// the internal mTLS endpoints, browsers are the callers here).
func TestUIServerTLS(t *testing.T) {
	r := newRig(t, "ui-tls")

	cfg, err := control.UIServerTLS(r.dataDir)
	if err != nil {
		t.Fatalf("UIServerTLS: %v", err)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Errorf("ClientAuth = %v, want NoClientCert (browsers present none)", cfg.ClientAuth)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(conn, cfg)
		_ = tlsConn.Handshake()
		_ = tlsConn.Close()
	}()

	// A plain client that does not trust the cluster CA still completes
	// the handshake (InsecureSkipVerify stands in for a browser's own
	// trust decision, which is out of scope here): the point under test
	// is that the server does not demand a client certificate.
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
}

// TestUIServerTLSMissingIdentity covers the identity-load failure path.
func TestUIServerTLSMissingIdentity(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := control.UIServerTLS(emptyDir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, want missing cert", err)
	}
}

// TestWebUITLSIsBrowserCompatible: the web UI serves an ECDSA chain (browsers reject Ed25519) that
// verifies against the ui-ca.pem written for operators, and every call shares one cluster UI CA.
func TestWebUITLSIsBrowserCompatible(t *testing.T) {
	r := newRig(t, "ui-ecdsa")
	ctx := context.Background()
	res := r.initRes

	cfg, err := control.WebUITLS(ctx, res.Store, res.Secret, r.dataDir, "n1", res.ClusterID)
	if err != nil {
		t.Fatalf("WebUITLS: %v", err)
	}
	pemBytes, err := os.ReadFile(filepath.Join(r.dataDir, control.UICAFile))
	if err != nil {
		t.Fatalf("ui-ca.pem not written: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pemBytes) {
		t.Fatal("ui-ca.pem holds no certificate")
	}

	leaf := handshakeLeaf(t, cfg, &tls.Config{RootCAs: roots, ServerName: "expanse-ui"})
	if leaf.PublicKeyAlgorithm != x509.ECDSA || leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
		t.Fatalf("UI leaf key %v sig %v, want ECDSA/ECDSAWithSHA256", leaf.PublicKeyAlgorithm, leaf.SignatureAlgorithm)
	}

	again, err := control.WebUITLS(ctx, res.Store, res.Secret, t.TempDir(), "n2", res.ClusterID)
	if err != nil {
		t.Fatalf("second WebUITLS: %v", err)
	}
	handshakeLeaf(t, again, &tls.Config{RootCAs: roots, ServerName: "expanse-ui"}) // same UI CA verifies it
}

func handshakeLeaf(t *testing.T, server, client *tls.Config) *x509.Certificate {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		s := tls.Server(conn, server)
		_ = s.Handshake()
		_ = s.Close()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	return conn.ConnectionState().PeerCertificates[0]
}

// An agent restart serves the same UI certificate, so a browser's exception for it still holds;
// an unreadable saved certificate is replaced.
func TestWebUITLSKeepsItsCertificateAcrossRestarts(t *testing.T) {
	r := newRig(t, "ui-keep")
	ctx := context.Background()
	res := r.initRes
	serve := func() *x509.Certificate {
		t.Helper()
		cfg, err := control.WebUITLS(ctx, res.Store, res.Secret, r.dataDir, "n1", res.ClusterID)
		if err != nil {
			t.Fatalf("WebUITLS: %v", err)
		}
		return handshakeLeaf(t, cfg, &tls.Config{InsecureSkipVerify: true})
	}

	first := serve()
	if again := serve(); again.SerialNumber.Cmp(first.SerialNumber) != 0 {
		t.Errorf("restart issued a new UI certificate: serial %v -> %v", first.SerialNumber, again.SerialNumber)
	}
	saved := filepath.Join(r.dataDir, control.UICertFile)
	if fi, err := os.Stat(saved); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("saved UI certificate %s: %v, mode %v; want 0600", saved, err, fi)
	}

	if err := os.WriteFile(saved, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fresh := serve(); fresh.SerialNumber.Cmp(first.SerialNumber) == 0 {
		t.Error("an unreadable saved certificate was not replaced")
	}
}
