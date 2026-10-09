package main

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestJellyfinSetupKeepsStateOnTheVolumeAndCacheOff(t *testing.T) {
	s, err := jellyfinSetupFrom("/vol", "/tmp/jf", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if s.DataDir != "/vol/jellyfin/data" || s.ConfigDir != "/vol/jellyfin/config" ||
		s.CacheDir != "/tmp/jf/cache" || s.LogDir != "/tmp/jf/log" || s.PublishedURL != "" {
		t.Errorf("setup = %+v", s)
	}
}

func TestJellyfinSetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"publishedServerUrl": "media.example.com"},
		{"publishedServerUrl": "https://media.example.com/a b"},
	} {
		if _, err := jellyfinSetupFrom("/vol", "/tmp/jf", cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestJellyfinArgs(t *testing.T) {
	s, err := jellyfinSetupFrom("/vol", "/tmp/jf", map[string]any{"publishedServerUrl": "https://media.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"--datadir", "/vol/jellyfin/data", "--configdir", "/vol/jellyfin/config",
		"--cachedir", "/tmp/jf/cache", "--logdir", "/tmp/jf/log",
		// The unit's sandbox has no AF_NETLINK, which watching for network changes needs.
		"--nonetchange",
		"--published-server-url", "https://media.example.com",
	}
	if got := jellyfinArgs(s); !slices.Equal(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

func TestJellyfinDatabaseConfigSyncsEveryCommit(t *testing.T) {
	var db struct {
		DatabaseType string
		Options      []struct{ Key, Value string } `xml:"CustomProviderOptions>Options>CustomDatabaseOption"`
	}
	if err := xml.Unmarshal([]byte(jellyfinDatabaseXML), &db); err != nil {
		t.Fatal(err)
	}
	// A failover is a crash: Jellyfin's default synchronous=NORMAL (1) loses committed WAL frames.
	if db.DatabaseType != "Jellyfin-SQLite" || len(db.Options) != 1 ||
		db.Options[0].Key != "syncmode" || db.Options[0].Value != "2" {
		t.Errorf("database.xml = %+v", db)
	}
}

func TestWriteJellyfinDatabaseConfigReplacesAnEarlierOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "database.xml")
	if err := os.WriteFile(path, []byte("<old/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJellyfinDatabaseConfig(dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != jellyfinDatabaseXML {
		t.Errorf("database.xml = %q", got)
	}
}
