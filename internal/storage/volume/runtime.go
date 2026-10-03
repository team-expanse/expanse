package volume

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	experrors "github.com/expanse/expanse/internal/errors"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
)

// maxPeers is the bitmap slots reserved at create-md: room for rebuilds under fresh ids.
const maxPeers = 7

// Metadata reserve: DRBD's internal metadata lives inside the backing device, so
// the LV is sized up to leave the requested usable space. Generous on purpose.
const (
	mdBase   = 1 << 20
	mdPerGiB = 384 << 10
	gib      = 1 << 30
)

// Action names one change a pass made; a pass that changed nothing has none.
type Action string

const (
	CreateLV    Action = "create-lv"
	ExtendLV    Action = "extend-lv"
	WriteConfig Action = "write-config"
	CreateMD    Action = "create-md"
	Up          Action = "up"
	Adjust      Action = "adjust"
	Resize      Action = "resize"
)

// Result reports a pass. Forgot lists retired node-ids this node has forgotten;
// the caller acknowledges them to the allocator.
type Result struct {
	Actions []Action
	Forgot  []int
}

// Changed reports whether the pass altered anything.
func (r Result) Changed() bool { return len(r.Actions) > 0 }

// Runtime converges local LVM and DRBD state. It never changes a role: promotion
// belongs to the lease-gated logic. Calls are serialised.
type Runtime struct {
	LVM           lvm.LVM
	DRBD          drbd.DRBD
	VG, Pool      string
	ConfigDir     string
	SplitBrainCmd string
	// Diverged reports a resource the kernel dropped for split-brain, which must not be
	// reconnected; nil means none is.
	Diverged func(name string) (bool, error)
	// Copy overwrites dst with the first n bytes of src; nil means copyDevice.
	Copy func(ctx context.Context, src, dst string, n uint64) error
	mu   sync.Mutex
}

func (r *Runtime) configPath(name string) string { return filepath.Join(r.ConfigDir, name+".res") }

func backingSize(size uint64) uint64 {
	return size + mdBase + (size+gib-1)/gib*mdPerGiB
}

// Reconcile drives the volume to the desired state and is safe to repeat. On
// error it returns the actions taken so far.
func (r *Runtime) Reconcile(ctx context.Context, d Desired) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &pass{Runtime: r, d: d}
	if err := p.prepare(); err != nil {
		return Result{}, err
	}
	steps := []func(context.Context) error{p.backing, p.config, p.bringUp, p.grow, p.forget}
	if d.Tiebreaker() {
		steps = []func(context.Context) error{p.config, p.bringUp} // no data: no LV, metadata or size
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return p.res, err
		}
	}
	return p.res, nil
}

type pass struct {
	*Runtime
	d       Desired
	cfg     string
	created bool // the backing LV was created by this pass
	res     Result
}

func (p *pass) did(a Action) { p.res.Actions = append(p.res.Actions, a) }

// prepare validates the desired state and renders the config before anything is touched.
func (p *pass) prepare() error {
	if err := p.d.validate(); err != nil {
		return err
	}
	if p.d.Thin && p.Pool == "" {
		return experrors.New(experrors.KindInvalid, "volume.Reconcile", "thin volume requested but no thin pool configured")
	}
	cfg, err := drbd.Resource{
		Name: p.d.Name, Minor: p.d.Minor, Port: p.d.Port, Disk: lvm.DevicePath(p.VG, p.d.Name),
		SplitBrainCmd: p.SplitBrainCmd, Members: p.d.Members,
	}.Render()
	p.cfg = cfg
	return err
}

func (p *pass) backing(ctx context.Context) error {
	_, err := p.LVM.Get(ctx, p.VG, p.d.Name)
	if experrors.KindOf(err) != experrors.KindNotFound {
		return err
	}
	size := backingSize(p.d.SizeBytes)
	if p.d.Thin {
		err = p.LVM.CreateThin(ctx, p.VG, p.Pool, p.d.Name, size)
	} else {
		err = p.LVM.CreateThick(ctx, p.VG, p.d.Name, size)
	}
	if err != nil {
		return err
	}
	p.created = true
	p.did(CreateLV)
	return nil
}

func (p *pass) config(context.Context) error {
	path := p.configPath(p.d.Name)
	if have, err := os.ReadFile(path); err == nil && string(have) == p.cfg {
		return nil
	}
	if err := os.MkdirAll(p.ConfigDir, 0o750); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "volume.config", "create "+p.ConfigDir)
	}
	if err := writeAtomic(path, p.cfg); err != nil {
		return experrors.Wrap(err, experrors.KindInternal, "volume.config", "write "+path)
	}
	p.did(WriteConfig)
	return nil
}

// writeAtomic stages the body beside the target and renames it into place, so a
// crash leaves either the old file or the new one, never a torn mix.
func writeAtomic(path, body string) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = tmp.WriteString(body); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// bringUp starts a resource that is down, or adjusts one whose kernel state drifted.
func (p *pass) bringUp(ctx context.Context) error {
	_, err := p.DRBD.Status(ctx, p.d.Name)
	switch {
	case err == nil:
		return p.adjustIfDrifted(ctx)
	case experrors.KindOf(err) == experrors.KindNotFound:
		return p.start(ctx)
	}
	return err
}

func (p *pass) adjustIfDrifted(ctx context.Context) error {
	pending, err := p.DRBD.AdjustPending(ctx, p.d.Name)
	if err != nil || !pending {
		return err
	}
	// The kernel writes the mark before it drops the connection, so a drop seen here is marked.
	if held, err := p.diverged(); err != nil || held {
		return err
	}
	if err := p.DRBD.Adjust(ctx, p.d.Name); err != nil {
		return err
	}
	p.did(Adjust)
	return nil
}

