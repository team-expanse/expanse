package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	experrors "github.com/expanse/expanse/internal/errors"
	expstorage "github.com/expanse/expanse/internal/storage"
	"github.com/expanse/expanse/internal/storage/drbd"
	"github.com/expanse/expanse/internal/storage/lvm"
	pb "github.com/expanse/expanse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pbproto "google.golang.org/protobuf/proto"
)

// Mirrors nix/blocks/util/restic-backup/schema.json.
var (
	resticName    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	resticEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	resticLine    = regexp.MustCompile(`^[^\n\r]+$`)
	resticKeeps   = []string{"last", "hourly", "daily", "weekly", "monthly", "yearly"}
	// The block sets these itself; letting env override them would split one repository's settings in two.
	resticReserved = []string{"RESTIC_REPOSITORY", "RESTIC_REPOSITORY_FILE", "RESTIC_PASSWORD", "RESTIC_PASSWORD_FILE", "RESTIC_PASSWORD_COMMAND"}
)

const (
	resticSnapshot = "restic" // the LV is <volume id>-snap-restic, so deleting the volume removes it too
	resticTick     = 30 * time.Second
	resticRetry    = time.Minute
	agentSocket    = "/run/expanse/agent.sock"
)

// resticSetup is everything one util/restic-backup replica needs.
type resticSetup struct {
	Repository, Password, Port string
	Env                        map[string]string
	Volumes                    []string // cluster volume names
	Interval                   time.Duration
	Keep                       []string // restic forget --keep-* flags
}

// resticSetupFrom applies the block's defaults to spec.config and validates it.
func resticSetupFrom(namespace string, cfg map[string]any) (resticSetup, error) {
	s := resticSetup{
		Repository: cfgStr(cfg, "repository"),
		Password:   cfgStr(cfg, "password"),
		Port:       cfgPortOr(cfg, "18900"),
		Env:        map[string]string{},
	}
	if !resticLine.MatchString(s.Repository) {
		return resticSetup{}, errors.New("util/restic-backup: repository is required, on one line")
	}
	if !resticLine.MatchString(s.Password) {
		return resticSetup{}, errors.New("util/restic-backup: password is required, on one line")
	}
	var err error
	if s.Volumes, err = resticVolumesFrom(namespace, cfg["volumes"]); err != nil {
		return resticSetup{}, err
	}
	if s.Interval, err = resticIntervalFrom(cfg["interval"]); err != nil {
		return resticSetup{}, err
	}
	if s.Keep, err = resticKeepFrom(cfg["keep"]); err != nil {
		return resticSetup{}, err
	}
	env, _ := cfg["env"].(map[string]any)
	for k, v := range env {
		val, ok := v.(string)
		if !resticEnvName.MatchString(k) || slices.Contains(resticReserved, k) || !ok || strings.ContainsAny(val, "\n\r\x00") {
			return resticSetup{}, fmt.Errorf("util/restic-backup: env %q is not a settable variable", k)
		}
		s.Env[k] = val
	}
	return s, nil
}

// resticVolumesFrom resolves "block/storage" entries to the volume a block's storage entry gets.
func resticVolumesFrom(namespace string, raw any) ([]string, error) {
	list, _ := raw.([]any)
	if len(list) == 0 {
		return nil, errors.New("util/restic-backup: volumes must name at least one volume")
	}
	var out []string
	for _, v := range list {
		ref, _ := v.(string)
		parts := strings.Split(ref, "/")
		for _, p := range parts {
			if len(parts) > 2 || !resticName.MatchString(p) {
				return nil, fmt.Errorf("util/restic-backup: volume %q must be <block>/<storage> or a volume name", ref)
			}
		}
		name := ref
		if len(parts) == 2 {
			name = expstorage.BlockVolumeName(namespace, parts[0], parts[1])
		}
		if slices.Contains(out, name) {
			return nil, fmt.Errorf("util/restic-backup: volume %q is listed twice", ref)
		}
		out = append(out, name)
	}
	return out, nil
}

