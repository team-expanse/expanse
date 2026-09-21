package controller

import (
	"context"
	"strings"

	"github.com/expanse/expanse/internal/store"
)

const opsPrefix = "/volumes/_ops/"

// sweepStaleOps drops the requests of volumes that are gone. Nothing else consumes
// them: the node that would act on one no longer holds the volume. byName maps a
// volume's name to its id, since a request may be keyed by either.
func (c *Controller) sweepStaleOps(ctx context.Context, byName map[string]string, ids []string) {
	entries, err := c.opts.St.List(ctx, store.Key(opsPrefix))
	if err != nil {
		return
	}
	for _, e := range entries {
		// <kind>/<volume>[/<node>]
		parts := strings.SplitN(strings.TrimPrefix(string(e.Key), opsPrefix), "/", 3)
		if len(parts) < 2 || c.volumeExists(parts[1], byName, ids) {
			continue
		}
		_ = c.opts.St.Delete(ctx, e.Key, 0)
	}
}

func (c *Controller) volumeExists(ref string, byName map[string]string, ids []string) bool {
	if _, ok := byName[ref]; ok {
		return true
	}
	for _, id := range ids {
		if id == ref {
			return true
		}
	}
	return false
}
