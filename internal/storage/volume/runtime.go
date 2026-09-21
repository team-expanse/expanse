package volume

import (
	"context"
	"os"
	"path/filepath"
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
	mu            sync.Mutex
}

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
	for _, step := range []func(context.Context) error{p.backing, p.config, p.bringUp, p.grow, p.forget} {
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
	path := filepath.Join(p.ConfigDir, p.d.Name+".res")
	if have, err := os.ReadFile(path); err == nil && string(have) == p.cfg {
		return nil
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
	if err := p.DRBD.Adjust(ctx, p.d.Name); err != nil {
		return err
	}
	p.did(Adjust)
	return nil
}

func (p *pass) start(ctx context.Context) error {
	if err := p.ensureMetadata(ctx); err != nil {
		return err
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
