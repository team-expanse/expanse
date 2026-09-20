package recovery

import (
	"context"
	"strings"
	"testing"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
)

// claimed builds a probe whose oplog claims seqs 1..n (odd = write, even = flush)
// and whose FetchOps serves bytes through serve.
func claimed(n uint64, serve func(seq uint64) []byte) Probe {
	crcs := map[uint64]uint32{}
	for s := uint64(1); s <= n; s++ {
		if s%2 == 1 {
			crcs[s] = crc32c(dataFor(s))
		}
	}
	return Probe{
		NodeID: "n1", Reachable: true, LastSeq: n, CRCs: crcs,
		FetchOps: func(_ context.Context, _, to uint64) ([]protocol.WriteOp, error) {
			if to%2 == 0 {
				return []protocol.WriteOp{{Seq: to, Flush: true}}, nil
			}
			return []protocol.WriteOp{{Seq: to, Data: serve(to)}}, nil
		},
	}
}

func dataFor(seq uint64) []byte { return []byte{byte(seq), 0xAB, 0xCD, byte(seq >> 8)} }

func TestVerifyClaimsAcceptsAReplicaThatServesWhatItClaims(t *testing.T) {
	p := claimed(40, dataFor)
	if err := p.VerifyClaims(context.Background()); err != nil {
		t.Fatalf("a healthy replica was rejected: %v", err)
	}
}

// The zvol lost writes the oplog still claims (a hard crash): zeros read back without error.
func TestVerifyClaimsCatchesTornBytesAtTheOldestClaim(t *testing.T) {
	p := claimed(4, func(uint64) []byte { return make([]byte, 4) })
	if err := p.VerifyClaims(context.Background()); err == nil || !strings.Contains(err.Error(), "crc32c") {
		t.Fatalf("err = %v, want a CRC mismatch", err)
	}
}

func TestVerifyClaimsCatchesTornBytesAmongTheNewestClaims(t *testing.T) {
	p := claimed(40, func(seq uint64) []byte {
		if seq == 35 {
			return make([]byte, 4) // lost; neither the oldest nor the newest claim
		}
		return dataFor(seq)
	})
	if err := p.VerifyClaims(context.Background()); err == nil {
		t.Fatal("torn bytes among the newest claims went unnoticed")
	}
}

func TestVerifyClaimsTreatsAFetchFailureAsUnserveable(t *testing.T) {
	p := claimed(4, dataFor)
	p.FetchOps = func(context.Context, uint64, uint64) ([]protocol.WriteOp, error) {
		return nil, context.DeadlineExceeded
	}
	if err := p.VerifyClaims(context.Background()); err == nil {
		t.Fatal("a replica that cannot serve its claims must fail verification")
	}
}
