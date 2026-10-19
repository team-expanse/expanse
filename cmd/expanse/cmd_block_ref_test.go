package main

import "testing"

// svr1: `block list` prints ns/name, so the other block commands take that form as well as -n.
func TestBlockRefTakesNamespaceSlashNameOrTheFlag(t *testing.T) {
	for _, tc := range []struct{ flag, arg, ns, name string }{
		{"default", "nginx", "default", "nginx"},
		{"web", "nginx", "web", "nginx"},
		{"default", "web/nginx", "web", "nginx"},
	} {
		if ns, name := blockRef(tc.flag, tc.arg); ns != tc.ns || name != tc.name {
			t.Errorf("blockRef(%q, %q) = %s, %s; want %s, %s", tc.flag, tc.arg, ns, name, tc.ns, tc.name)
		}
	}
}
