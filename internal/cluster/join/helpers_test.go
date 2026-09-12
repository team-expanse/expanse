package join_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/expanse/expanse/internal/store/raftstore"
)

func freeJoinPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("grab port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitJoinLeader(t *testing.T, st *raftstore.Store, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if st.IsLeader() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no leader within timeout")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// makeCSR returns a DER-encoded CSR with CN=nodeID.
func makeCSR(t *testing.T, nodeID string) ([]byte, error) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: nodeID, Organization: []string{"Expanse"}},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	return x509.CreateCertificateRequest(rand.Reader, tmpl, key)
}

func parseCertPEM(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
