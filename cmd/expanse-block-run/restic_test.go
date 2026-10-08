package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	pb "github.com/expanse/expanse/proto"
	pbproto "google.golang.org/protobuf/proto"
)

func resticCfg(kv ...any) map[string]any {
	m := map[string]any{
		"repository": "s3:http://10.0.0.9:3900/backups", "password": "repo-pass",
		"volumes": []any{"forge/forgejo-data"},
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(m, kv[i].(string))
		} else {
			m[kv[i].(string)] = kv[i+1]
		}
	}
	return m
}

func TestResticSetupAppliesDefaults(t *testing.T) {
	s, err := resticSetupFrom("prod", resticCfg("volumes", []any{"forge/forgejo-data", "scratch"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"blk-prod-forge-forgejo-data", "scratch"}; !slices.Equal(s.Volumes, want) {
		t.Errorf("volumes = %v, want %v", s.Volumes, want)
	}
	if s.Interval != 24*time.Hour || s.Port != "18900" {
		t.Errorf("interval = %v, port = %s", s.Interval, s.Port)
	}
	if want := []string{"--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6"}; !slices.Equal(s.Keep, want) {
		t.Errorf("keep = %v, want %v", s.Keep, want)
	}
}

func TestResticSetupKeepOverridesDefaults(t *testing.T) {
	s, err := resticSetupFrom("default", resticCfg("keep", map[string]any{"last": 3.0, "yearly": 2.0}))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--keep-last", "3", "--keep-yearly", "2"}; !slices.Equal(s.Keep, want) {
		t.Errorf("keep = %v, want %v", s.Keep, want)
	}
}

func TestResticSetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		resticCfg("repository", nil),
		resticCfg("repository", "s3:http://x/\nb"),
		resticCfg("password", nil),
		resticCfg("volumes", nil),
		resticCfg("volumes", []any{}),
		resticCfg("volumes", []any{"a/b/c"}),
		resticCfg("volumes", []any{"Bad Name"}),
		resticCfg("volumes", []any{"x", "x"}),
		resticCfg("interval", "30s"),
		resticCfg("interval", "daily"),
		resticCfg("keep", map[string]any{"daily": 0.0}),
		resticCfg("keep", map[string]any{"hourly": -1.0}),
		resticCfg("keep", map[string]any{"forever": 1.0}),
		resticCfg("env", map[string]any{"bad name": "v"}),
		resticCfg("env", map[string]any{"RESTIC_PASSWORD": "v"}),
		resticCfg("env", map[string]any{"AWS_ACCESS_KEY_ID": "a\nb"}),
	} {
		if _, err := resticSetupFrom("default", cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

// The password goes in the environment, never argv, so it stays out of the process list.
func TestResticEnvCarriesRepositoryAndSecrets(t *testing.T) {
	s, err := resticSetupFrom("default", resticCfg("env", map[string]any{"AWS_ACCESS_KEY_ID": "GK1"}))
	if err != nil {
		t.Fatal(err)
	}
	env := resticEnv(s)
	for _, want := range []string{
		"RESTIC_REPOSITORY=s3:http://10.0.0.9:3900/backups", "RESTIC_PASSWORD=repo-pass", "AWS_ACCESS_KEY_ID=GK1",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env lacks %s", want)
		}
	}
}

func TestBackupDue(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		last time.Time
		want bool
	}{
		{time.Time{}, true},
		{now.Add(-25 * time.Hour), true},
		{now.Add(-24 * time.Hour), true},
		{now.Add(-time.Hour), false},
	} {
		if got := backupDue(tc.last, now, 24*time.Hour); got != tc.want {
			t.Errorf("last %v: due = %v, want %v", tc.last, got, tc.want)
		}
	}
}

