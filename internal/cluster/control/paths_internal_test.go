package control

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/cluster/ca"
	"github.com/expanse/expanse/internal/errors"
)

// testInitOpts builds InitOptions for a loopback in-proc bootstrap.
func testInitOpts(dataDir, name string) InitOptions {
	bind := "127.0.0.1:0"
	return InitOptions{
		DataDir: dataDir, NodeID: "n1", Name: name,
		AdvertiseAddr: bind, BindAddr: bind, JoinHost: "127.0.0.1",
	}
}

// execCopyDir copies a data dir via cp -a (the dir contains a live
// bolt store; plain file copy is fine between closes).
func execCopyDir(src, dst string) (string, error) {
	out, err := exec.Command("cp", "-a", src+"/.", dst).CombinedOutput()
	return string(out), err
}

// TestNodeIDRoundTrip covers SaveNodeID/LoadNodeID and the missing-dir
// case.
func TestNodeIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveNodeID(dir, "n42"); err != nil {
		t.Fatalf("SaveNodeID: %v", err)
	}
	if got := LoadNodeID(dir); got != "n42" {
		t.Errorf("LoadNodeID = %q, want n42", got)
	}
	if got := LoadNodeID(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("LoadNodeID(missing dir) = %q, want empty", got)
	}
}

// TestLocalIP returns a parseable, non-loopback-favouring IP.
func TestLocalIP(t *testing.T) {
	ip := LocalIP()
	if ip == "" {
		t.Fatal("LocalIP empty")
	}
	if net.ParseIP(ip) == nil {
		t.Errorf("LocalIP = %q not an IP", ip)
	}
}

// TestLoadClusterCorruption walks every error branch: missing files,
// corrupt secret, corrupt CA PEM, unseal failure.
func TestLoadClusterCorruption(t *testing.T) {
	// Healthy enrollment to corrupt from.
	healthy := t.TempDir()
	res, err := Init(context.Background(), testInitOpts(healthy, "corrupt-target"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = res.Store.Close() }()

	rewrite := func(dir, rel string, content []byte) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Missing cluster-id → not a cluster node.
	empty := t.TempDir()
	if _, _, _, err := LoadCluster(empty); !errors.Is(err, errors.KindNotFound) {
		t.Errorf("LoadCluster(empty) = %v, want not-found", err)
	}

	// Corrupt branches, each against a fresh copy of the healthy dir.
	if _, _, _, err := LoadCluster(healthy); err != nil {
		t.Fatalf("healthy LoadCluster failed: %v", err)
	}

	corrupt := func(name string, mutate func(dir string)) string {
		t.Helper()
		dir := t.TempDir()
		if out, err := execCopyDir(healthy, dir); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		mutate(dir)
		_, _, _, err := LoadCluster(dir)
		if err == nil {
			t.Fatalf("%s: LoadCluster accepted corrupt state", name)
		}
		return err.Error()
	}

	msg := corrupt("bad-hex", func(dir string) { rewrite(dir, SecretFile, []byte("zz-not-hex")) })
	if !strings.Contains(msg, "corrupt") {
		t.Errorf("bad-hex secret: %v", msg)
	}
	msg = corrupt("short-secret", func(dir string) { rewrite(dir, SecretFile, []byte("aabb")) }) // valid hex, wrong length
	if !strings.Contains(msg, "corrupt") {
		t.Errorf("short secret: %v", msg)
	}
	msg = corrupt("ca-pem", func(dir string) { rewrite(dir, CAFile, []byte("not a pem")) })
	if !strings.Contains(msg, "CA cert corrupt") {
		t.Errorf("corrupt CA: %v", msg)
	}
	msg = corrupt("sealed-key", func(dir string) { rewrite(dir, CAKeyFile, []byte("not-a-sealed-key")) })
	if !strings.Contains(msg, "unseal") {
		t.Errorf("corrupt sealed key: %v", msg)
	}

	// Missing secret / CA / sealed key → not-found kinds.
	for _, rel := range []string{SecretFile, CAFile, CAKeyFile} {
		dir := t.TempDir()
		if out, err := execCopyDir(healthy, dir); err != nil {
			t.Fatalf("copy: %v %s", err, out)
		}
		if err := os.Remove(filepath.Join(dir, rel)); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := LoadCluster(dir); !errors.Is(err, errors.KindNotFound) {
			t.Errorf("LoadCluster without %s = %v, want not-found", rel, err)
		}
	}
}

// TestSaveLoadRaftAddrRoundTrip covers the raft-addr persistence.
func TestSaveLoadRaftAddrRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveRaftAddr(dir, "127.0.0.1:7001", "10.0.0.1:7001"); err != nil {
		t.Fatalf("SaveRaftAddr: %v", err)
	}
	bind, adv := LoadRaftAddr(dir)
	if bind != "127.0.0.1:7001" || adv != "10.0.0.1:7001" {
		t.Errorf("LoadRaftAddr = (%q, %q)", bind, adv)
	}
	// Missing file → empty strings, no panic.
	bind, adv = LoadRaftAddr(filepath.Join(dir, "nope"))
	if bind != "" || adv != "" {
		t.Errorf("LoadRaftAddr(missing) = (%q, %q)", bind, adv)
	}
}

// TestUnsealKeyWrongSecret verifies the sealed CA key refuses a wrong
// secret (the "cluster secret leaked + sealed key copied" attack).
func TestUnsealKeyWrongSecret(t *testing.T) {
	dir := t.TempDir()
	res, err := Init(context.Background(), testInitOpts(dir, "unseal-check"))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() { _ = res.Store.Close() }()

	sealed, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	for i := range wrong {
		wrong[i] = byte(i)
	}
	if _, err := ca.UnsealKey(sealed, wrong); err == nil {
		t.Error("UnsealKey accepted a wrong secret")
	}
}

func TestPickIPPrefersTheDefaultRouteSource(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("169.254.0.2"), Mask: net.CIDRMask(24, 32)}, // a BMC's USB NIC
		&net.IPNet{IP: net.ParseIP("192.168.10.154"), Mask: net.CIDRMask(24, 32)},
	}
	if got := pickIP(net.ParseIP("192.168.10.154"), addrs); got != "192.168.10.154" {
		t.Errorf("with a default route: %s", got)
	}
	if got := pickIP(nil, addrs); got != "192.168.10.154" {
		t.Errorf("without a default route, a link-local address won: %s", got)
	}
	if got := pickIP(net.ParseIP("169.254.0.2"), addrs[:2]); got != "127.0.0.1" {
		t.Errorf("only link-local: %s", got)
	}
}

func TestRepairAdvertiseReplacesOnlyALinkLocalHost(t *testing.T) {
	for _, c := range []struct{ adv, local, want string }{
		{"169.254.0.2:7444", "192.168.10.154", "192.168.10.154:7444"},
		{"192.168.10.154:7444", "10.0.0.5", "192.168.10.154:7444"}, // a chosen address stays
		{"169.254.0.2:7444", "127.0.0.1", "169.254.0.2:7444"},      // nothing better to move to
		{"169.254.0.2:7444", "169.254.9.9", "169.254.0.2:7444"},
		{"not-an-address", "192.168.10.154", "not-an-address"},
	} {
		if got := repairAdvertise(c.adv, c.local); got != c.want {
			t.Errorf("repairAdvertise(%q, %q) = %q, want %q", c.adv, c.local, got, c.want)
		}
	}
}
