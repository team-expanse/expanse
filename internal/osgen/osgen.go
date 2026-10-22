// Package osgen describes a node's NixOS system generations for the cluster: which Expanse release
// each one runs, whether it is exactly the system an upgrade built for that release, and cleanup.
package osgen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/store"
)

// DefaultReleaseSystemsPath maps each system closure an upgrade built to its release.
const DefaultReleaseSystemsPath = "/persist/expanse/release-systems.json"

// Generation is one system generation on a node.
type Generation struct {
	Number  int       `json:"n"`
	Date    time.Time `json:"date"`
	Release string    `json:"release,omitempty"` // the Expanse version the system runs
	// Exact: an upgrade built this very system for Release.
	Exact bool `json:"exact,omitempty"`
	// Recorded: an upgrade built some system for Release here, so a non-exact one has drifted.
	Recorded bool `json:"recorded,omitempty"`
	Current  bool `json:"current,omitempty"`
	Booted   bool `json:"booted,omitempty"`
}

// Modified reports a generation that runs a release but is not the system its upgrade built.
func (g Generation) Modified() bool { return g.Recorded && !g.Exact }

// Report is what a node publishes about its generations.
type Report struct {
	Generations []Generation `json:"generations"` // newest first
	Clean       *CleanResult `json:"clean,omitempty"`
}

// CleanResult is the outcome of the node's last cleanup.
type CleanResult struct {
	At      time.Time `json:"at"`
	Keep    int       `json:"keep"`
	Removed []int     `json:"removed,omitempty"`
	Error   string    `json:"error,omitempty"`
}

// CleanRequest asks a node to delete all but its newest Keep generations.
type CleanRequest struct {
	Keep int `json:"keep"`
}

// DefaultKeep is the cleanup form's starting value.
const DefaultKeep = 3

// Prefix holds every node's Report and cleanup request. Not under /nodes/: anything JSON there
// reads as a node record.
const Prefix = "/os-generations/"

// ReportKey is where a node publishes its Report.
func ReportKey(node string) store.Key { return store.Key(Prefix + node) }

// RequestKey is where the web UI asks a node to clean up.
func RequestKey(node string) store.Key { return store.Key(Prefix + node + "/clean") }

// ToRemove lists the generations a cleanup keeping the newest keep (at least 1) deletes, newest
// first; the running and booted generations always stay.
func ToRemove(gens []Generation, keep int) []int {
	keep = max(keep, 1)
	var out []int
	for i, g := range gens {
		if i < keep || g.Current || g.Booted {
			continue
		}
		out = append(out, g.Number)
	}
	return out
}

// Drift names the running generation's release when it is not the system its upgrade built.
func Drift(gens []Generation) string {
	for _, g := range gens {
		if g.Current && g.Modified() {
			return g.Release + ", modified"
		}
	}
	return ""
}

// ReleaseOf is the Expanse version a system closure runs, from its sw/bin/expanse package name.
func ReleaseOf(system string) string {
	bin, err := filepath.EvalSymlinks(filepath.Join(system, "sw", "bin", "expanse"))
	if err != nil {
		return ""
	}
	_, pkg, _ := strings.Cut(filepath.Base(filepath.Dir(filepath.Dir(bin))), "-") // drop the store hash
	v, ok := strings.CutPrefix(pkg, "expanse-")
	if !ok {
		return ""
	}
	return v
}

// LoadReleaseSystems reads the system→release record; a missing or unreadable file is empty.
func LoadReleaseSystems(path string) map[string]string {
	out := map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

// RecordReleaseSystem notes that system is the one built for version.
func RecordReleaseSystem(path, system, version string) error {
	m := LoadReleaseSystems(path)
	m[system] = version
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
