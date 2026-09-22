package control_test

import (
	"crypto/tls"
	"net"
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
