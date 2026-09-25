// Package ca implements the cluster certificate authority: an Ed25519 root
// CA, node certificate issuance and renewal, mTLS configuration builders for
// all internal listeners, and a trust bundle that supports two simultaneously
// active CAs (for CA rotation, Phase 10 X2 — internal/cluster/control's
// renewal loop and castate.go drive it over this bundle).
//
// Certificate layout:
//   - CA:  Ed25519, self-signed, CN "Expanse Cluster CA <cluster-id>",
//     validity 10 years. Each node caches it at
//     /persist/expanse/ca/ca.pem, with the private key sealed alongside at
//     ca/ca.key.sealed (age-encrypted with a key derived from the cluster
//     secret; TPM-sealed is a deferred future feature, ARCHITECTURE.md —
//     no TPM hardware to test against). The authoritative copy — needed so
//     every node agrees during rotation, not just whichever CA a node's own
//     disk cache happens to hold — lives in the Raft store at
//     /cluster/ca/trust (internal/cluster/control's CATrust record).
//   - Node: Ed25519, CN = node ID, SANs = node ID, hostname, all node IPs,
//     validity 30 days, auto-renewed when 10 days remain (i.e. at 20 days of
//     a 30-day cert) by internal/cluster/control's renewal loop. A node
//     whose cert fully expires (offline > 30 days) must re-join with a
//     fresh token.
package ca

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"slices"
	"sort"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/hkdf"
)

// Validity periods.
const (
	CAValidity        = 10 * 365 * 24 * time.Hour
	NodeCertValidity  = 30 * 24 * time.Hour
	NodeCertRenewLeft = 10 * 24 * time.Hour // renew while ≥10d remain → renewed at 20 days of a 30-day cert
)

// CA is a cluster certificate authority.
type CA struct {
	Priv ed25519.PrivateKey
	Cert *x509.Certificate
}

// Generate creates a fresh self-signed Ed25519 CA for clusterID.
func Generate(clusterID string, now time.Time) (*CA, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject: pkix.Name{
			CommonName:   fmt.Sprintf("Expanse Cluster CA %s", clusterID),
			Organization: []string{"Expanse"},
		},
		NotBefore:             now.Add(-time.Hour), // tolerate small clock skew
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("create CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse CA cert: %w", err)
	}
	return &CA{Priv: priv, Cert: cert}, nil
}

// ParseCA restores a CA from a sealed private key and the stored certificate.
func ParseCA(priv ed25519.PrivateKey, certPEM []byte) (*CA, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("ca: invalid Ed25519 private key size")
	}
	cert, err := UnmarshalCert(certPEM)
	if err != nil {
		return nil, err
	}
	if cert.PublicKeyAlgorithm != x509.Ed25519 {
		return nil, errors.New("ca: certificate is not Ed25519")
	}
	if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey.(ed25519.PublicKey)) {
		return nil, errors.New("ca: private key does not match certificate")
	}
	return &CA{Priv: priv, Cert: cert}, nil
}

