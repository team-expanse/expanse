package livespec

import "testing"

func TestOnlyAVMsSizeIsLive(t *testing.T) {
	base := []string{"--config", `{"a":1,"b":2}`, "--cpu", "1", "--mem", "1Gi", "--voldev", "disk=v1"}
	cases := []struct {
		name string
		typ  string
		next []string
		want bool
	}{
		{"cpu and mem", "vm/instance", []string{"--config", `{"a":1,"b":2}`, "--cpu", "2", "--mem", "2Gi", "--voldev", "disk=v1"}, true},
		{"config spacing", "vm/instance", []string{"--config", `{"b": 2, "a": 1}`, "--cpu", "2", "--mem", "1Gi", "--voldev", "disk=v1"}, true},
		{"a size added", "vm/instance", []string{"--config", `{"a":1,"b":2}`, "--cpu", "1", "--voldev", "disk=v1"}, true},
		{"config", "vm/instance", []string{"--config", `{"a":1,"b":3}`, "--cpu", "2", "--mem", "1Gi", "--voldev", "disk=v1"}, false},
		{"volume", "vm/instance", []string{"--config", `{"a":1,"b":2}`, "--cpu", "2", "--mem", "1Gi", "--voldev", "disk=v2"}, false},
		{"another type", "util/echo", []string{"--config", `{"a":1,"b":2}`, "--cpu", "2", "--mem", "1Gi", "--voldev", "disk=v1"}, false},
		{"no change", "vm/instance", base, false},
	}
	for _, c := range cases {
		if got := LiveOnly(c.typ, base, c.next); got != c.want {
			t.Errorf("%s: LiveOnly = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanonicalArgsReencodesTheConfig(t *testing.T) {
	got := CanonicalArgs([]string{"--x", "--config", `{"b": 2, "a": 1}`})
	if got[2] != `{"a":1,"b":2}` {
		t.Errorf("config = %s", got[2])
	}
	if bad := CanonicalArgs([]string{"--config", "{"}); bad[1] != "{" {
		t.Errorf("unparsable config changed: %s", bad[1])
	}
}
