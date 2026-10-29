// Package livespec says which replica spec changes a block's runtime applies live, on a reload,
// rather than through a restart. The agent and expanse-block-run share it.
package livespec

import (
	"encoding/json"
	"path/filepath"
	"slices"
)

// liveFlags are the args each type's runtime applies on a reload; each takes one value.
var liveFlags = map[string][]string{"vm/instance": {"--cpu", "--mem"}}

// LiveOnly reports whether next differs from cur, and only in typ's live flags.
func LiveOnly(typ string, cur, next []string) bool {
	flags := liveFlags[typ]
	if len(flags) == 0 {
		return false
	}
	a, b := CanonicalArgs(cur), CanonicalArgs(next)
	return !slices.Equal(a, b) && slices.Equal(without(a, flags), without(b, flags))
}

// without drops each flag in flags, and its value, from args.
func without(args, flags []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if slices.Contains(flags, args[i]) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

// CanonicalArgs returns args with the --config JSON re-encoded by encoding/json (sorted keys, no
// spacing), so agent builds that encode it differently compare equal.
func CanonicalArgs(args []string) []string {
	i := slices.Index(args, "--config")
	if i < 0 || i+1 >= len(args) {
		return args
	}
	var v any
	if err := json.Unmarshal([]byte(args[i+1]), &v); err != nil {
		return args
	}
	out, err := json.Marshal(v)
	if err != nil {
		return args
	}
	args = slices.Clone(args)
	args[i+1] = string(out)
	return args
}

// DefaultDir holds a marker per replica whose runtime serves reloads.
const DefaultDir = "/run/expanse/block-live"

// MarkerPath is the marker a runtime writes once it handles SIGHUP; without it the agent restarts.
func MarkerPath(dir, instance string) string {
	if dir == "" {
		dir = DefaultDir
	}
	return filepath.Join(dir, instance)
}
