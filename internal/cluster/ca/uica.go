package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"time"
)

// UI certificates are ECDSA P-256: browsers do not accept Ed25519 for TLS, so the web UI
// cannot use the cluster CA's chain; node↔node mTLS keeps it.
const (
	UICertValidity  = 90 * 24 * time.Hour
	UICertRenewLeft = 30 * 24 * time.Hour
)

// UICA is the cluster's web UI certificate authority.
type UICA struct {
	Priv *ecdsa.PrivateKey
	Cert *x509.Certificate
}

// GenerateUI creates a fresh self-signed ECDSA P-256 UI CA for clusterID.
func GenerateUI(clusterID string, now time.Time) (*UICA, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate UI CA key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "Expanse Web UI CA " + clusterID, Organization: []string{"Expanse"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, fmt.Errorf("create UI CA cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse UI CA cert: %w", err)
	}
	return &UICA{Priv: priv, Cert: cert}, nil
}

// IssueServer issues a UI server certificate valid for the node's names, the UI VIP name,
// localhost, the given IPs and 127.0.0.1.
func (u *UICA) IssueServer(nodeID, hostname string, ips []net.IP, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate UI key: %w", err)
	}
	names := []string{UIVIPHostname, nodeID, "localhost"}
	if hostname != "" && hostname != nodeID {
		names = append(names, hostname)
	}
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"Expanse"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(UICertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
		IPAddresses:  append([]net.IP{net.IPv4(127, 0, 0, 1)}, ips...),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, u.Cert, &priv.PublicKey, u.Priv)
	if err != nil {
		return nil, nil, fmt.Errorf("sign UI cert: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse UI cert: %w", err)
	}
	return cert, priv, nil
}

const pemTypeECKey = "EC PRIVATE KEY"

// SealUIKey encrypts the UI CA key with the cluster secret, as SealKey does for the cluster CA.
func SealUIKey(priv *ecdsa.PrivateKey, clusterSecret []byte) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal UI CA key: %w", err)
	}
	return SealBytes(pem.EncodeToMemory(&pem.Block{Type: pemTypeECKey, Bytes: der}), clusterSecret)
}

// UnsealUIKey decrypts a key sealed by SealUIKey.
func UnsealUIKey(data, clusterSecret []byte) (*ecdsa.PrivateKey, error) {
	pemKey, err := UnsealBytes(data, clusterSecret)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(pemKey)
	if block == nil || block.Type != pemTypeECKey {
		return nil, fmt.Errorf("ca: sealed UI CA key is not an EC key")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}