// IssueCSR signs a client-provided CSR. The joiner generates and keeps
// its own private key; only the public key travels (§4.5 JoinRequest
// carries the CSR). CN and SANs come from the CSR subject. The CSR
// signature is verified before signing; the resulting certificate is
// valid for NodeCertValidity with both server and client usage.
func (c *CA) IssueCSR(csr *x509.CertificateRequest, now time.Time) (*x509.Certificate, error) {
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: csr.Subject.CommonName, Organization: []string{"Expanse"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(NodeCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     csr.DNSNames,
		IPAddresses:  csr.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, csr.PublicKey, c.Priv)
	if err != nil {
		return nil, fmt.Errorf("sign CSR: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse node cert: %w", err)
	}
	return cert, nil
}

// UIVIPHostname is a fixed SAN present on every node certificate,
// regardless of which node issues it (Phase 2, D9: the UI VIP's TLS
// identity). A client reaching the web UI's VIP by this name gets a
// hostname match no matter which node currently holds the address --
// unlike nodeID, which only matches the node that happens to answer.
const UIVIPHostname = "expanse-ui"

// IssueNode issues a node certificate. CN = nodeID; SANs include nodeID,
// UIVIPHostname, hostname and every IP. The certificate is valid for
// NodeCertValidity and usable for both client and server authentication
// (all node↔node peers are both gRPC servers and clients).
func (c *CA) IssueNode(nodeID, hostname string, ips []net.IP, now time.Time) (*x509.Certificate, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate node key: %w", err)
	}
	sans := []string{nodeID, UIVIPHostname}
	if hostname != "" && hostname != nodeID && hostname != UIVIPHostname {
		sans = append(sans, hostname)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"Expanse"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(NodeCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     sans,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, pub, c.Priv)
	if err != nil {
		return nil, nil, fmt.Errorf("sign node cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse node cert: %w", err)
	}
	return cert, priv, nil
}

// NeedsRenewal reports whether a node certificate should be renewed now:
// true when less than NodeCertRenewLeft of validity remains (i.e. the cert
// has reached 20 days of its 30-day lifetime).
func NeedsRenewal(cert *x509.Certificate, now time.Time) bool {
	return now.After(cert.NotAfter.Add(-NodeCertRenewLeft))
}

// Expired reports whether cert is not valid at now.
func Expired(cert *x509.Certificate, now time.Time) bool {
	return now.Before(cert.NotBefore) || now.After(cert.NotAfter)
}

// randomSerial returns a random positive certificate serial number.
func randomSerial() *big.Int {
	for {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			panic(err) // crypto/rand failure is unrecoverable
		}
		n := new(big.Int).SetBytes(b)
		if n.Sign() > 0 {
			return n
		}
	}
}

// --- cert (un)marshaling ---

const pemTypeCert = "CERTIFICATE"

// MarshalCert PEM-encodes a certificate for storage (e.g. at /cluster/ca/cert).
func MarshalCert(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Raw})
}

// UnmarshalCert parses a PEM certificate produced by MarshalCert.
func UnmarshalCert(b []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(b)
	if block == nil || block.Type != pemTypeCert {
		return nil, errors.New("ca: no CERTIFICATE PEM block found")
	}
	return x509.ParseCertificate(block.Bytes)
}

// Fingerprint returns a stable, comparable identifier for a CA
// certificate: SHA-256 of its raw DER, hex encoded. Used during CA
// rotation to tell which CA issued a given node cert, and which CA a
// node has already renewed onto.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// KeyPEM PEM-encodes a private key (PKCS8).
func KeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM parses a PKCS8 private key.
func ParseKeyPEM(b []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("ca: no PRIVATE KEY PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("ca: private key is not Ed25519")
	}
	return priv, nil
}

// --- trust bundle (supports two active CAs for rotation) ---

// Bundle is the set of trusted cluster CAs. It normally holds exactly one
// CA; during CA rotation it holds two (the incoming and the outgoing) so
// that certs issued by either are accepted until the old CA is removed.
type Bundle struct {
	CAs []*x509.Certificate // first entry is the primary (issuing) CA
}

// NewBundle builds a bundle from one or more CA certificates.
func NewBundle(certs ...*x509.Certificate) (*Bundle, error) {
	if len(certs) == 0 {
		return nil, errors.New("ca: empty trust bundle")
	}
	for _, c := range certs {
		if !c.IsCA {
			return nil, errors.New("ca: bundle contains a non-CA certificate")
		}
	}
	return &Bundle{CAs: certs}, nil
}

// Primary returns the issuing CA.
func (b *Bundle) Primary() *x509.Certificate { return b.CAs[0] }

// Pool returns an x509 pool containing every CA in the bundle.
func (b *Bundle) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	for _, c := range b.CAs {
		pool.AddCert(c)
	}
	return pool
}

// VerifyNode verifies that a node certificate chains to one of the bundle's
// CAs and has the right key usages. The node ID (CN) must be checked
// separately by the caller against membership (see VerifyPeerCN).
func (b *Bundle) VerifyNode(cert *x509.Certificate, now time.Time) error {
	_, err := cert.Verify(x509.VerifyOptions{
		Roots:         b.Pool(),
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		Intermediates: x509.NewCertPool(),
	})
	if err != nil {
		return fmt.Errorf("ca: verify node cert: %w", err)
	}
	return nil
}

