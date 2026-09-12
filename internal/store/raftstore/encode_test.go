package raftstore

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

func TestCommandRoundTrip(t *testing.T) {
	c := &pb.Command{
		Version: CommandVersion,
		Type:    pb.CommandType_COMMAND_TYPE_TXN,
		Key:     "/some/key",
		Value:   []byte{0x00, 0xff, 0x10},
		Expect:  42,
		Ops: []*pb.Op{
			{Kind: pb.OpKind_OP_KIND_PUT, Key: "/a", Value: []byte("v"), Expect: 7},
			{Kind: pb.OpKind_OP_KIND_DELETE, Key: "/b"},
			{Kind: pb.OpKind_OP_KIND_CHECK, Key: "/c", Expect: 9},
		},
		TimestampUnixNs: 1234567890,
		RequestId:       "req-1",
	}
	b, err := encodeCommand(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := decodeCommand(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !proto.Equal(c, got) {
		t.Errorf("round-trip mismatch:\n got  %v\n want %v", got, c)
	}
}

func TestCommandUnknownFieldForwardCompat(t *testing.T) {
	c := &pb.Command{Version: CommandVersion, Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/k"}
	b, err := encodeCommand(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// Append an unknown field: tag 99 (field number 12, wire type 2,
	// length-delimited) + payload. A v1 decoder must accept it.
	b = append(b, 0x62, 0x03, 'x', 'y', 'z')
	got, err := decodeCommand(b)
	if err != nil {
		t.Fatalf("decode with unknown field: %v", err)
	}
	if got.GetKey() != "/k" {
		t.Errorf("key = %q, want /k", got.GetKey())
	}
}

func TestCommandVersionRejected(t *testing.T) {
	for _, tc := range []struct {
		desc string
		c    *pb.Command
		kind errors.Kind
	}{
		{"version 2", &pb.Command{Version: 2, Type: pb.CommandType_COMMAND_TYPE_PUT}, errors.KindInvalid},
		{"version 0", &pb.Command{Version: 0, Type: pb.CommandType_COMMAND_TYPE_PUT}, errors.KindInvalid},
		{"unknown type", &pb.Command{Version: 1, Type: pb.CommandType_COMMAND_TYPE_TXN + 40}, errors.KindInvalid},
		{"garbage bytes", nil, errors.KindInternal},
	} {
		var b []byte
		if tc.c != nil {
			var err error
			if b, err = encodeCommand(tc.c); err != nil {
				t.Fatalf("encode: %v", err)
			}
		} else {
			b = []byte{0xff, 0xff, 0xff}
		}
		_, err := decodeCommand(b)
		if err == nil {
			t.Errorf("%s: decode accepted", tc.desc)
			continue
		}
		if errors.KindOf(err) != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.desc, errors.KindOf(err), tc.kind)
		}
	}
}

func TestOpsConversion(t *testing.T) {
	ops := []store.Op{
		{Kind: store.OpPut, Key: "/a", Value: []byte("v"), Expect: 1},
		{Kind: store.OpDelete, Key: "/b", Expect: 2},
		{Kind: store.OpCheck, Key: "/c", Expect: 3},
	}
	got, err := storeOps(protoOps(ops))
	if err != nil {
		t.Fatalf("storeOps: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	for i := range ops {
		if got[i].Kind != ops[i].Kind || got[i].Key != ops[i].Key || got[i].Expect != ops[i].Expect || !bytes.Equal(got[i].Value, ops[i].Value) {
			t.Errorf("op[%d] = %+v, want %+v", i, got[i], ops[i])
		}
	}
	// Unknown kind rejected.
	if _, err := storeOps([]*pb.Op{{Kind: pb.OpKind(99), Key: "/x"}}); err == nil {
		t.Error("storeOps accepted unknown op kind")
	}
}

func TestDedupCache(t *testing.T) {
	d := newDedupCache(4)
	// Empty ID is a no-op.
	d.Add("", 5, true)
	if _, _, found := d.Get(""); found {
		t.Error("empty ID should never be cached")
	}
	if d.Len() != 0 {
		t.Fatalf("Len = %d, want 0", d.Len())
	}

	d.Add("a", 1, true)
	d.Add("b", 2, false)
	if rev, ok, found := d.Get("a"); !found || rev != 1 || !ok {
		t.Errorf("Get(a) = %d,%v,%v", rev, ok, found)
	}
	if rev, ok, found := d.Get("b"); !found || rev != 2 || ok {
		t.Errorf("Get(b) = %d,%v,%v", rev, ok, found)
	}
	// Miss.
	if _, _, found := d.Get("c"); found {
		t.Error("Get(c) should miss")
	}
	// Update in place.
	d.Add("a", 9, false)
	if rev, ok, found := d.Get("a"); !found || rev != 9 || ok {
		t.Errorf("Get(a) after update = %d,%v,%v", rev, ok, found)
	}
	// LRU eviction: capacity 4, "b" is now oldest.
	d.Add("c", 3, true)
	d.Add("d", 4, true)
	d.Add("e", 5, true)
	if d.Len() != 4 {
		t.Fatalf("Len = %d, want 4", d.Len())
	}
	if _, _, found := d.Get("b"); found {
		t.Error("oldest entry b should have been evicted")
	}
	if _, _, found := d.Get("e"); !found {
		t.Error("newest entry e missing")
	}
	// Re-getting "a" refreshes it, then adding 1 more evicts "c".
	d.Get("a")
	d.Add("f", 6, true)
	if _, _, found := d.Get("c"); found {
		t.Error("c should have been evicted after a was refreshed")
	}
	if _, _, found := d.Get("a"); !found {
		t.Error("a should still be cached")
	}
}

// encodeCommandBytes is a helper for building log entries in tests.
func encodeCommandBytes(t *testing.T, c *pb.Command) []byte {
	t.Helper()
	c.Version = CommandVersion
	b, err := encodeCommand(c)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func TestEncodeCanonicalDeterministic(t *testing.T) {
	// Smoke-test through the FSM: same state → same canonical bytes.
	f1, f2 := NewFSM(), NewFSM()
	cmds := []*pb.Command{
		{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/b", Value: []byte("2"), TimestampUnixNs: 1},
		{Type: pb.CommandType_COMMAND_TYPE_PUT, Key: "/a", Value: []byte("1"), TimestampUnixNs: 2},
		{Type: pb.CommandType_COMMAND_TYPE_DELETE, Key: "/b"},
	}
	for _, c := range cmds {
		f1.Apply(testLog(encodeCommandBytes(t, c)))
	}
	// Apply in a different order → different state, but for the SAME order
	// on a second FSM the bytes must be identical.
	for _, c := range cmds {
		f2.Apply(testLog(encodeCommandBytes(t, c)))
	}
	if !bytes.Equal(f1.canonicalSnapshot(), f2.canonicalSnapshot()) {
		t.Error("canonical serialization differs for identical state")
	}
}

func (f *FSM) canonicalSnapshot() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canonicalLocked()
}
