// Package blockkey tells a block's spec key apart from the keys stored beneath it.
package blockkey

import (
	"strings"

	"github.com/expanse/expanse/internal/store"
)

// IsSpec reports whether k is a block spec key, /blocks/<ns>/<name>, and not its status, a
// replica's probe record, or anything else under it.
func IsSpec(k store.Key) bool {
	rest, ok := strings.CutPrefix(string(k), "/blocks/")
	ns, name, found := strings.Cut(rest, "/")
	return ok && found && ns != "" && name != "" && !strings.Contains(name, "/")
}
