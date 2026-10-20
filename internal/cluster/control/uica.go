package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
)

// UICAKey holds the cluster's web UI CA (ECDSA, sealed like CATrust); UICAFile is its certificate
// on each node's disk, for operators to import into a browser.
const (
	UICAKey  = "/cluster/ui/ca"
	UICAFile = "ca/ui-ca.pem"
	// UICertFile keeps the node's UI certificate and key, so a restart serves the one browsers accepted.
	UICertFile = "ca/ui-server.pem"
)

// EnsureUICA returns the cluster's UI CA, creating it once (expect-absent CAS) if the cluster has none.
func EnsureUICA(ctx context.Context, st store.Store, secret []byte, clusterID string) (*ca.UICA, error) {
	if e, err := st.Get(ctx, store.Key(UICAKey)); err == nil {
		return decodeUICA(e.Value, secret)
	} else if !errors.Is(err, errors.KindNotFound) {
		return nil, err
	}
	fresh, err := ca.GenerateUI(clusterID, time.Now())
	if err != nil {
		return nil, err
	}
	sealed, err := ca.SealUIKey(fresh.Priv, secret)
	if err != nil {
		return nil, err
	}
	v, err := json.Marshal(CAEntry{CertPEM: ca.MarshalCert(fresh.Cert), SealedKey: sealed})
	if err != nil {
		return nil, err
	}
	if _, err := st.CompareAndSwap(ctx, store.Key(UICAKey), 0, v); err != nil {
		if errors.Is(err, errors.KindConflict) {
			return EnsureUICA(ctx, st, secret, clusterID) // another node created it first
		}
		return nil, err
	}
	return fresh, nil
}

func decodeUICA(v, secret []byte) (*ca.UICA, error) {
	var e CAEntry
	if err := json.Unmarshal(v, &e); err != nil {
		return nil, errors.New(errors.KindInternal, "control.UICA", "corrupt record: "+err.Error())
	}
	cert, err := ca.UnmarshalCert(e.CertPEM)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.UICA", "corrupt cert: "+err.Error())
	}
	priv, err := ca.UnsealUIKey(e.SealedKey, secret)
	if err != nil {
		return nil, errors.New(errors.KindInternal, "control.UICA", "unseal key: "+err.Error())
	}
	return &ca.UICA{Priv: priv, Cert: cert}, nil
}

// WebUITLS is the web UI listener's TLS config: an ECDSA certificate from the cluster UI CA,
// reissued in memory as it nears expiry, and the CA written to UICAFile for operators.
func WebUITLS(ctx context.Context, st store.Store, secret []byte, dataDir, nodeID, clusterID string) (*tls.Config, error) {
	uiCA, err := EnsureUICA(ctx, st, secret, clusterID)
	if err != nil {
		return nil, err
	}
	caPath := filepath.Join(dataDir, UICAFile)
	if err := os.MkdirAll(filepath.Dir(caPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(caPath, ca.MarshalCert(uiCA.Cert), 0o644); err != nil {
		return nil, err
	}
	src := &uiCertSource{ca: uiCA, nodeID: nodeID, path: filepath.Join(dataDir, UICertFile)}
	src.cur = src.load(time.Now())
	if _, err := src.get(time.Now()); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return src.get(time.Now()) },
	}, nil
}

type uiCertSource struct {
	ca     *ca.UICA
	nodeID string
	path   string
	mu     sync.Mutex
	cur    *tls.Certificate
}

func (s *uiCertSource) get(now time.Time) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil && s.cur.Leaf.NotAfter.Sub(now) > ca.UICertRenewLeft {
		return s.cur, nil
	}
	leaf, priv, err := s.ca.IssueServer(s.nodeID, hostnameOrLocal(), localIPs(), now)
	if err != nil {
		return nil, err
	}
	s.cur = &tls.Certificate{Certificate: [][]byte{leaf.Raw, s.ca.Cert.Raw}, PrivateKey: priv, Leaf: leaf}
	s.save(leaf, priv)
	return s.cur, nil
}

// load returns the saved certificate when the UI CA signed it, it is not due for renewal and it
// still names this node's hostname and addresses; otherwise nil.
func (s *uiCertSource) load(now time.Time) *tls.Certificate {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil
	}
	c, err := tls.X509KeyPair(raw, raw)
	if err != nil || c.Leaf == nil {
		return nil
	}
	roots := x509.NewCertPool()
	roots.AddCert(s.ca.Cert)
	if _, err := c.Leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now}); err != nil ||
		c.Leaf.Subject.CommonName != s.nodeID || c.Leaf.NotAfter.Sub(now) <= ca.UICertRenewLeft ||
		c.Leaf.VerifyHostname(hostnameOrLocal()) != nil {
		return nil
	}
	for _, ip := range localIPs() {
		if !slices.ContainsFunc(c.Leaf.IPAddresses, ip.Equal) {
			return nil
		}
	}
	c.Certificate = append(c.Certificate[:1], s.ca.Cert.Raw)
	return &c
}

// save writes the certificate and key for the next start; a failure only costs a new certificate then.
func (s *uiCertSource) save(leaf *x509.Certificate, priv *ecdsa.PrivateKey) {
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return
	}
	out := append(ca.MarshalCert(leaf), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, out, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// localIPs lists this host's non-loopback addresses, so the UI verifies when reached by IP.
func localIPs() []net.IP {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.IsLinkLocalUnicast() {
			ips = append(ips, ipn.IP)
		}
	}
	return ips
}
