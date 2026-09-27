package control

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
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
	src := &uiCertSource{ca: uiCA, nodeID: nodeID}
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
	return s.cur, nil
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
