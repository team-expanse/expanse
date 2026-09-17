package transport

import (
	"bufio"
	"context"
	"fmt"
	"net"

	"github.com/expanse/expanse/internal/storage/exvol/protocol"
	expb "github.com/expanse/expanse/proto"
	"google.golang.org/protobuf/proto"
)

// Handler processes one replicated write on the secondary side. It is
// T04's protocol.Secondary.Handle wrapped around the local writer (T06);
// the transport only moves the bytes.
type Handler func(op protocol.WriteOp) protocol.Reply

// RawHandler processes one resync chunk (zfs send stream bytes) and
// returns a response chunk. Used by T12; nil means resync is refused.
type RawHandler func(chunk []byte) ([]byte, error)

// Server is a replication listener. One Server can serve many volumes:
// WriteRequests carry vol_id and the VolumeRouter picks the handler.
type Server struct {
	ln         net.Listener
	router     func(volID string) (Handler, error)
	rawHandler RawHandler
	dscp       int // 0 = no marking
}

// NewServer wraps a listener. router resolves a volume ID to its
// protocol handler; dscp > 0 marks every accepted connection with that
// DSCP class (resync connections).
func NewServer(ln net.Listener, router func(volID string) (Handler, error), rawHandler RawHandler, dscp int) *Server {
	return &Server{ln: ln, router: router, rawHandler: rawHandler, dscp: dscp}
}

// Addr is the listening address (for tests to learn the port).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts connections until the listener is closed. Connection
// errors are logged-and-dropped; the caller restarts or the controller
// reconnects. Blocks — run in a goroutine.
func (s *Server) Serve() error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return err
		}
		if tc, ok := nc.(*net.TCPConn); ok && s.dscp > 0 {
			markDSCP(tc, s.dscp)
		}
		go s.handleConn(nc)
	}
}

func (s *Server) handleConn(nc net.Conn) {
	defer nc.Close()
	c := &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}

	for {
		typ, payload, err := readFrame(c.r)
		if err != nil {
			return // EOF or protocol violation: drop the connection
		}
		switch typ {
		case msgWriteRequest:
			req := &expb.WriteRequest{}
			if err := proto.Unmarshal(payload, req); err != nil {
				return
			}
			h, err := s.router(req.GetVolId())
			if err != nil {
				// Unknown volume: explicit NACK-style reply, then drop.
				rep := &expb.WriteReply{Ack: false, Reason: err.Error()}
				if b, merr := proto.Marshal(rep); merr == nil {
					_ = writeFrame(c.w, msgWriteReply, b)
					_ = c.w.Flush()
				}
				return
			}
			reply := h(protocol.WriteOp{
				Seq:    req.GetSeq(),
				Offset: req.GetOffset(),
				Data:   req.GetData(),
				CRC:    req.GetCrc32C(),
				Flush:  req.GetFlush(),
			})
			b, err := proto.Marshal(replyToPB(reply))
			if err != nil {
				return
			}
			if err := writeFrame(c.w, msgWriteReply, b); err != nil {
				return
			}
			if err := c.w.Flush(); err != nil {
				return
			}

		case msgResyncChunk:
			if s.rawHandler == nil {
				return // resync refused on this server
			}
			resp, err := s.rawHandler(payload)
			if err != nil {
				return
			}
			if err := writeFrame(c.w, msgResyncAck, resp); err != nil {
				return
			}
			if err := c.w.Flush(); err != nil {
				return
			}

		default:
			return // unknown frame type: protocol violation
		}
	}
}

// conn is the shared connection state for the client side.
type conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// Conn is a client connection to a secondary. Requests are pipelined:
// multiple Send calls may precede the matching Recv calls (replies come
// back in send order). Use Sender for the R3 bounded-window wrapper.
type Conn struct {
	c        *conn
	throttle *Throttle // non-nil → resync chunks are rate-limited
}