func resticIntervalFrom(raw any) (time.Duration, error) {
	if raw == nil {
		return 24 * time.Hour, nil
	}
	str, _ := raw.(string)
	d, err := time.ParseDuration(str)
	if err != nil || d < time.Minute {
		return 0, fmt.Errorf("util/restic-backup: interval %v must be a duration of at least 1m, such as 6h", raw)
	}
	return d, nil
}

func resticKeepFrom(raw any) ([]string, error) {
	keep := map[string]any{"daily": 7.0, "weekly": 4.0, "monthly": 6.0}
	if raw != nil {
		keep, _ = raw.(map[string]any)
	}
	var out []string
	for k := range keep {
		if !slices.Contains(resticKeeps, k) {
			return nil, fmt.Errorf("util/restic-backup: keep.%s is not one of %v", k, resticKeeps)
		}
	}
	for _, k := range resticKeeps {
		v, ok := keep[k]
		if !ok {
			continue
		}
		n, isNum := v.(float64)
		if !isNum || n < 1 || n != float64(int(n)) {
			return nil, fmt.Errorf("util/restic-backup: keep.%s must be a whole number of at least 1", k)
		}
		out = append(out, "--keep-"+k, strconv.Itoa(int(n)))
	}
	if len(out) == 0 {
		return nil, errors.New("util/restic-backup: keep must keep something")
	}
	return out, nil
}

// resticEnv is restic's environment: the repository, its password and any backend credentials.
func resticEnv(s resticSetup) []string {
	env := append(os.Environ(), "RESTIC_REPOSITORY="+s.Repository, "RESTIC_PASSWORD="+s.Password)
	for k, v := range s.Env {
		env = append(env, k+"="+v)
	}
	return env
}

func backupDue(last, now time.Time, interval time.Duration) bool {
	return last.IsZero() || now.Sub(last) >= interval
}

// latestSnapshotTime reads `restic snapshots --json --latest 1`; zero means none yet.
func latestSnapshotTime(out string) (time.Time, error) {
	var snaps []struct {
		Time time.Time `json:"time"`
	}
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		return time.Time{}, fmt.Errorf("parse restic snapshots: %w", err)
	}
	var last time.Time
	for _, s := range snaps {
		if s.Time.After(last) {
			last = s.Time
		}
	}
	return last, nil
}

// volumeHost is what a backup needs from the node and from restic.
type volumeHost interface {
	IsPrimary(ctx context.Context, id string) (bool, error)
	// Snapshot freezes the volume, returning the snapshot's device and the size of the volume's data in it.
	Snapshot(ctx context.Context, id, snap string) (string, int64, error)
	DropSnapshot(ctx context.Context, id, snap string) error
	Restic(ctx context.Context, args ...string) (string, error)
}

// backupVolume streams a crash-consistent snapshot of one volume into the repository.
func backupVolume(ctx context.Context, s resticSetup, h volumeHost, name, id string) error {
	if err := h.DropSnapshot(ctx, id, resticSnapshot); err != nil {
		return fmt.Errorf("drop stale snapshot: %w", err)
	}
	dev, size, err := h.Snapshot(ctx, id, resticSnapshot)
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	// The image is the DRBD device's contents, so it can be written straight back to /dev/drbdN.
	_, err = h.Restic(ctx, "backup", "--host", name, "--tag", "expanse", "--stdin-filename", name+".img",
		"--stdin-from-command", "--", "head", "-c", strconv.FormatInt(size, 10), dev)
	if dropErr := h.DropSnapshot(ctx, id, resticSnapshot); err == nil && dropErr != nil {
		err = fmt.Errorf("drop snapshot: %w", dropErr)
	}
	if err != nil {
		return err
	}
	_, err = h.Restic(ctx, append([]string{"forget", "--host", name, "--group-by", "host", "--prune"}, s.Keep...)...)
	return err
}

