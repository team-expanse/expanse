package agent

import (
	"context"
	"testing"

	"github.com/expanse/expanse/internal/cluster/join"
	"github.com/expanse/expanse/internal/store/boltstore"
)

// Node IDs can be UUIDs; the web UI names nodes by the hostname each publishes.
func TestPublishHostnameWritesOnlyChanges(t *testing.T) {
	st, err := boltstore.New(t.TempDir() + "/bolt.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := &Agent{store: st, cfg: Config{NodeID: "n1"}}
	ctx := context.Background()

	a.publishHostname(ctx, "rack1-a")
	first, err := st.Get(ctx, join.HostnameKey("n1"))
	if err != nil || string(first.Value) != "rack1-a" {
		t.Fatalf("hostname record = %v, %v; want rack1-a", first, err)
	}
	a.publishHostname(ctx, "rack1-a") // live pages wake on every write
	if again, _ := st.Get(ctx, join.HostnameKey("n1")); again.Revision != first.Revision {
		t.Errorf("rewrote an unchanged hostname")
	}
	a.publishHostname(ctx, "")
	if e, _ := st.Get(ctx, join.HostnameKey("n1")); string(e.Value) != "rack1-a" {
		t.Errorf("an empty hostname replaced the record: %q", e.Value)
	}
}
