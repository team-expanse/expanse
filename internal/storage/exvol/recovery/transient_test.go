package recovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

var opData = []byte("acked-bytes")

func holder(id string, fetch func(context.Context, uint64, uint64) ([]protocol.WriteOp, error)) Probe {
	return Probe{
		NodeID: id, Reachable: true, LastSeq: 5,
		CRCs:     map[uint64]uint32{5: crc32c(opData)},
		FetchOps: fetch,
	}
}

func serves(_ context.Context, _, to uint64) ([]protocol.WriteOp, error) {
	return []protocol.WriteOp{{Seq: to, Data: opData, CRC: crc32c(opData)}}, nil
}

func drops(context.Context, uint64, uint64) ([]protocol.WriteOp, error) {
	return nil, &TransportError{Err: errors.New("read: connection reset")}
}

func lies(context.Context, uint64, uint64) ([]protocol.WriteOp, error) {
	return []protocol.WriteOp{{Seq: 5, Data: []byte("garbage!!!!"), CRC: 1}}, nil
}

func refuses(context.Context, uint64, uint64) ([]protocol.WriteOp, error) {
	return nil, errors.New("replica: op re-read failed")
}

// A holder whose connection broke mid-fetch says nothing about its data:
// the op may well be there. That is retryable, never "data loss".
func TestFillOneTransportFailureIsTransient(t *testing.T) {
	_, err := FillOne(context.Background(), []Probe{holder("n2", drops)}, 5)
	var tr *TransientError
	if !errors.As(err, &tr) || tr.Seq != 5 {
		t.Fatalf("err = %v, want *TransientError for op 5", err)
	}
	var un *UnfillableError
	if errors.As(FetchFailure(5, err), &un) {
		t.Fatal("a transient failure must not become UnfillableError")
	}
}

func TestFillOneAnsweredRefusalStaysDefinitive(t *testing.T) {
	for name, fetch := range map[string]func(context.Context, uint64, uint64) ([]protocol.WriteOp, error){"error reply": refuses, "bad bytes": lies} {
		_, err := FillOne(context.Background(), []Probe{holder("n2", fetch)}, 5)
		var tr *TransientError
		if errors.As(err, &tr) {
			t.Errorf("%s: an answering holder that cannot serve is dishonest, not transient (%v)", name, err)
		}
		var un *UnfillableError
		if !errors.As(FetchFailure(5, err), &un) {
			t.Errorf("%s: want UnfillableError, got %v", name, FetchFailure(5, err))
		}
	}
}

// One holder lies and another is unreachable: the unreachable one may
// still hold the op, so this cannot be called data loss yet.
func TestFillOneMixedFailuresAreTransient(t *testing.T) {
	_, err := FillOne(context.Background(), []Probe{holder("n2", lies), holder("n3", drops)}, 5)
	var tr *TransientError
	if !errors.As(err, &tr) {
		t.Fatalf("err = %v, want *TransientError", err)
	}
}

func TestFillOneServedByAnotherHolderDespiteDrop(t *testing.T) {
	op, err := FillOne(context.Background(), []Probe{holder("n2", drops), holder("n3", serves)}, 5)
	if err != nil || op.Seq != 5 || string(op.Data) != string(opData) {
		t.Fatalf("op=%+v err=%v, want the op served by n3", op, err)
	}
}

// When nobody claims a needed op the failure must say what each probe
// reported (a replica can advertise a last seq whose ops it never logged,
// e.g. after adopting a snapshot), and it is still definitive.
func TestFillOneNoClaimantExplainsTheProbes(t *testing.T) {
	adopted := Probe{NodeID: "n3", Reachable: true, LastSeq: 9, CRCs: map[uint64]uint32{7: 1, 8: 2}, FetchOps: serves}
	gone := Probe{NodeID: "n1", Reachable: false}
	_, err := FillOne(context.Background(), []Probe{gone, adopted}, 9)
	if err == nil {
		t.Fatal("op 9 has no claimant: must fail")
	}
	for _, want := range []string{"no reachable replica claims op 9", "n3", "last=9", "claims=2", "max=8", "n1", "unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	var un *UnfillableError
	if !errors.As(FetchFailure(9, err), &un) {
		t.Fatal("no claimant stays a definitive (unfillable) failure")
	}
}
