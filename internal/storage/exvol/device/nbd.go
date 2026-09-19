package device

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
)

// NBD server (§4.4 device transport). Newstyle handshake with
// NBD_OPT_EXPORT_NAME; transmission phase supports the commands the
// kernel client issues for a filesystem-backed volume:
// READ(0) WRITE(1) DISC(2) FLUSH(3) TRIM(4) WRITE_ZEROES(6).
//
// The device appears at /dev/nbd<N> via `nbd-client -unix`; attach()
// creates the /dev/exvol/<vol-id> symlink (§4.4 "udev symlink from
// /dev/ublkbN" — same shape for the NBD transport).

const (
	nbdMagic       = "NBDMAGIC"         // 0x4E42444D41474943
	nbdNewStyle    = 0x49484156454F5054 // "IHAVEOPT"
	nbdOptExport   = 1
	nbdOptAbort    = 2
	nbdOptList     = 3
	nbdOptStartTLS = 4
	nbdOptInfo     = 5
	nbdOptGo       = 6
	nbdOptStruct   = 8 // NBD_OPT_STRUCTURED_REPLY
	nbdOptListMeta = 9
	nbdOptSetMeta  = 10

	nbdRepAck        = 1
	nbdRepInfo       = 3
	nbdRepErrUnsup   = 0x80000001
	nbdOptReplyMagic = 0x0003E889045565A9
	nbdReqMagic      = 0x25609513
	nbdRepMagic      = 0x67446698
	nbdCmdRead       = 0
	nbdCmdWrite      = 1
	nbdCmdDisc       = 2
	nbdCmdFlush      = 3
	nbdCmdTrim       = 4
	nbdWriteZeroes   = 6

	nbdFlagHasFlags  = 1 // transmission flag bit 0
	nbdFlagSendFlush = 1 << 2
	nbdFlagSendTrim  = 1 << 5
	nbdFlagSendDisc  = 1 << 7
	nbdFlagSendWZ    = 1 << 9
)

// Server serves one BlockDevice over NBD on a unix socket.
type Server struct {
	ln     net.Listener
	dev    BlockDevice
	sock   string
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
}

// Listen creates the unix socket for the NBD server.
func Listen(sock string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return nil, err
	}
	os.Remove(sock) //nolint:errcheck — stale socket after a crash
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, experrors.Wrap(err, experrors.KindUnavailable, "exvol.nbd.listen", "listen failed")
	}
	return &Server{ln: ln, sock: sock, conns: map[net.Conn]struct{}{}}, nil
}

// SetDevice binds the device (before Serve).
func (s *Server) SetDevice(d BlockDevice) { s.dev = d }

// Serve accepts and serves client connections until Close.
func (s *Server) Serve() error {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			nc.Close()
			return nil
		}
		s.conns[nc] = struct{}{}
		s.mu.Unlock()
		go func() {
			defer func() {
				s.mu.Lock()
				delete(s.conns, nc)
				s.mu.Unlock()
				nc.Close()
			}()
			serveNBDConn(nc, s.dev)
		}()
	}
}

// Close shuts the server down and closes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for nc := range s.conns {
		nc.Close() //nolint:errcheck
	}
	s.mu.Unlock()
	err := s.ln.Close()
	os.Remove(s.sock) //nolint:errcheck
	return err
}

// nbdExportFlags is the transmission-flag set advertised on both the
// EXPORT_NAME and OPT_GO paths. TRIM is deliberately not advertised: it
// is a hint, ExvolDevice.Discard is not replicated (it would leave the
// replicas holding bytes the primary dropped), and zvols reject the
// hole-punch anyway. The kernel therefore never sends it.
const nbdExportFlags = uint16(nbdFlagHasFlags | nbdFlagSendFlush | nbdFlagSendDisc | nbdFlagSendWZ)

// nbdOptionReply writes an option-reply record.
func nbdOptionReply(w io.Writer, option, replyType uint32, payload []byte) error {
	rep := make([]byte, 20)
	binary.BigEndian.PutUint64(rep[0:], nbdOptReplyMagic)
	binary.BigEndian.PutUint32(rep[8:], option)
	binary.BigEndian.PutUint32(rep[12:], replyType)
	binary.BigEndian.PutUint32(rep[16:], uint32(len(payload)))
	_, err := w.Write(append(rep, payload...))
	return err
}