// volumeIDsFrom maps volume names to IDs from the agent's /volumes/ store entries.
func volumeIDsFrom(entries []*pb.KeyValueEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(strings.TrimPrefix(e.GetKey(), "/volumes/"), "/spec")
		if !ok || !strings.HasPrefix(id, "vol-") || strings.Contains(id, "/") {
			continue
		}
		var spec pb.VolumeSpec
		if pbproto.Unmarshal(e.GetValue(), &spec) == nil && spec.GetName() != "" {
			out[spec.GetName()] = id
		}
	}
	return out
}

// volumeStatus is one volume's backup state as this replica last saw it.
type volumeStatus struct {
	Primary     bool      `json:"primary"`
	LastBackup  time.Time `json:"lastBackup,omitzero"`
	LastAttempt time.Time `json:"lastAttempt,omitzero"`
	LastError   string    `json:"lastError,omitempty"`
}

// resticStatus serves the replica's view: / as JSON, /healthz failing while a local volume's backup fails.
type resticStatus struct {
	mu   sync.Mutex
	vols map[string]volumeStatus
}

func newResticStatus() *resticStatus { return &resticStatus{vols: map[string]volumeStatus{}} }

func (r *resticStatus) set(name string, v volumeStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vols[name] = v
}

func (r *resticStatus) get(name string) volumeStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.vols[name]
}

