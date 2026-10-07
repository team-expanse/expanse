package main

import (
	"strings"
	"testing"
)

func TestNFSSetupDefaults(t *testing.T) {
	s, err := nfsSetupFrom("default-files-0", "/vol", "/pkg/lib/ganesha", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	want := nfsSetup{
		Port: "12049", Pseudo: "/share", SharePath: "/vol/share", RecoveryRoot: "/vol/.nfs-state/recovery",
		PluginsDir: "/pkg/lib/ganesha", Squash: "Root_Squash", Grace: 90, Lease: 60, Fsid: nfsFsid("default-files-0"),
	}
	if s != want {
		t.Errorf("setup = %+v\nwant    %+v", s, want)
	}
}

func TestNFSSetupFromConfig(t *testing.T) {
	s, err := nfsSetupFrom("i", "/vol", "/p", map[string]any{
		"port": float64(22049), "pseudo": "/media", "path": "pub", "readOnly": true,
		"squash": "none", "gracePeriod": float64(30),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "22049" || s.Pseudo != "/media" || s.SharePath != "/vol/pub" || !s.ReadOnly ||
		s.Squash != "No_Root_Squash" || s.Grace != 30 || s.Lease != 30 {
		t.Errorf("setup = %+v", s)
	}
}

func TestNFSSetupRejectsBadInput(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"unknown squash":     {"squash": "some"},
		"relative pseudo":    {"pseudo": "share"},
		"path escapes mount": {"path": "../etc"},
		"volume root":        {"path": "."}, // would export .nfs-state too
	} {
		if _, err := nfsSetupFrom("i", "/vol", "/p", cfg); err == nil {
			t.Errorf("%s: accepted %v", name, cfg)
		}
	}
}

func TestNFSFsidIsStablePerInstance(t *testing.T) {
	a, b := nfsFsid("default-files-0"), nfsFsid("default-other-0")
	if a != nfsFsid("default-files-0") || a == b {
		t.Errorf("fsid not a stable per-instance value: %q %q", a, b)
	}
	if strings.Count(a, ".") != 1 {
		t.Errorf("fsid %q is not major.minor", a)
	}
}

func TestGaneshaConfigExportsTheShareWithStateOnTheVolume(t *testing.T) {
	conf := ganeshaConfig(nfsSetup{
		Port: "12049", Pseudo: "/share", SharePath: "/vol/data", RecoveryRoot: "/vol/.nfs-state/recovery",
		PluginsDir: "/pkg/lib/ganesha", Squash: "Root_Squash", Grace: 90, Lease: 60, Fsid: "7.9",
	})
	for _, want := range []string{
		"NFS_Port = 12049;", "Protocols = 4;", "Plugins_Dir = \"/pkg/lib/ganesha\";", "Enable_Metrics = false;",
		"RecoveryBackend = fs;", "RecoveryRoot = \"/vol/.nfs-state/recovery\";",
		"Grace_Period = 90;", "Lease_Lifetime = 60;",
		"Path = \"/vol/data\";", "Pseudo = \"/share\";", "Access_Type = RW;", "Squash = Root_Squash;",
		"Filesystem_Id = 7.9;", "Name = VFS;",
		"Anonymous_Uid = 65534;", "Anonymous_Gid = 65534;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q:\n%s", want, conf)
		}
	}
	ro := ganeshaConfig(nfsSetup{ReadOnly: true, Squash: "Root_Squash"})
	if !strings.Contains(ro, "Access_Type = RO;") {
		t.Errorf("read-only export not RO:\n%s", ro)
	}
}

func TestGaneshaPluginsDirIsBesideTheBinary(t *testing.T) {
	if got := ganeshaPluginsDir("/nix/store/x-nfs-ganesha/bin/ganesha.nfsd"); got != "/nix/store/x-nfs-ganesha/lib/ganesha" {
		t.Errorf("plugins dir = %q", got)
	}
}
