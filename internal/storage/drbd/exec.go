// Package drbd wraps drbdadm and drbdsetup for the volume engine. It is the
// only package permitted to exec them; everything else uses the DRBD interface.
package drbd

import (
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	experrors "github.com/expanse/expanse/internal/errors"
)

// maxPeersLimit is DRBD 9's per-resource peer ceiling.
const maxPeersLimit = 31

var resourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// DRBD is the volume engine's view of the local DRBD kernel module.
type DRBD interface {
	CreateMD(ctx context.Context, res string, maxPeers int) error
	Up(ctx context.Context, res string) error
	Down(ctx context.Context, res string) error
	Primary(ctx context.Context, res string) error
	ForcePrimary(ctx context.Context, res string) error
	Secondary(ctx context.Context, res string) error
	Adjust(ctx context.Context, res string) error
	Resize(ctx context.Context, res string) error
	Verify(ctx context.Context, res string) error
	Invalidate(ctx context.Context, res string) error
	// HasMetadata reports whether the backing device carries DRBD metadata. Only a
	// positive "no valid meta data" answer is false; any doubt is an error.
	HasMetadata(ctx context.Context, res string) (bool, error)
	// AdjustPending reports whether the running config differs from the file.
	AdjustPending(ctx context.Context, res string) (bool, error)
	Connect(ctx context.Context, res string) error
	// ConnectDiscarding connects and, where the handshake finds a split-brain, makes
	// this side the one whose changes are thrown away.
	ConnectDiscarding(ctx context.Context, res string) error
	Disconnect(ctx context.Context, res string) error
	ForgetPeer(ctx context.Context, res string, nodeID int) error
	Status(ctx context.Context, res string) (*Status, error)
}

// Exec implements DRBD by shelling out. runCmd is the injection point tests use.
type Exec struct {
	DrbdadmPath   string
	DrbdsetupPath string
	runCmd        func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// New creates the default exec-backed driver.
func New() *Exec {
	return &Exec{DrbdadmPath: "drbdadm", DrbdsetupPath: "drbdsetup"}
}

func (e *Exec) cmd(ctx context.Context, name string, args ...string) *exec.Cmd {
	if e.runCmd != nil {
		return e.runCmd(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// run validates res, executes the command and returns stdout; errors name the resource.
func (e *Exec) run(ctx context.Context, op, res, name string, args ...string) ([]byte, error) {
	if !resourceName.MatchString(res) {
		return nil, experrors.New(experrors.KindInvalid, op, "invalid resource name "+strconv.Quote(res))
	}
	cmd := e.cmd(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, execErr(ctx, op, res, err, stderr.String()+stdout.String())
	}
	return stdout.Bytes(), nil
}

func execErr(ctx context.Context, op, res string, err error, stderr string) error {
	if ctx.Err() != nil {
		return experrors.Wrap(ctx.Err(), experrors.KindTimeout, op, "resource "+res+": canceled")
	}
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = err.Error()
	}
	kind := experrors.KindInternal
	if strings.Contains(msg, "not defined in your config") || strings.Contains(msg, "No such resource") {
		kind = experrors.KindNotFound
	}
	return experrors.Wrap(err, kind, op, "resource "+res+": "+msg)
}

func (e *Exec) adm(ctx context.Context, verb, res string, flags ...string) error {
	args := append(append([]string{verb}, flags...), res)
	_, err := e.run(ctx, "drbd."+verb, res, e.DrbdadmPath, args...)
	return err
}

// CreateMD writes internal metadata with maxPeers bitmap slots. It overwrites
// existing metadata, so callers must only use it on a new backing device.
func (e *Exec) CreateMD(ctx context.Context, res string, maxPeers int) error {
	if maxPeers < 1 || maxPeers > maxPeersLimit {
		return experrors.New(experrors.KindInvalid, "drbd.create-md", "max-peers out of range 1.."+strconv.Itoa(maxPeersLimit))
	}
	return e.adm(ctx, "create-md", res, "--force", "--max-peers="+strconv.Itoa(maxPeers))
}

func (e *Exec) Up(ctx context.Context, res string) error      { return e.adm(ctx, "up", res) }
func (e *Exec) Down(ctx context.Context, res string) error    { return e.adm(ctx, "down", res) }
func (e *Exec) Primary(ctx context.Context, res string) error { return e.adm(ctx, "primary", res) }

func (e *Exec) Secondary(ctx context.Context, res string) error { return e.adm(ctx, "secondary", res) }
func (e *Exec) Adjust(ctx context.Context, res string) error    { return e.adm(ctx, "adjust", res) }
func (e *Exec) Resize(ctx context.Context, res string) error    { return e.adm(ctx, "resize", res) }
func (e *Exec) Verify(ctx context.Context, res string) error    { return e.adm(ctx, "verify", res) }
func (e *Exec) Invalidate(ctx context.Context, res string) error {
	return e.adm(ctx, "invalidate", res)
}
func (e *Exec) Connect(ctx context.Context, res string) error { return e.adm(ctx, "connect", res) }
func (e *Exec) ConnectDiscarding(ctx context.Context, res string) error {
	return e.adm(ctx, "connect", res, "--discard-my-data")
}

func (e *Exec) Disconnect(ctx context.Context, res string) error {
	return e.adm(ctx, "disconnect", res)
}

const (
	noMetadata      = "No valid meta data found"
	uncleanMetadata = "unclean" // metadata exists but the activity log needs applying
)

// HasMetadata is safe to gate create-md on: unclean metadata counts as present.
func (e *Exec) HasMetadata(ctx context.Context, res string) (bool, error) {
	_, err := e.run(ctx, "drbd.dump-md", res, e.DrbdadmPath, "dump-md", res)
	switch {
	case err == nil, err != nil && strings.Contains(err.Error(), uncleanMetadata):
		return true, nil
	case strings.Contains(err.Error(), noMetadata):
		return false, nil
	}
	return false, err
}

// AdjustPending runs adjust as a dry run: it prints the commands it would issue,
// and prints nothing when the kernel already matches the file.
func (e *Exec) AdjustPending(ctx context.Context, res string) (bool, error) {
	out, err := e.run(ctx, "drbd.adjust-dry-run", res, e.DrbdadmPath, "-d", "adjust", res)
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// ForcePrimary promotes without an up-to-date peer. Only the initial promotion
// of a fresh resource may use it; it discards the peers' claim to newer data.
func (e *Exec) ForcePrimary(ctx context.Context, res string) error {
	return e.adm(ctx, "primary", res, "--force")
}

// ForgetPeer frees a dead peer's node-id and bitmap slot (see PHASE-01 D5).
func (e *Exec) ForgetPeer(ctx context.Context, res string, nodeID int) error {
	_, err := e.run(ctx, "drbd.forget-peer", res, e.DrbdsetupPath, "forget-peer", res, strconv.Itoa(nodeID))
	return err
}
