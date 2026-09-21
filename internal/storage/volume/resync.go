package volume

import (
	"context"

	"github.com/expanse/expanse/internal/store"
)

// opResync asks one node to rebuild its replica from its peers, one request per
// node under /volumes/_ops/resync/<volID>/<node>.
const opResync = "resync"

func resyncKey(id, node string) store.Key { return opKey(opResync, id+"/"+node) }

// runResync carries out this node's request, if any, on any pass: a request left
// queued would otherwise fire later, when nobody expects a replica to be discarded.
func (n *Node) runResync(ctx context.Context, d Desired) error {
	return n.runBareOp(ctx, resyncKey(d.Name, n.Self), opResync, d.Name, func() error { return n.RT.Resync(ctx, d.Name) })
}