// Dial connects to a secondary. Foreground write connections are never
// rate-limited; rate limiting applies to resync connections (WithResync
// throttle).
func Dial(ctx context.Context, addr string, opts ...DialOption) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial replica %s: %w", addr, err)
	}
	c := &Conn{c: &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// DialOption configures a client connection.
type DialOption func(*Conn)

// WithResyncThrottle rate-limits outbound resync chunks (§4.3 bandwidth
// control, default 100 MiB/s — set explicitly per connection).
func WithResyncThrottle(t *Throttle) DialOption { return func(c *Conn) { c.throttle = t } }

// WithDSCP marks the connection's IP packets with a DSCP class (§4.3
// bandwidth control). Best-effort: failures are ignored, traffic still
// flows unmarked.
func WithDSCP(class int) DialOption {
	return func(c *Conn) {
		if tc, ok := c.c.nc.(*net.TCPConn); ok {
			markDSCP(tc, class)
		}
	}
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.nc.Close() }

// Send pipelines one WriteOp to the secondary. It does not wait for the
// reply — pair with Recv (in send order), usually via Sender.
func (c *Conn) Send(volID string, op protocol.WriteOp) error {
	b, err := proto.Marshal(&expb.WriteRequest{
		VolId:  volID,
		Seq:    op.Seq,
		Offset: op.Offset,
		Data:   op.Data,
		Crc32C: op.CRC,
		Flush:  op.Flush,
	})
	if err != nil {
		return err
	}
	if err := writeFrame(c.c.w, msgWriteRequest, b); err != nil {
		return err
	}
	return c.c.w.Flush()
}

// Recv reads the next reply in send order.
func (c *Conn) Recv() (protocol.Reply, error) {
	typ, payload, err := readFrame(c.c.r)
	if err != nil {
		return protocol.Reply{}, err
	}
	if typ != msgWriteReply {
		return protocol.Reply{}, fmt.Errorf("unexpected frame type %d, want %d", typ, msgWriteReply)
	}
	rep := &expb.WriteReply{}
	if err := proto.Unmarshal(payload, rep); err != nil {
		return protocol.Reply{}, err
	}
	return replyFromPB(rep), nil
}

// SendResyncChunk sends one raw resync chunk, rate-limited by the
// connection's throttle (if any), and blocks for the response chunk.
func (c *Conn) SendResyncChunk(chunk []byte) ([]byte, error) {
	if c.throttle != nil {
		c.throttle.Wait(len(chunk))
	}
	if err := writeFrame(c.c.w, msgResyncChunk, chunk); err != nil {
		return nil, err
	}
	if err := c.c.w.Flush(); err != nil {
		return nil, err
	}
	typ, payload, err := readFrame(c.c.r)
	if err != nil {
		return nil, err
	}
	if typ != msgResyncAck {
		return nil, fmt.Errorf("unexpected frame type %d, want %d", typ, msgResyncAck)
	}
	return payload, nil
}

// --- protobuf <-> protocol conversions (single source of truth for the
// wire mapping, so no double-encoding surprises can hide) ---

func replyToPB(r protocol.Reply) *expb.WriteReply {
	return &expb.WriteReply{
		Ack:        r.ACK,
		Seq:        r.Seq,
		LastSeq:    r.LastSeq,
		Gap:        r.Gap,
		Retransmit: r.Retransmit,
		Resync:     r.Resync,
		Reason:     r.Reason,
	}
}

func replyFromPB(r *expb.WriteReply) protocol.Reply {
	return protocol.Reply{
		ACK:        r.GetAck(),
		Seq:        r.GetSeq(),
		LastSeq:    r.GetLastSeq(),
		Gap:        r.GetGap(),
		Retransmit: r.GetRetransmit(),
		Resync:     r.GetResync(),
		Reason:     r.GetReason(),
	}
}

// Close shuts the server down (closes the listener; open conns error
// out on their next I/O).
func (s *Server) Close() error { return s.ln.Close() }