func (p *pass) diverged() (bool, error) {
	if p.Diverged == nil {
		return false, nil
	}
	return p.Diverged(p.d.Name)
}

func (p *pass) start(ctx context.Context) error {
	if !p.d.Tiebreaker() {
		if err := p.ensureMetadata(ctx); err != nil {
			return err
		}
	}
	if err := p.DRBD.Up(ctx, p.d.Name); err != nil {
		return err
	}
	p.did(Up)
	return nil
}

// ensureMetadata writes fresh metadata on a new LV (whatever its recycled extents
// hold) and on any LV DRBD positively reports as blank. Doubt is an error.
func (p *pass) ensureMetadata(ctx context.Context) error {
	if !p.created {
		has, err := p.DRBD.HasMetadata(ctx, p.d.Name)
		if err != nil || has {
			return err
		}
	}
	if err := p.DRBD.CreateMD(ctx, p.d.Name, maxPeers); err != nil {
		return err
	}
	p.did(CreateMD)
	return nil
}

// grow extends the LV once the resource is up (DRBD relocates its internal
// metadata online), then resizes DRBD until its usable size covers the request.
// Shrinking is never attempted.
func (p *pass) grow(ctx context.Context) error {
	lv, err := p.LVM.Get(ctx, p.VG, p.d.Name)
	if err != nil {
		return err
	}
	if want := backingSize(p.d.SizeBytes); lv.Size < want {
		if err := p.LVM.Extend(ctx, p.VG, p.d.Name, want); err != nil {
			return err
		}
		p.did(ExtendLV)
	}
	st, err := p.DRBD.Status(ctx, p.d.Name)
	if err != nil {
		return err
	}
	if len(st.Volumes) == 0 || st.Volumes[0].SizeKiB*1024 >= p.d.SizeBytes {
		return nil
	}
	if err := p.DRBD.Resize(ctx, p.d.Name); err != nil {
		return err
	}
	p.did(Resize)
	return nil
}

// forget drops retired node-ids. It runs after the dead peer's connection is gone
// (adjust) and before any replacement joins, which the controller sequences.
func (p *pass) forget(ctx context.Context) error {
	for _, id := range p.d.Retired {
		if err := p.DRBD.ForgetPeer(ctx, p.d.Name, id); err != nil {
			return err
		}
		p.res.Forgot = append(p.res.Forgot, id)
	}
	return nil
}

// Present reports whether the volume's backing LV or, for a tiebreaker, its config exists on this node.
func (r *Runtime) Present(ctx context.Context, name string) (bool, error) {
	if has, err := r.hasLV(ctx, name); err != nil || has {
		return has, err
	}
	_, err := os.Stat(r.configPath(name))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (r *Runtime) hasLV(ctx context.Context, name string) (bool, error) {
	_, err := r.LVM.Get(ctx, r.VG, name)
	if experrors.KindOf(err) == experrors.KindNotFound {
		return false, nil
	}
	return err == nil, err
}

// Remove takes a volume off this node: down, then its config, then its snapshots
// and the backing LV. It is safe to repeat and stops at the first failure, so the next call resumes.
// The caller must have demoted the volume first.
func (r *Runtime) Remove(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.DRBD.Status(ctx, name); err == nil {
		if err := r.DRBD.Down(ctx, name); err != nil {
			return err
		}
	} else if experrors.KindOf(err) != experrors.KindNotFound {
		return err
	}
	if err := os.Remove(r.configPath(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return experrors.Wrap(err, experrors.KindInternal, "volume.Remove", "remove config of "+name)
	}
	if ok, err := r.hasLV(ctx, name); err != nil || !ok {
		return err
	}
	if err := r.removeSnapshots(ctx, name); err != nil {
		return err
	}
	return r.LVM.Remove(ctx, r.VG, name)
}

// Rejoin reconnects a replica the kernel dropped after a split-brain. With discard
// this side's changes are thrown away in favour of its peers'; without, it is the
// side that keeps its data. It never adjusts (that would reconnect without the
// discard flag) and never creates the backing device: a missing one is an error.
func (r *Runtime) Rejoin(ctx context.Context, d Desired, discard bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &pass{Runtime: r, d: d}
	if err := p.prepare(); err != nil {
		return err
	}
	if _, err := r.LVM.Get(ctx, r.VG, d.Name); err != nil {
		return err
	}
	if err := p.config(ctx); err != nil {
		return err
	}
	st, err := r.DRBD.Status(ctx, d.Name)
	if experrors.KindOf(err) == experrors.KindNotFound {
		if err = p.start(ctx); err == nil {
			st, err = r.DRBD.Status(ctx, d.Name) // starting connects it, so its peers are Connecting now
		}
	}
	if err != nil {
		return err
	}
	return p.reconnect(ctx, st, discard)
}

func (p *pass) reconnect(ctx context.Context, st *drbd.Status, discard bool) error {
	if discard {
		if slices.ContainsFunc(st.Peers, func(pr drbd.Peer) bool { return pr.Connection == drbd.ConnConnecting }) {
			if err := p.DRBD.Disconnect(ctx, p.d.Name); err != nil {
				return err
			}
		}
		return p.DRBD.ConnectDiscarding(ctx, p.d.Name)
	}
	if !slices.ContainsFunc(st.Peers, func(pr drbd.Peer) bool { return pr.Connection == drbd.ConnStandAlone }) {
		return nil
	}
	return p.DRBD.Connect(ctx, p.d.Name)
}
