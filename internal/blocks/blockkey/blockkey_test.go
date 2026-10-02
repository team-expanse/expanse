package blockkey

import (
	"testing"

	"github.com/expanse/expanse/internal/store"
)

func TestIsSpec(t *testing.T) {
	for k, want := range map[store.Key]bool{
		"/blocks/default/web":                      true,
		"/blocks/default/web/status":               false,
		"/blocks/default/web/status/replicas/0":    false,
		"/blocks/default":                          false,
		"/blocks/":                                 false,
		"/blocks/default/":                         false,
		"/nodes/n1":                                false,
		"/blocks/default/web/anything-added-later": false,
	} {
		if got := IsSpec(k); got != want {
			t.Errorf("IsSpec(%q) = %t, want %t", k, got, want)
		}
	}
}