func (r *resticStatus) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.Path == "/healthz" {
		var failing []string
		for name, v := range r.vols {
			if v.Primary && v.LastError != "" {
				failing = append(failing, name+": "+v.LastError)
			}
		}
		if len(failing) > 0 {
			slices.Sort(failing)
			http.Error(w, strings.Join(failing, "\n"), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(r.vols)
}

// resticRunner backs up, on each tick, the listed volumes whose primary is on this node.
type resticRunner struct {
	s         resticSetup
	host      volumeHost
	volumeIDs func(ctx context.Context) (map[string]string, error)
	status    *resticStatus
	repoReady bool
}

func (r *resticRunner) tick(ctx context.Context) {
	ids, err := r.volumeIDs(ctx)
	if err != nil {
		log.Printf("restic-backup: list volumes: %v", err)
		return
	}
	for _, name := range r.s.Volumes {
		prev := r.status.get(name)
		id, ok := ids[name]
		if !ok {
			r.status.set(name, volumeStatus{LastBackup: prev.LastBackup, LastError: "no such volume"})
			continue
		}
		primary, err := r.host.IsPrimary(ctx, id)
		if err != nil || !primary {
			r.status.set(name, volumeStatus{LastBackup: prev.LastBackup})
			continue
		}
		if prev.LastError != "" && time.Since(prev.LastAttempt) < resticRetry {
			continue
		}
		next := volumeStatus{Primary: true, LastBackup: prev.LastBackup, LastAttempt: time.Now()}
		if err := r.backupIfDue(ctx, name, id, &next); err != nil {
			next.LastError = err.Error()
			log.Printf("restic-backup: %s: %v", name, err)
		}
		r.status.set(name, next)
	}
}

func (r *resticRunner) backupIfDue(ctx context.Context, name, id string, st *volumeStatus) error {
	if err := r.ensureRepo(ctx); err != nil {
		return err
	}
	out, err := r.host.Restic(ctx, "snapshots", "--json", "--host", name, "--latest", "1")
	if err != nil {
		return err
	}
	last, err := latestSnapshotTime(out)
	if err != nil {
		return err
	}
	st.LastBackup = last
	if !backupDue(last, time.Now(), r.s.Interval) {
		return nil
	}
	log.Printf("restic-backup: backing up %s (%s)", name, id)
	if err := backupVolume(ctx, r.s, r.host, name, id); err != nil {
		return err
	}
	st.LastBackup = time.Now().UTC()
	log.Printf("restic-backup: backed up %s", name)
	return nil
}

// ensureRepo initialises the repository the first time; restic exits 10 when there is none.
func (r *resticRunner) ensureRepo(ctx context.Context) error {
	if r.repoReady {
		return nil
	}
	_, err := r.host.Restic(ctx, "cat", "config")
	if resticExitCode(err) == 10 {
		if _, initErr := r.host.Restic(ctx, "init"); initErr != nil {
			// Another node may have initialised it first.
			_, err = r.host.Restic(ctx, "cat", "config")
		} else {
			err = nil
		}
	}
	r.repoReady = err == nil
	return err
}

func resticExitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 0
}

// nodeVolumeHost is volumeHost on the real node: DRBD, LVM and the restic binary.
type nodeVolumeHost struct {
	env []string
	dr  *drbd.Exec
	lv  *lvm.Exec
	bin string
}

func (h nodeVolumeHost) IsPrimary(ctx context.Context, id string) (bool, error) {
	st, err := h.dr.Status(ctx, id)
	if experrors.KindOf(err) == experrors.KindNotFound {
		return false, nil // no replica on this node
	}
	if err != nil {
		return false, err
	}
	return st.Role == drbd.RolePrimary, nil
}

func (h nodeVolumeHost) Snapshot(ctx context.Context, id, snap string) (string, int64, error) {
	size, err := h.deviceSize(ctx, id)
	if err != nil {
		return "", 0, err
	}
	origin, err := h.lv.Find(ctx, id)
	if err != nil {
		return "", 0, err
	}
	name := id + "-snap-" + snap
	if err := h.lv.Snapshot(ctx, origin.VG, id, name); err != nil {
		return "", 0, err
	}
	if err := h.lv.Activate(ctx, origin.VG, name); err != nil {
		return "", 0, err
	}
	return lvm.DevicePath(origin.VG, name), size, nil
}

// deviceSize is the size of the volume's DRBD device, which DRBD's internal metadata makes smaller than its LV.
func (h nodeVolumeHost) deviceSize(ctx context.Context, id string) (int64, error) {
	st, err := h.dr.Status(ctx, id)
	if err != nil {
		return 0, err
	}
	if len(st.Volumes) == 0 {
		return 0, fmt.Errorf("volume %s has no DRBD device", id)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/sys/class/block/drbd%d/size", st.Volumes[0].Minor))
	if err != nil {
		return 0, err
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return sectors * 512, err
}

func (h nodeVolumeHost) DropSnapshot(ctx context.Context, id, snap string) error {
	lv, err := h.lv.Find(ctx, id+"-snap-"+snap)
	if experrors.KindOf(err) == experrors.KindNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	return h.lv.Remove(ctx, lv.VG, lv.Name)
}

// Restic runs without a cache (the unit has no writable home) and waits out other nodes' locks.
func (h nodeVolumeHost) Restic(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, h.bin, append([]string{"--no-cache", "--retry-lock", "5m"}, args...)...)
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("restic %s: %w: %s", args[0], err, strings.TrimSpace(lastLines(stderr.String(), 5)))
	}
	return stdout.String(), nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// agentVolumeIDs asks the local agent which ID each volume name has.
func agentVolumeIDs(ctx context.Context) (map[string]string, error) {
	conn, err := grpc.NewClient("unix://"+agentSocket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := pb.NewNodeServiceClient(conn).ListKeyValue(ctx, &pb.ListKeyValueRequest{Prefix: "/volumes/", Stale: true})
	if err != nil {
		return nil, err
	}
	return volumeIDsFrom(res.GetEntries()), nil
}

func runRestic(ctx context.Context, namespace string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	s, err := resticSetupFrom(namespace, cfg)
	if err != nil {
		return err
	}
	bin, err := resolveBin("restic")
	if err != nil {
		return err
	}
	status := newResticStatus()
	srv := &http.Server{Addr: net.JoinHostPort("", s.Port), Handler: status, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("restic-backup: status server: %v", err)
		}
	}()
	defer func() { _ = srv.Close() }()
	r := &resticRunner{
		s:         s,
		host:      nodeVolumeHost{env: resticEnv(s), dr: drbd.New(), lv: lvm.New(), bin: bin},
		volumeIDs: agentVolumeIDs,
		status:    status,
	}
	log.Printf("restic-backup: backing up %v every %v to %s", s.Volumes, s.Interval, s.Repository)
	for {
		r.tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(resticTick):
		}
	}
}
