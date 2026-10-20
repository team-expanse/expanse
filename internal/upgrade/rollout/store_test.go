package rollout

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/expanse/expanse/internal/store/boltstore"
)

func TestPlanAndReportsRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, err := boltstore.New(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if p, err := LoadPlan(ctx, st); err != nil || p != nil {
		t.Fatalf("empty store: plan %+v, err %v", p, err)
	}
	if err := SavePlan(ctx, st, running("n1", "n2")); err != nil {
		t.Fatal(err)
	}
	if p, err := LoadPlan(ctx, st); err != nil || p.Target != "1.12.0" || len(p.Order) != 2 {
		t.Fatalf("plan %+v, err %v", p, err)
	}
	for _, n := range []string{"n1", "n2"} {
		if err := SaveReport(ctx, st, n, at("1.11.3")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListReports(ctx, st)
	if err != nil || len(got) != 2 || got["n2"].Version != "1.11.3" {
		t.Fatalf("reports %+v, err %v", got, err)
	}
}
