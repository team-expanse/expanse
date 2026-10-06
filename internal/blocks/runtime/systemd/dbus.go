package systemd

// Production unitAPI over systemd D-Bus (T20.5b). Tests inject fakes;
// this is the real thing, registered by the agent.

import (
	"context"

	"github.com/coreos/go-systemd/v22/dbus"

	"github.com/expanse/expanse/internal/errors"
)

// DBUSAPI controls units via the system bus.
type DBUSAPI struct {
	// Conn lazily established; one connection per agent is plenty.
	conn *dbus.Conn
}

// NewDBUSAPI connects to the system bus.
func NewDBUSAPI(ctx context.Context) (*DBUSAPI, error) {
	conn, err := dbus.NewSystemConnectionContext(ctx)
	if err != nil {
		return nil, errors.Wrap(err, errors.KindUnavailable, "systemd.NewDBUSAPI", "system bus")
	}
	return &DBUSAPI{conn: conn}, nil
}

// Close releases the bus connection.
func (d *DBUSAPI) Close() { d.conn.Close() }

// UnitState reads load/active/sub for a unit (unknown triple when the
// unit is not loaded — callers see load="not-found").
func (d *DBUSAPI) UnitState(ctx context.Context, unit string) (load, active, sub string, err error) {
	props, err := d.conn.GetUnitPropertiesContext(ctx, unit)
	if err != nil {
		return "not-found", "inactive", "dead", nil //nolint:nilerr // an unloaded unit, as documented
	}
	return strProp(props["LoadState"]), strProp(props["ActiveState"]), strProp(props["SubState"]), nil
}

// StatusText reads the service's sd_notify STATUS= text.
func (d *DBUSAPI) StatusText(ctx context.Context, unit string) (string, error) {
	p, err := d.conn.GetUnitTypePropertyContext(ctx, unit, "Service", "StatusText")
	if err != nil {
		return "", errors.Wrap(err, errors.KindUnavailable, "systemd.StatusText", unit)
	}
	text, _ := p.Value.Value().(string)
	return text, nil
}

// Start starts (or restarts, via replace) the unit and waits for the job.
//
// Clears a tripped StartLimitBurst first (PHASE-05-TASKS.md Stream A,
// X1: found via db/postgres, whose replicas race ahead of their own
// per-replica volume with no P12-style placement gate the way SINGLETON/
// DAEMONSET get — a few sub-second "not ready yet" failures exhaust the
// unit's own StartLimitBurst=3/60s (systemd.go's UnitFile) long before a
// multi-volume DRBD/LVM chain converges). Once tripped, systemd refuses
// EVERY further start, including this reconciler's own explicit ones, so
// the "keep retrying until the dependency is ready" behavior every block
// type's Restart=on-failure is meant to provide silently stops working.
// Best-effort and harmless when the unit isn't in that state at all.
func (d *DBUSAPI) Start(ctx context.Context, unit string) error {
	_ = d.conn.ResetFailedUnitContext(ctx, unit)
	ch := make(chan string)
	if _, err := d.conn.RestartUnitContext(ctx, unit, "replace", ch); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "systemd.Start", unit)
	}
	select {
	case res := <-ch:
		if res != "done" {
			return errors.New(errors.KindUnavailable, "systemd.Start",
				unit+": job "+res)
		}
		return nil
	case <-ctx.Done():
		return errors.New(errors.KindTimeout, "systemd.Start", unit+": timed out")
	}
}

// Stop stops the unit and waits for the job.
func (d *DBUSAPI) Stop(ctx context.Context, unit string) error {
	ch := make(chan string)
	if _, err := d.conn.StopUnitContext(ctx, unit, "replace", ch); err != nil {
		return errors.Wrap(err, errors.KindUnavailable, "systemd.Stop", unit)
	}
	select {
	case res := <-ch:
		if res != "done" {
			return errors.New(errors.KindUnavailable, "systemd.Stop",
				unit+": job "+res)
		}
		return nil
	case <-ctx.Done():
		return errors.New(errors.KindTimeout, "systemd.Stop", unit+": timed out")
	}
}

func strProp(v any) string {
	s, _ := v.(string)
	return s
}
