package ca

import (
	"crypto/tls"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestARemovedNodeCannotResumeASession: a session ticket from before the removal must not
// skip the CN-membership check, which a resumed handshake never calls VerifyPeerCertificate for.
func TestARemovedNodeCannotResumeASession(t *testing.T) {
	ca := newTestCA(t, "resume")
	bundle, _ := NewBundle(ca.Cert)
	issue := func(id string) tls.Certificate {
		cert, key, err := ca.IssueNode(id, id, []net.IP{net.ParseIP("127.0.0.1")}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	}
	server, peer := issue("server-1"), issue("n2")
	var mu sync.Mutex
	members := []string{"n2", "server-1"}
	known := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(members) }

	for name, cfg := range map[string]*tls.Config{
		"static":  TLSConfig(bundle, server, known),
		"dynamic": TLSConfigDynamic(func() (*Bundle, error) { return bundle, nil }, func() (*tls.Certificate, error) { return &server, nil }, known),
	} {
		t.Run(name, func(t *testing.T) {
			mu.Lock()
			members = []string{"n2", "server-1"}
			mu.Unlock()
			client := &tls.Config{
				MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{peer}, RootCAs: bundle.Pool(),
				ServerName: "server-1", ClientSessionCache: tls.NewLRUClientSessionCache(4),
			}
			if _, err := handshake(t, cfg, client); err != nil {
				t.Fatalf("a member's first handshake: %v", err)
			}
			mu.Lock()
			members = []string{"server-1"}
			mu.Unlock()
			if resumed, err := handshake(t, cfg, client); err == nil {
				t.Errorf("a removed node was let back in (resumed=%v)", resumed)
			}
		})
	}
}

// handshake connects once and reads a byte, so the client also stores the session ticket;
// it returns whether the session was resumed and the server's handshake error.
func handshake(t *testing.T, server, client *tls.Config) (bool, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		s := tls.Server(c, server)
		if err := s.Handshake(); err != nil {
			done <- err
			return
		}
		_, err = s.Write([]byte{1})
		done <- err
	}()
	c, err := tls.Dial("tcp", ln.Addr().String(), client)
	resumed := false
	if err == nil {
		resumed = c.ConnectionState().DidResume
		_, _ = c.Read(make([]byte, 1))
		c.Close()
	}
	return resumed, <-done
}
