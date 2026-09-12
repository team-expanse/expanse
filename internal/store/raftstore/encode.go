// Package raftstore implements store.Store over hashicorp/raft: a
// deterministic replicated FSM (fsm.go), a versioned command encoding
// (encode.go), and leader-forwarded writes with linearizable reads.
//
// It passes the exact same conformance suite as the Phase 02 boltstore
// (internal/store/conformance).
package raftstore

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/store"
	pb "github.com/expanse/expanse/proto"
)

// CommandVersion is the current command encoding version. Bump when the
// encoding changes; Phase 16's rolling upgrade relies on it.
const CommandVersion uint32 = 1

// Command is the decoded form of the replicated log entry (see
// proto/store.proto). All timestamps travel inside the command — the FSM
// never consults a clock.
type Command = pb.Command

// CommandType values.
const (
	CmdPut    = pb.CommandType_COMMAND_TYPE_PUT
	CmdDelete = pb.CommandType_COMMAND_TYPE_DELETE
	CmdTxn    = pb.CommandType_COMMAND_TYPE_TXN
)

// encodeCommand serializes a Command for the Raft log.
func encodeCommand(c *Command) ([]byte, error) {
	return proto.Marshal(c)
}

// decodeCommand parses Raft log data. Unknown protobuf fields are retained
// (forward compatibility: a v1 reader silently ignores fields it does not
// know). An unsupported version is rejected loudly.
func decodeCommand(b []byte) (*Command, error) {
	var c pb.Command
	if err := proto.Unmarshal(b, &c); err != nil {
		return nil, errors.Wrap(err, errors.KindInternal, "raftstore.decode", "unmarshal command")
	}
	if c.GetVersion() != CommandVersion {
		return nil, errors.New(errors.KindInvalid, "raftstore.decode",
			fmt.Sprintf("unsupported command version %d (want %d)", c.GetVersion(), CommandVersion))
	}
	switch c.GetType() {
	case CmdPut, CmdDelete, CmdTxn:
	default:
		return nil, errors.New(errors.KindInvalid, "raftstore.decode", fmt.Sprintf("unknown command type %d", c.GetType()))
	}
	return &c, nil
}

// applyError converts a command-application failure into a typed error for
// transport back through raft.Apply's future.
func applyError(kind errors.Kind, op string, msg string) error {
	return errors.New(kind, op, msg)
}

// protoOps converts store ops to proto ops.
func protoOps(ops []store.Op) []*pb.Op {
	out := make([]*pb.Op, 0, len(ops))
	for _, op := range ops {
		po := &pb.Op{
			Key:    string(op.Key),
			Value:  op.Value,
			Expect: uint64(op.Expect),
		}
		switch op.Kind {
		case store.OpPut:
			po.Kind = pb.OpKind_OP_KIND_PUT
		case store.OpDelete:
			po.Kind = pb.OpKind_OP_KIND_DELETE
		case store.OpCheck:
			po.Kind = pb.OpKind_OP_KIND_CHECK
		}
		out = append(out, po)
	}
	return out
}

// storeOps converts proto ops back to store ops.
func storeOps(ops []*pb.Op) ([]store.Op, error) {
	out := make([]store.Op, 0, len(ops))
	for _, po := range ops {
		op := store.Op{
			Key:    store.Key(po.GetKey()),
			Value:  po.GetValue(),
			Expect: store.Revision(po.GetExpect()),
		}
		switch po.GetKind() {
		case pb.OpKind_OP_KIND_PUT:
			op.Kind = store.OpPut
		case pb.OpKind_OP_KIND_DELETE:
			op.Kind = store.OpDelete
		case pb.OpKind_OP_KIND_CHECK:
			op.Kind = store.OpCheck
		default:
			return nil, errors.New(errors.KindInvalid, "raftstore.storeOps", "unknown txn op kind")
		}
		out = append(out, op)
	}
	return out, nil
}
