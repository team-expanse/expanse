package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/lvm"
	"github.com/expanse/expanse/internal/store"
)

// Turning storage on per node: the web UI writes an EnableRequest, the node's
// agent builds the thin pool, starts its volume stack and reports a NodeStorage.
const (
	EnablePrefix      = "/settings/storage/"
	NodeStoragePrefix = "/storage-nodes/"

	DefaultVG      = "expanse" // the installer's data VG
	DefaultPool    = "pool"
	DefaultPercent = 95 // of the VG's free space; the rest is headroom (STORAGE.md §3)
	MaxPercent     = 95
)

// EnableRequest asks one node to hold volume replicas in a thin pool it creates.
type EnableRequest struct {
	VG      string `json:"vg"`
	Pool    string `json:"pool"`
	Percent int    `json:"percent"` // of the VG's free space, for a pool that does not exist yet
}

// WithDefaults fills the fields left empty.
func (r EnableRequest) WithDefaults() EnableRequest {
	if r.VG == "" {
		r.VG = DefaultVG
	}
	if r.Pool == "" {
		r.Pool = DefaultPool
	}
	if r.Percent == 0 {
		r.Percent = DefaultPercent
	}
	return r
}

// Validate checks the names LVM would see and the pool size.
func (r EnableRequest) Validate() error {
	const op = "storage.EnableRequest"
	if !lvm.ValidName(r.VG) || !lvm.ValidName(r.Pool) {
		return experrors.New(experrors.KindInvalid, op, fmt.Sprintf("%q/%q is not a valid LVM name", r.VG, r.Pool))
	}
	if r.Percent < 1 || r.Percent > MaxPercent {
		return experrors.New(experrors.KindInvalid, op, fmt.Sprintf("the pool takes 1 to %d%% of the free space", MaxPercent))
	}
	return nil
}

// EnableKey is where a node's EnableRequest lives.
func EnableKey(node string) store.Key { return store.Key(EnablePrefix + node) }

// LoadEnableRequest reads a node's request; ok is false when there is none.
func LoadEnableRequest(ctx context.Context, st store.Store, node string) (EnableRequest, bool, error) {
	var r EnableRequest
	ok, err := loadJSON(ctx, st, EnableKey(node), &r)
	return r, ok, err
}

// SaveEnableRequest writes a node's request.
func SaveEnableRequest(ctx context.Context, st store.Store, node string, r EnableRequest) error {
	return saveJSON(ctx, st, EnableKey(node), r)
}

// NodeStorageState is where a node's volume stack stands.
type NodeStorageState string

const (
	NodeStorageOff    NodeStorageState = "off"    // not asked for
	NodeStorageReady  NodeStorageState = "ready"  // holds volume replicas
	NodeStorageReboot NodeStorageState = "reboot" // DRBD 8.4 is loaded; DRBD 9 needs a reboot
	NodeStorageFailed NodeStorageState = "failed" // asked for, but the pool could not be built
)

// Where a node's storage settings come from.
const (
	SourceUI     = "ui"
	SourceConfig = "config" // the node's --storage-vg, which the UI cannot change
)

// NodeStorage is what a node reports about its storage. Sizes are bytes.
type NodeStorage struct {
	State    NodeStorageState `json:"state"`
	Source   string           `json:"source,omitempty"`
	VG       string           `json:"vg"`
	Pool     string           `json:"pool,omitempty"`
	VGSize   uint64           `json:"vgSize"`
	VGFree   uint64           `json:"vgFree"`
	PoolSize uint64           `json:"poolSize,omitempty"`
	Message  string           `json:"message,omitempty"`
}

// NodeStorageKey is where a node reports its NodeStorage.
func NodeStorageKey(node string) store.Key { return store.Key(NodeStoragePrefix + node) }

// SaveNodeStorage writes a node's report.
func SaveNodeStorage(ctx context.Context, st store.Store, node string, s NodeStorage) error {
	return saveJSON(ctx, st, NodeStorageKey(node), s)
}

// ListNodeStorage maps each reporting node to its NodeStorage, skipping unreadable records.
func ListNodeStorage(ctx context.Context, st store.Store) (map[string]NodeStorage, error) {
	ents, err := st.List(ctx, store.Key(NodeStoragePrefix))
	if err != nil {
		return nil, err
	}
	out := make(map[string]NodeStorage, len(ents))
	for _, e := range ents {
		var s NodeStorage
		if json.Unmarshal(e.Value, &s) == nil {
			out[strings.TrimPrefix(string(e.Key), NodeStoragePrefix)] = s
		}
	}
	return out, nil
}

func loadJSON(ctx context.Context, st store.Store, k store.Key, v any) (bool, error) {
	e, err := st.Get(ctx, k)
	if experrors.KindOf(err) == experrors.KindNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(e.Value, v); err != nil {
		return false, experrors.Wrap(err, experrors.KindInternal, "storage", "corrupt record "+string(k))
	}
	return true, nil
}

func saveJSON(ctx context.Context, st store.Store, k store.Key, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, k, raw)
	return err
}
