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
		return "not-found", "inactive", "dead", nil
	}
	return strProp(props["LoadState"]), strProp(props["ActiveState"]), strProp(props["SubState"]), nil
}

// Start starts (or restarts, via replace) the unit and waits for the job.
func (d *DBUSAPI) Start(ctx context.Context, unit string) error {
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