// debugLog is wired for VM-test diagnostics (T10); replaced by the
// node logger when the runtime owns server construction.
var debugLog = func(format string, args ...any) {
	slog.Info(fmt.Sprintf("exvol.nbd: "+format, args...))
}

// serveNBDConn runs one client: newstyle handshake + transmission loop.
func serveNBDConn(nc net.Conn, dev BlockDevice) {
	defer nc.Close() //nolint:errcheck
	r := io.Reader(nc)
	w := io.Writer(nc)

	// Handshake: magic(8) + IHAVEOPT(8) + flags(2).
	var hs [18]byte
	copy(hs[0:], nbdMagic)
	binary.BigEndian.PutUint64(hs[8:], nbdNewStyle)
	binary.BigEndian.PutUint16(hs[16:], 1) // NBD_FLAG_FIXED_NEWSTYLE
	if _, err := w.Write(hs[:]); err != nil {
		return
	}

	// Client flags (4 bytes, read and ignore).
	var cf [4]byte
	if _, err := io.ReadFull(r, cf[:]); err != nil {
		return
	}

	// Options until NBD_OPT_EXPORT_NAME: magic(8) + option(4) +
	// length(4) + payload.
	var magic [8]byte
	var optAndLen [8]byte
exportDone:
	for {
		if _, err := io.ReadFull(r, magic[:]); err != nil {
			return
		}
		if binary.BigEndian.Uint64(magic[:]) != nbdNewStyle {
			return // protocol violation
		}
		if _, err := io.ReadFull(r, optAndLen[:]); err != nil {
			return
		}
		optLen := binary.BigEndian.Uint32(optAndLen[4:])
		payload := make([]byte, optLen)
		if optLen > 0 {
			if _, err := io.ReadFull(r, payload); err != nil {
				return
			}
		}
		switch binary.BigEndian.Uint32(optAndLen[:4]) {
		case nbdOptExport:
			break exportDone
		case nbdOptGo, nbdOptInfo:
			// Accept: REPLY_MAGIC(8) + option(4) + type(4) + len(4).
			// NBD_REP_INFO with NBD_INFO_EXPORT (size + flags), then
			// NBD_REP_ACK. Modern nbd-client negotiates via GO; the
			// kernel client requires the INFO record to size the device.
			if dev == nil {
				return
			}
			info := make([]byte, 14)
			binary.BigEndian.PutUint16(info[0:], 0) // NBD_INFO_EXPORT
			binary.BigEndian.PutUint64(info[2:], uint64(dev.Size()))
			binary.BigEndian.PutUint16(info[10:], nbdExportFlags)
			if err := nbdOptionReply(w, binary.BigEndian.Uint32(optAndLen[:4]), nbdRepInfo, info); err != nil {
				return
			}
			if err := nbdOptionReply(w, binary.BigEndian.Uint32(optAndLen[:4]), nbdRepAck, nil); err != nil {
				return
			}
			continue
		case nbdOptAbort:
			return
		case nbdOptStruct, nbdOptList, nbdOptListMeta, nbdOptSetMeta, nbdOptStartTLS:
			// Refused: the client proceeds without structured replies
			// / TLS (the mesh already encrypts; TLS here is redundant).
			if err := nbdOptionReply(w, binary.BigEndian.Uint32(optAndLen[:4]), nbdRepErrUnsup, nil); err != nil {
				return
			}
		default:
			if err := nbdOptionReply(w, binary.BigEndian.Uint32(optAndLen[:4]), nbdRepErrUnsup, nil); err != nil {
				return
			}
		}
	}

	if dev == nil {
		return // no device bound
	}

	// Export info: size(8) + transmission flags(2) + 124 reserved zeros.
	var info [134]byte
	binary.BigEndian.PutUint64(info[0:], uint64(dev.Size()))
	binary.BigEndian.PutUint16(info[8:], nbdExportFlags)
	if _, err := w.Write(info[:]); err != nil {
		return
	}

	// Transmission loop.
	var req [28]byte // magic(4) flags(2) type(2) handle(8) offset(8) len(4)
	var rep [16]byte // magic(4) error(4) handle(8)
	for {
		if _, err := io.ReadFull(r, req[:]); err != nil {
			return
		}
		if binary.BigEndian.Uint32(req[0:]) != nbdReqMagic {
			return // protocol violation
		}
		cmd := binary.BigEndian.Uint16(req[6:])
		handle := binary.BigEndian.Uint64(req[8:])
		offset := int64(binary.BigEndian.Uint64(req[16:]))
		length := int64(binary.BigEndian.Uint32(req[24:]))

		var errCode uint32
		switch cmd {
		case nbdCmdRead:
			buf := make([]byte, length)
			if _, err := dev.ReadAt(buf, offset); err != nil {
				errCode = 5 // EIO
			} else {
				binary.BigEndian.PutUint32(rep[0:], nbdRepMagic)
				binary.BigEndian.PutUint32(rep[4:], 0)
				binary.BigEndian.PutUint64(rep[8:], handle)
				if _, err := w.Write(rep[:]); err != nil {
					return
				}
				if _, err := w.Write(buf); err != nil {
					return
				}
				continue
			}
		case nbdCmdWrite:
			buf := make([]byte, length)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			if _, err := dev.WriteAt(buf, offset); err != nil {
				debugLog("write %d bytes at %d failed: %v", length, offset, err)
				errCode = 5
			}
		case nbdCmdFlush:
			if err := dev.Flush(context.Background()); err != nil {
				errCode = 5
			}
		case nbdCmdTrim:
			if err := dev.Discard(offset, length); err != nil {
				errCode = 5
			}
		case nbdWriteZeroes:
			// Semantics: write length zero bytes (flag 1 = "no hole"
			// is an optimization we ignore — zeros are zeros either way).
			zeros := make([]byte, 4096)
			for n := int64(0); n < length; n += 4096 {
				c := int64(4096)
				if n+c > length {
					c = length - n
				}
				if _, err := dev.WriteAt(zeros[:c], offset+n); err != nil {
					errCode = 5
					break
				}
			}
		case nbdCmdDisc:
			return // client disconnects
		default:
			errCode = 1 // EPERM: unsupported command
		}

		binary.BigEndian.PutUint32(rep[0:], nbdRepMagic)
		binary.BigEndian.PutUint32(rep[4:], errCode)
		binary.BigEndian.PutUint64(rep[8:], handle)
		if _, err := w.Write(rep[:]); err != nil {
			return
		}
	}
}

