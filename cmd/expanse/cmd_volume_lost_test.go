package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func TestForcedDeleteAsksTheControllerNotToWaitForDownNodes(t *testing.T) {
	fs := healthyStore(t)
	if err := runVolume(t, fs, "delete", "db", "--force"); err != nil {
		t.Fatal(err)
	}
	if got := string(fs.kv["/volumes/_ops/delete/vol-abc"]); !strings.Contains(got, `"force":true`) {
		t.Errorf("payload %s does not carry the force flag", got)
	}
}

func TestAPlainDeleteCarriesNoForceFlag(t *testing.T) {
	fs := healthyStore(t)
	if err := runVolume(t, fs, "delete", "db"); err != nil {
		t.Fatal(err)
	}
	if got := string(fs.kv["/volumes/_ops/delete/vol-abc"]); strings.Contains(got, "force") {
		t.Errorf("payload %s forces a delete nobody asked to force", got)
	}
}

func TestRetireQueuesOneRequestNamingTheNodeAndSaysItsDataIsGivenUp(t *testing.T) {
	fs := healthyStore(t)
	opts, stop := serveCLI(t, fs)
	defer stop()
	cmd := newVolumeCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"retire", "db", "--node", "n3"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/volumes/_ops/retire/vol-abc"}; !reflect.DeepEqual(fs.puts, want) {
		t.Errorf("writes %v, want %v", fs.puts, want)
	}
	got := string(fs.kv["/volumes/_ops/retire/vol-abc"])
	if !strings.Contains(got, `"node":"n3"`) || !strings.Contains(got, `"target":"db"`) {
		t.Errorf("payload %s", got)
	}
	if !strings.Contains(out.String(), "n3") {
		t.Errorf("output does not name the node whose replica is given up:\n%s", out.String())
	}
}

func TestRetireRefusesWhatItCannotDoAndQueuesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		why  string
	}{
		"no node":                {[]string{"retire", "db"}, "--node"},
		"a node with no replica": {[]string{"retire", "db", "--node", "n9"}, "n9 holds no replica"},
		"an unknown volume":      {[]string{"retire", "nope", "--node", "n3"}, "no such volume"},
	} {
		fs := healthyStore(t)
		err := runVolume(t, fs, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.why) {
			t.Errorf("%s: error %v, want one mentioning %q", name, err, tc.why)
		}
		if len(fs.puts) != 0 {
			t.Errorf("%s: queued %v", name, fs.puts)
		}
	}
}