func TestLatestSnapshotTime(t *testing.T) {
	got, err := latestSnapshotTime(`[{"time":"2026-10-08T11:00:00.5+00:00","hostname":"v"}]`)
	if err != nil || !got.Equal(time.Date(2026, 10, 8, 11, 0, 0, 5e8, time.UTC)) {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err := latestSnapshotTime("[]"); err != nil || !got.IsZero() {
		t.Errorf("none: got %v, %v", got, err)
	}
	if _, err := latestSnapshotTime("not json"); err == nil {
		t.Error("garbage accepted")
	}
}

// fakeVolumeHost records what a backup does to the node and to restic.
type fakeVolumeHost struct {
	calls   []string
	failOn  string
	primary bool
}

func (f *fakeVolumeHost) record(c string) error {
	f.calls = append(f.calls, c)
	if c == f.failOn {
		return errors.New(c + " failed")
	}
	return nil
}

func (f *fakeVolumeHost) IsPrimary(_ context.Context, id string) (bool, error) {
	return f.primary, f.record("primary " + id)
}

func (f *fakeVolumeHost) Snapshot(_ context.Context, id, snap string) (string, int64, error) {
	return "/dev/vg0/" + snap, 4096, f.record("snapshot " + id + " " + snap)
}

func (f *fakeVolumeHost) DropSnapshot(_ context.Context, id, snap string) error {
	return f.record("drop " + id + " " + snap)
}

func (f *fakeVolumeHost) Restic(_ context.Context, args ...string) (string, error) {
	return "[]", f.record("restic " + strings.Join(args, " "))
}

// Only the DRBD device's bytes are read: the LV also holds DRBD's metadata past them.
func TestBackupVolumeSnapshotsStreamsAndPrunes(t *testing.T) {
	s, _ := resticSetupFrom("default", resticCfg("keep", map[string]any{"last": 2.0}))
	f := &fakeVolumeHost{}
	if err := backupVolume(context.Background(), s, f, "blk-default-forge-forgejo-data", "vol-ab"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"drop vol-ab restic",
		"snapshot vol-ab restic",
		"restic backup --host blk-default-forge-forgejo-data --tag expanse --stdin-filename blk-default-forge-forgejo-data.img --stdin-from-command -- head -c 4096 /dev/vg0/restic",
		"drop vol-ab restic",
		"restic forget --host blk-default-forge-forgejo-data --group-by host --prune --keep-last 2",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
}

// A failed backup still drops its snapshot, so thin-pool space is never held between runs.
func TestBackupVolumeDropsTheSnapshotOnFailure(t *testing.T) {
	s, _ := resticSetupFrom("default", resticCfg())
	f := &fakeVolumeHost{failOn: "restic backup --host v --tag expanse --stdin-filename v.img --stdin-from-command -- head -c 4096 /dev/vg0/restic"}
	if err := backupVolume(context.Background(), s, f, "v", "vol-ab"); err == nil {
		t.Fatal("backup failure not reported")
	}
	if last := f.calls[len(f.calls)-1]; last != "drop vol-ab restic" {
		t.Errorf("last call %q, want the snapshot dropped", last)
	}
}

func TestVolumeIDsFromStoreEntries(t *testing.T) {
	spec := func(name string) []byte {
		b, _ := pbproto.Marshal(&pb.VolumeSpec{Name: name})
		return b
	}
	got := volumeIDsFrom([]*pb.KeyValueEntry{
		{Key: "/volumes/vol-a1/spec", Value: spec("blk-default-forge-forgejo-data")},
		{Key: "/volumes/vol-a1/status", Value: []byte("x")},
		{Key: "/volumes/vol-b2/spec", Value: spec("scratch")},
		{Key: "/volumes/_ops/snapshot/vol-a1", Value: []byte("{}")},
		{Key: "/volumes/vol-c3/snapshots/s1", Value: []byte("{}")},
	})
	if len(got) != 2 || got["blk-default-forge-forgejo-data"] != "vol-a1" || got["scratch"] != "vol-b2" {
		t.Errorf("got %v", got)
	}
}

func TestResticStatusReportsFailuresOnLocalVolumes(t *testing.T) {
	st := newResticStatus()
	st.set("a", volumeStatus{Primary: true, LastBackup: time.Unix(100, 0).UTC()})
	st.set("b", volumeStatus{Primary: false, LastError: "old failure on another node"})
	rec := httptest.NewRecorder()
	st.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthy: code %d", rec.Code)
	}
	st.set("a", volumeStatus{Primary: true, LastError: "restic: connection refused"})
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("failing: code %d body %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	st.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `"a":{"primary":true`) {
		t.Errorf("status body %q", rec.Body.String())
	}
}