// --- attach (node-side): nbd-client to the socket, udev-style symlink ---

// Runner shells out (injectable for tests).
type Runner interface {
	Run(name string, args ...string) error
}

// ExecRunner runs real commands.
type ExecRunner struct{}

// Run executes argv.
func (ExecRunner) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %v: %v: %s", name, args, err, out)
	}
	return nil
}

// Attach connects /dev/nbd<N> to the server's socket and symlinks
// /dev/exvol/<volID> to it (§4.4 device appearance).
func Attach(sock, volID string, nbdDev string, r Runner) error {
	if err := os.MkdirAll("/dev/exvol", 0o755); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "exvol.nbd.attach", "mkdir /dev/exvol failed")
	}
	if err := r.Run("nbd-client", "-unix", sock, nbdDev); err != nil {
		return experrors.Wrap(err, experrors.KindUnavailable, "exvol.nbd.attach",
			"nbd-client failed: "+err.Error())
	}
	link := "/dev/exvol/" + volID
	os.Remove(link) //nolint:errcheck — replace stale symlink
	return os.Symlink(nbdDev, link)
}

// Detach disconnects the NBD device and removes the symlink.
func Detach(volID, nbdDev string, r Runner) error {
	os.Remove("/dev/exvol/" + volID) //nolint:errcheck
	if err := r.Run("nbd-client", "-d", nbdDev); err != nil {
		return experrors.Wrap(err, experrors.KindUnavailable, "exvol.nbd.detach", "nbd-client -d failed")
	}
	return nil
}