// VerifyPeerCN returns a tls.VerifyPeerCertificate callback that checks the
// leaf certificate's Common Name against the currently known node IDs.
// Membership changes take effect on the next handshake without restarting
// the listener. Chain and expiry verification are still performed by the
// standard verifier; this callback only adds the CN-membership check.
func VerifyPeerCN(knownNodes func() []string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("ca: no peer certificate presented")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("ca: parse peer cert: %w", err)
		}
		known := knownNodes()
		i := sort.SearchStrings(known, cert.Subject.CommonName)
		if i >= len(known) || known[i] != cert.Subject.CommonName {
			return fmt.Errorf("ca: peer CN %q is not a known node", cert.Subject.CommonName)
		}
		return nil
	}
}

// TLSConfig builds the tls.Config used by every internal listener (gRPC
// :7443, Raft :7444, join :7446). TLS 1.3 only, mutual TLS with the cluster
// CA(s) on both sides, plus the CN-membership check.
func TLSConfig(bundle *Bundle, ownCert tls.Certificate, knownNodes func() []string) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		ClientAuth:            tls.RequireAndVerifyClientCert,
		ClientCAs:             bundle.Pool(),
		RootCAs:               bundle.Pool(),
		Certificates:          []tls.Certificate{ownCert},
		VerifyPeerCertificate: VerifyPeerCN(knownNodes),
	}
}

// CertSource returns this node's current TLS identity. Callers re-invoke
// it on every handshake (via GetCertificate/GetClientCertificate) so a
// cert renewed on disk takes effect without restarting the listener.
type CertSource func() (*tls.Certificate, error)

// BundleSource returns the currently trusted CA set. Callers re-invoke
// it on every handshake so a CA rotation's bundle change (or its later
// retirement of the outgoing CA) takes effect without restarting the
// listener — mirrors VerifyPeerCN's existing "membership changes take
// effect on the next handshake" design, extended to trust itself.
type BundleSource func() (*Bundle, error)

// TLSConfigDynamic is TLSConfig, but the identity certificate and trust
// bundle are re-read on every handshake instead of fixed at config-build
// time (Phase 10 X2: CA rotation and cert renewal must not require
// restarting the internal :7443/:7446 listeners).
func TLSConfigDynamic(bundle BundleSource, cert CertSource, knownNodes func() []string) *tls.Config {
	base := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	base.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert() }
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		b, err := bundle()
		if err != nil {
			return nil, err
		}
		cfg := base.Clone()
		cfg.ClientCAs = b.Pool()
		cfg.RootCAs = b.Pool()
		cfg.VerifyPeerCertificate = VerifyPeerCN(knownNodes)
		return cfg, nil
	}
	return base
}

// PeerTLSConfigDynamic is PeerTLSConfig with the same live re-read of
// identity and trust bundle as TLSConfigDynamic (see its doc).
func PeerTLSConfigDynamic(bundle BundleSource, cert CertSource, knownNodes func() []string) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		GetClientCertificate:  func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return cert() },
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyPeerChainAndCNDynamic(bundle, knownNodes),
	}
}

// verifyPeerChainAndCNDynamic is VerifyPeerChainAndCN with a live bundle
// re-read instead of a fixed one.
func verifyPeerChainAndCNDynamic(bundle BundleSource, knownNodes func() []string) func([][]byte, [][]*x509.Certificate) error {
	cnCheck := VerifyPeerCN(knownNodes)
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("ca: no peer certificate presented")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("ca: parse peer cert: %w", err)
		}
		b, err := bundle()
		if err != nil {
			return fmt.Errorf("ca: load trust bundle: %w", err)
		}
		if err := b.VerifyNode(cert, time.Now()); err != nil {
			return err
		}
		return cnCheck(rawCerts, nil)
	}
}

// --- sealed CA key storage ---

const hkdfInfo = "expanse/ca-seal/v1"

