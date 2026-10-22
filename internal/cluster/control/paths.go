// Package control implements the `expanse cluster` control plane that
// wraps the Phase 03 building blocks: init (§4.5 bootstrap steps 1–8),
// enrollment (join), status reporting (§5), token management, and
// leave. The CLI in cmd/expanse is a thin wrapper; everything here is
// importable so tests can drive a full init → token → join → status
// flow in-process.
package control

import (
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
)

// On-disk layout under the data dir (default /persist/expanse):
//
//	cluster-id          cluster UUID (plain text) — its presence marks a cluster node
//	cluster-secret      32-byte cluster secret, hex (0600)
//	ca/ca.pem           cluster CA certificate (PEM)
//	ca/ca.key.sealed    CA private key, age-sealed to the cluster secret
//	raft/               raftstore data (BoltDB log + snapshots)
//	tls/node-cert.pem   this node's certificate (PEM)
//	tls/node-key.pem    this node's private key (0600)
const (
	ClusterIDFile    = "cluster-id"
	NodeIDFile       = "node-id"
	SecretFile       = "cluster-secret"
	CADir            = "ca"
	CAFile           = "ca/ca.pem"
	CAKeyFile        = "ca/ca.key.sealed"
	RaftDir          = "raft"
	TLSDir           = "tls"
	NodeCertFile     = "tls/node-cert.pem"
	NodeKeyFile      = "tls/node-key.pem"
	MetaKey          = "/cluster/meta"
	GenerationKey    = "/cluster/generation"
	GenerationOne    = "1"
	DefaultNodeTTL64 = int64(0)
)

// IsClusterNode reports whether the data dir contains a cluster
// enrollment (cluster-id present). The daemon checks this at startup to
// pick raftstore over the single-node boltstore.
func IsClusterNode(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, ClusterIDFile))
	return err == nil
}

// LoadCluster reads the persisted enrollment: cluster ID, secret, CA.
func LoadCluster(dataDir string) (clusterID string, secret []byte, clusterCA *ca.CA, err error) {
	idb, err := os.ReadFile(filepath.Join(dataDir, ClusterIDFile))
	if err != nil {
		return "", nil, nil, errors.New(errors.KindNotFound, "control.LoadCluster", "not a cluster node: "+err.Error())
	}
	sb, err := os.ReadFile(filepath.Join(dataDir, SecretFile))
	if err != nil {
		return "", nil, nil, errors.New(errors.KindNotFound, "control.LoadCluster", "cluster secret missing: "+err.Error())
	}
	secret, err = hex.DecodeString(trimSpace(sb))
	if err != nil || len(secret) != 32 {
		return "", nil, nil, errors.New(errors.KindInternal, "control.LoadCluster", "cluster secret corrupt")
	}
	certPEM, err := os.ReadFile(filepath.Join(dataDir, CAFile))
	if err != nil {
		return "", nil, nil, errors.New(errors.KindNotFound, "control.LoadCluster", "CA cert missing: "+err.Error())
	}
	cert, err := ca.UnmarshalCert(certPEM)
	if err != nil {
		return "", nil, nil, errors.New(errors.KindInternal, "control.LoadCluster", "CA cert corrupt: "+err.Error())
	}
	sealed, err := os.ReadFile(filepath.Join(dataDir, CAKeyFile))
	if err != nil {
		return "", nil, nil, errors.New(errors.KindNotFound, "control.LoadCluster", "CA key missing: "+err.Error())
	}
	priv, err := ca.UnsealKey(sealed, secret)
	if err != nil {
		return "", nil, nil, errors.New(errors.KindInternal, "control.LoadCluster", "CA key unseal failed: "+err.Error())
	}
	return trimSpace(idb), secret, &ca.CA{Priv: priv, Cert: cert}, nil
}

// writeFileSync writes atomically: temp file + fsync + rename + parent
// dir fsync. Enrollment material (cluster secret, sealed CA key, TLS)
// must survive a hard power loss (§4.8 node restart): a torn write to
// any of these files bricks the node's re-enrollment.
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".expanse-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// trimSpace strips surrounding whitespace from file contents.
func trimSpace(b []byte) string { return strings.TrimSpace(string(b)) }

