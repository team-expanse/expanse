// Package transport wires T04's protocol simulator message types
// (protocol.WriteOp / protocol.Reply) to real TCP connections on the
// exp0 WireGuard mesh (Phase 06 T05).
//
// Transport choice — TCP, deliberately: the spec doesn't mandate QUIC
// and the replication path is bulk, ordered, in-datacenter-over-VPN
// traffic where kernel TCP with one stream per (primary, secondary)
// pair is simpler to reason about, and ordering on the wire matches
// R4's ordering requirement for free. QUIC would add a userspace
// dependency for no measured gain at this stage.
//
// Framing (exact, interop-relevant across rolling upgrades):
//
//	+0  uint32 BE  frame length (of everything after this field)
//	+4  byte       version (1; unknown version → hard error, no
//	               negotiation yet — that is a later-phase concern)
//	+5  byte       message type (1=WriteRequest, 2=WriteReply,
//	                               3=ResyncChunk, 4=ResyncAck,
//	                               5=SeqQuery, 6=FetchOps)
//	+6  ...        payload (protobuf for types 1–2, raw bytes for 3–4)
//
// Max frame = 68 MiB (64 MiB max op payload + slack). A peer sending a
// larger frame is treated as a protocol violation and the connection is
// dropped.
//
// Backpressure matches R3's bounded window exactly: a Sender allows a
// bounded number of in-flight (un-acked) ops by count AND bytes; Submit
// blocks when the window is full — never drops.
//
// Resync traffic (§4.3 bandwidth control) is rate-limited by a
// token-bucket throttle (default 100 MiB/s) and DSCP-marked so it does
// not starve foreground I/O. Foreground writes are never rate-limited.
package transport

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	// Version is the current frame version byte.
	Version = 1

	msgWriteRequest = 1
	msgWriteReply   = 2
	msgResyncChunk  = 3
	msgResyncAck    = 4
	msgQuerySeq     = 5
	msgFetchOps     = 6

	// MaxFrameSize caps a frame: 64 MiB op payload plus framing slack.
	MaxFrameSize = 68 << 20

	headerLen = 6 // 4 (len) + 1 (version) + 1 (type)
)

type frameType byte

// writeFrame writes one framed message: length prefix, version, type,
// payload.
func writeFrame(w io.Writer, typ frameType, payload []byte) error {
	if len(payload)+headerLen > MaxFrameSize {
		return fmt.Errorf("frame payload %d exceeds max %d", len(payload), MaxFrameSize-headerLen)
	}
	hdr := make([]byte, headerLen)
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(payload)+2))
	hdr[4] = Version
	hdr[5] = byte(typ)
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// readFrame reads one framed message, validating length and version.
func readFrame(r io.Reader) (frameType, []byte, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	if hdr[4] != Version {
		return 0, nil, fmt.Errorf("protocol version %d, want %d (rolling-upgrade negotiation is a later-phase concern)",
			hdr[4], Version)
	}
	n := binary.BigEndian.Uint32(hdr[0:4]) - 2
	if n > MaxFrameSize-headerLen {
		return 0, nil, fmt.Errorf("frame length %d exceeds max %d", n, MaxFrameSize-headerLen)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return frameType(hdr[5]), payload, nil
}
