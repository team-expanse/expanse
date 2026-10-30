package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// errPGDemoted means pgha handed the primary lease to another replica while this one was down.
var errPGDemoted = errors.New("the primary lease moved to another replica")

// pgLeaseWaitTimeout bounds a restarted primary's wait for its lease; the unit then restarts.
const pgLeaseWaitTimeout = 5 * time.Minute

// waitForPrimaryLease holds a restarted primary until pgha marks the lease held again
// (pgha.HeldFile, duplicated as "held"), so it never takes writes without the lease.
func waitForPrimaryLease(ctx context.Context, stateDir string, timeout, interval time.Duration) error {
	held := filepath.Join(stateDir, "held")
	if err := os.Remove(held); err != nil && !os.IsNotExist(err) {
		return err
	}
	deadline := time.Now().Add(timeout)
	for {
		if content, err := os.ReadFile(filepath.Join(stateDir, "role")); err == nil && roleFileSaysReplica(string(content)) {
			return errPGDemoted
		}
		if _, err := os.Stat(held); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("this node did not get the primary lease back within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

// awaitPrimaryLease waits for the lease and wipes a demoted primary's diverged pgdata.
func awaitPrimaryLease(ctx context.Context, stateDir, pgdata string, timeout, interval time.Duration) error {
	fmt.Println("expanse-block-run: postgres waits for this node to hold the primary lease")
	err := waitForPrimaryLease(ctx, stateDir, timeout, interval)
	if errors.Is(err, errPGDemoted) {
		if rerr := os.RemoveAll(pgdata); rerr != nil {
			return fmt.Errorf("%w; wiping pgdata: %w", err, rerr)
		}
		return fmt.Errorf("%w, restarting to re-clone from it", err)
	}
	return err
}
