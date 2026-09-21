package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/volume"
)

// fileLease stands in for the Raft lease: it is held while its file exists, so a
// VM test can revoke it with rm. The real lease is covered by unit tests.
type fileLease struct {
	done chan struct{}
	path string
}

func newFileLease(ctx context.Context, path string) (*fileLease, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("lease file: %w", err)
	}
	l := &fileLease{done: make(chan struct{}), path: path}
	go func() {
		defer close(l.done)
		for ctx.Err() == nil && l.Valid() {
			time.Sleep(50 * time.Millisecond)
		}
	}()
	return l, nil
}

func (l *fileLease) Done() <-chan struct{} { return l.done }

func (l *fileLease) Valid() bool {
	_, err := os.Stat(l.path)
	return err == nil
}

// umountConsumer releases a device by unmounting the directory it is mounted on.
type umountConsumer struct{ dir string }

func (c umountConsumer) Release(ctx context.Context, _ string) error {
	if !mounted(ctx, c.dir) {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "umount", c.dir).CombinedOutput(); err != nil {
		return fmt.Errorf("umount %s: %w: %s", c.dir, err, out)
	}
	return nil
}

func mounted(ctx context.Context, dir string) bool {
	return exec.CommandContext(ctx, "mountpoint", "-q", dir).Run() == nil
}

// hold runs the promoter for one resource until SIGTERM or the lease file goes.
func hold(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("hold", flag.ContinueOnError)
	name := fs.String("name", "", "resource name")
	leasePath := fs.String("lease-file", "", "the volume is leased while this file exists")
	mountDir := fs.String("mount-dir", "", "directory to unmount before every demotion")
	initial := fs.Bool("initial", false, "the volume has never been primary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	lease, err := newFileLease(ctx, *leasePath)
	if err != nil {
		return err
	}
	p := &volume.Promoter{
		DRBD: drbd.New(), Poll: 200 * time.Millisecond, Settled: 500 * time.Millisecond,
		StepDownTimeout: 20 * time.Second,
		Log:             slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	if *mountDir != "" {
		p.Consumer = umountConsumer{dir: *mountDir}
	}
	err = p.Hold(ctx, *name, lease, volume.HoldOptions{
		Initial:   *initial,
		OnPrimary: func() { fmt.Fprintln(out, "primary") },
	})
	fmt.Fprintln(out, "stepped down")
	return err
}