// sealWorkFactor is age's scrypt work-factor exponent for CA-key
// sealing. The passphrase is HKDF-SHA256 of the 256-bit cluster secret
// — not a human password — so the KDF adds no real brute-force
// resistance; it only costs every daemon startup 128·8·2^logN bytes of
// scrypt state. age's default (2^18) peaks at 256 MiB, which a witness
// node (§4.9: RSS < 64 MiB) can never afford. 2^12 = 4 MiB.
// Unsealing stays compatible both ways (the factor is in the header).
const sealWorkFactor = 12

// deriveSealPassphrase derives the age passphrase from the cluster secret
// via HKDF-SHA256 (32 bytes, hex-encoded). The passphrase never leaves the
// sealing layer; rotate by rotating the cluster secret itself.
func deriveSealPassphrase(clusterSecret []byte) string {
	key := make([]byte, 32)
	h := hkdf.New(sha256.New, clusterSecret, nil, []byte(hkdfInfo))
	if _, err := io.ReadFull(h, key); err != nil {
		panic(err) // HKDF-SHA256 from 32+ byte secrets cannot fail short reads
	}
	return hex.EncodeToString(key)
}

// SealKey encrypts the CA private key for storage at
// /persist/expanse/secrets/ca.key. The key is age-encrypted with a
// passphrase derived (HKDF-SHA256) from the cluster secret.
func SealKey(priv ed25519.PrivateKey, clusterSecret []byte) ([]byte, error) {
	pemKey, err := KeyPEM(priv)
	if err != nil {
		return nil, err
	}
	recipient, err := age.NewScryptRecipient(deriveSealPassphrase(clusterSecret))
	if err != nil {
		return nil, fmt.Errorf("ca: scrypt recipient: %w", err)
	}
	recipient.SetWorkFactor(sealWorkFactor)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipient)
	if err != nil {
		return nil, fmt.Errorf("ca: seal key: %w", err)
	}
	if _, err := w.Write(pemKey); err != nil {
		return nil, fmt.Errorf("ca: seal key write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("ca: seal key close: %w", err)
	}
	return buf.Bytes(), nil
}

// UnsealKey decrypts a key sealed by SealKey.
func UnsealKey(data, clusterSecret []byte) (ed25519.PrivateKey, error) {
	identity, err := age.NewScryptIdentity(deriveSealPassphrase(clusterSecret))
	if err != nil {
		return nil, fmt.Errorf("ca: scrypt identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), identity)
	if err != nil {
		return nil, fmt.Errorf("ca: unseal key (wrong cluster secret?): %w", err)
	}
	pemKey, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("ca: unseal key read: %w", err)
	}
	return ParseKeyPEM(pemKey)
}

// SortedIDs is a helper for callers maintaining a known-node list for
// VerifyPeerCN: the list must be sorted for binary search.
func SortedIDs(ids []string) []string {
	out := slices.Clone(ids)
	sort.Strings(out)
	return out
}

// PeerTLSConfig is the CLIENT-side config for node↔node connections
// (§4.4/G3.8): TLS 1.3, mutual TLS with the node's own identity, chain
// verification against the cluster CA plus the CN-membership check —
// WITHOUT IP/hostname matching. Node certs carry no IP SANs; peers are
// authenticated by certificate CN (the node ID) and reached at whatever
// address the cluster view provides.
func PeerTLSConfig(bundle *Bundle, ownCert tls.Certificate, knownNodes func() []string) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{ownCert},
		// Chain + CN verification runs in the callback; Go's built-in
		// IP/hostname matching is disabled for the reason above.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: VerifyPeerChainAndCN(bundle, knownNodes),
	}
}

// VerifyPeerChainAndCN verifies the peer's leaf against the cluster CA
// bundle and the CN-membership check (used when IP/hostname matching is
// not applicable, see PeerTLSConfig).
func VerifyPeerChainAndCN(bundle *Bundle, knownNodes func() []string) func([][]byte, [][]*x509.Certificate) error {
	cnCheck := VerifyPeerCN(knownNodes)
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("ca: no peer certificate presented")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return fmt.Errorf("ca: parse peer cert: %w", err)
		}
		if err := bundle.VerifyNode(cert, time.Now()); err != nil {
			return err
		}
		return cnCheck(rawCerts, nil)
	}
}