// saveCA persists the CA cert and the key sealed to the cluster secret.
func saveCA(dataDir string, clusterCA *ca.CA, secret []byte) error {
	dir := filepath.Join(dataDir, CADir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dataDir, CAFile), ca.MarshalCert(clusterCA.Cert), 0o644); err != nil {
		return err
	}
	sealed, err := ca.SealKey(clusterCA.Priv, secret)
	if err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, CAKeyFile), sealed, 0o600)
}

// saveSecret persists the hex cluster secret (0600).
func saveSecret(dataDir string, secret []byte) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, SecretFile), []byte(hex.EncodeToString(secret)), 0o600)
}

// saveNodeTLS persists the joiner's cert + key.
func saveNodeTLS(dataDir string, certPEM, keyPEM []byte) error {
	dir := filepath.Join(dataDir, TLSDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dataDir, NodeCertFile), certPEM, 0o644); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, NodeKeyFile), keyPEM, 0o600)
}

// SaveNodeID persists the enrolled node ID (used by CLI reopen paths).
func SaveNodeID(dataDir, nodeID string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, NodeIDFile), []byte(nodeID), 0o644)
}

// LoadNodeID reads the persisted node ID ("" if not enrolled).
func LoadNodeID(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, NodeIDFile))
	if err != nil {
		return ""
	}
	return trimSpace(b)
}

func saveClusterID(dataDir, id string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return writeFileSync(filepath.Join(dataDir, ClusterIDFile), []byte(id), 0o644)
}

// localIP returns a routable non-loopback IPv4 for advertise defaults,
// falling back to 127.0.0.1 (single-host testing).
// LocalIP is exported for CLI advertise defaults.
func LocalIP() string { return localIP() }

func localIP() string {
	addrs, _ := net.InterfaceAddrs()
	return pickIP(defaultRouteSource(), addrs)
}

// defaultRouteSource is the address the kernel sends from on the default route, nil without one.
// A UDP "dial" only picks the route; nothing is sent.
func defaultRouteSource() net.IP {
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return nil
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP
}

// pickIP prefers the default route's source, else the first interface IPv4; never a link-local
// one (a BMC's USB NIC, 169.254/16), which other nodes cannot reach.
func pickIP(route net.IP, addrs []net.Addr) string {
	usable := func(ip net.IP) bool { return ip.To4() != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() }
	if usable(route) {
		return route.To4().String()
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && usable(ipn.IP) {
			return ipn.IP.To4().String()
		}
	}
	return "127.0.0.1"
}

// RepairAdvertise swaps a link-local advertise host, which releases before 1.16 could pick from
// a BMC's USB NIC, for LocalIP, keeping the port.
func RepairAdvertise(adv string) string { return repairAdvertise(adv, LocalIP()) }

func repairAdvertise(adv, local string) string {
	host, port, err := net.SplitHostPort(adv)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLinkLocalUnicast() {
		return adv
	}
	if ip := net.ParseIP(local); ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return adv
	}
	return net.JoinHostPort(local, port)
}

// RaftAddrFile records this node's raft bind/advertise addresses
// (host:port each, advertise optional) so later CLI invocations rebind
// on the same ports (multi-node clusters on one host).
const RaftAddrFile = "raft-addr"

// SaveRaftAddr persists the node's raft bind (and advertise, if
// different) addresses under the data dir.
func SaveRaftAddr(dataDir, bind, advertise string) error {
	if bind == "" {
		return nil
	}
	content := bind
	if advertise != "" && advertise != bind {
		content += "\n" + advertise
	}
	return os.WriteFile(filepath.Join(dataDir, RaftAddrFile), []byte(content), 0o600)
}

// LoadRaftAddr returns the persisted (bind, advertise) raft addresses;
// empty strings when unrecorded (defaults apply).
func LoadRaftAddr(dataDir string) (bind, advertise string) {
	b, err := os.ReadFile(filepath.Join(dataDir, RaftAddrFile))
	if err != nil {
		return "", ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 0 {
		bind = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		advertise = strings.TrimSpace(lines[1])
	}
	return bind, advertise
}
