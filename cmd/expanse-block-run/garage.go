package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	garageKeyID     = regexp.MustCompile(`^GK[0-9a-f]{24}$`)
	garageSecret    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	s3BucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// s3Setup is everything one storage/s3 instance needs.
type s3Setup struct {
	Port, RPCPort, AdminPort, Region string
	MetaDir, DataDir, SecretsDir     string
	AccessKeyID, SecretAccessKey     string
	Buckets                          []string
}

// s3SetupFrom applies the block's defaults to spec.config and validates it.
func s3SetupFrom(mountPath string, cfg map[string]any) (s3Setup, error) {
	state := filepath.Join(mountPath, ".garage")
	s := s3Setup{
		Port:            cfgPortOr(cfg, "13900"),
		RPCPort:         cfgPortKeyOr(cfg, "rpcPort", "13901"),
		AdminPort:       cfgPortKeyOr(cfg, "adminPort", "13903"),
		Region:          cfgStr(cfg, "region"),
		MetaDir:         filepath.Join(state, "meta"),
		DataDir:         filepath.Join(state, "data"),
		SecretsDir:      filepath.Join(state, "secrets"),
		AccessKeyID:     cfgStr(cfg, "accessKeyId"),
		SecretAccessKey: cfgStr(cfg, "secretAccessKey"),
	}
	if s.Region == "" {
		s.Region = "garage"
	}
	if !garageKeyID.MatchString(s.AccessKeyID) {
		return s3Setup{}, fmt.Errorf("storage/s3: accessKeyId must be GK followed by 24 lowercase hex digits")
	}
	if !garageSecret.MatchString(s.SecretAccessKey) {
		return s3Setup{}, fmt.Errorf("storage/s3: secretAccessKey must be 64 lowercase hex digits")
	}
	if s.Port == s.RPCPort || s.Port == s.AdminPort || s.RPCPort == s.AdminPort {
		return s3Setup{}, fmt.Errorf("storage/s3: port, rpcPort and adminPort must differ")
	}
	list, _ := cfg["buckets"].([]any)
	for _, v := range list {
		b, _ := v.(string)
		if !s3BucketPattern.MatchString(b) {
			return s3Setup{}, fmt.Errorf("storage/s3: bucket %q is not a valid S3 bucket name", b)
		}
		s.Buckets = append(s.Buckets, b)
	}
	return s, nil
}

// cfgPortKeyOr reads a port under key k, def when unset.
func cfgPortKeyOr(m map[string]any, k, def string) string {
	if v, ok := m[k].(float64); ok && v > 0 {
		return fmt.Sprintf("%d", int(v))
	}
	return def
}

// garageConfig renders a single-node garage.toml whose state is all on the volume.
func garageConfig(s s3Setup, rpcSecret, adminToken string) string {
	return fmt.Sprintf(`metadata_dir = %q
data_dir = %q
# LMDB, not sqlite: Garage's sqlite runs WAL with synchronous=NORMAL even with
# metadata_fsync, so a crash (every failover) can drop acknowledged objects.
db_engine = "lmdb"
replication_factor = 1
metadata_fsync = true
data_fsync = true
rpc_bind_addr = "127.0.0.1:%s"
rpc_public_addr = "127.0.0.1:%s"
rpc_secret = %q

[s3_api]
s3_region = %q
api_bind_addr = "0.0.0.0:%s"

[admin]
api_bind_addr = "127.0.0.1:%s"
admin_token = %q
`, s.MetaDir, s.DataDir, s.RPCPort, s.RPCPort, rpcSecret, s.Region, s.Port, s.AdminPort, adminToken)
}

// loadOrCreateSecret returns the hex secret at path, generating it when
// missing or torn; only this node's own Garage ever uses it.
func loadOrCreateSecret(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if secret := strings.TrimSpace(string(raw)); err == nil && garageSecret.MatchString(secret) {
		return secret, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return secret, writeFileDurably(path, []byte(secret+"\n"), 0o600)
}

// writeFileDurably replaces path so a crash leaves the old or the new
// content, never an empty file: data and directory are both fsynced.
func writeFileDurably(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ensureNodeKey writes Garage's ed25519 node key durably before its first
// start: Garage never fsyncs it, and a new key would strand the data.
func ensureNodeKey(metaDir string) error {
	path := filepath.Join(metaDir, "node_key")
	if raw, err := os.ReadFile(path); err == nil {
		if len(raw) != ed25519.PrivateKeySize {
			return fmt.Errorf("%s is %d bytes, not a %d-byte key; restore it from a backup", path, len(raw), ed25519.PrivateKeySize)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	if err := writeFileDurably(path+".pub", pub, 0o644); err != nil {
		return err
	}
	return writeFileDurably(path, key, 0o600)
}

// garageAdmin is a client for Garage's v1 admin API.
type garageAdmin struct {
	base, token string
	hc          *http.Client
}

// call sends one request; non-2xx statuses are returned, not errors.
func (g garageAdmin) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	resp, err := g.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

// bootstrap gives the node a layout role, then provisions the key and buckets; each step is skipped when done.
func (g garageAdmin) bootstrap(ctx context.Context, s s3Setup, capacity uint64) error {
	if err := g.ensureLayout(ctx, capacity); err != nil {
		return err
	}
	if err := g.ensureKey(ctx, s); err != nil {
		return err
	}
	for _, b := range s.Buckets {
		if err := g.ensureBucket(ctx, b, s.AccessKeyID); err != nil {
			return err
		}
	}
	return nil
}

func (g garageAdmin) ensureLayout(ctx context.Context, capacity uint64) error {
	var st struct {
		Node          string `json:"node"`
		LayoutVersion int    `json:"layoutVersion"`
		Nodes         []struct {
			ID   string          `json:"id"`
			Role json.RawMessage `json:"role"`
		} `json:"nodes"`
	}
	// The admin API comes up a moment after the process starts.
	for {
		_, err := g.call(ctx, http.MethodGet, "/v1/status", nil, &st)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("garage admin API never answered: %w", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
	for _, n := range st.Nodes {
		if n.ID == st.Node && len(n.Role) > 0 && string(n.Role) != "null" {
			return nil
		}
	}
	role := []map[string]any{{"id": st.Node, "zone": "expanse", "capacity": capacity, "tags": []string{}}}
	if _, err := g.call(ctx, http.MethodPost, "/v1/layout", role, nil); err != nil {
		return err
	}
	_, err := g.call(ctx, http.MethodPost, "/v1/layout/apply", map[string]int{"version": st.LayoutVersion + 1}, nil)
	return err
}

func (g garageAdmin) ensureKey(ctx context.Context, s s3Setup) error {
	var key struct {
		SecretAccessKey string `json:"secretAccessKey"`
	}
	path := "/v1/key?showSecretKey=true&id=" + url.QueryEscape(s.AccessKeyID)
	code, err := g.call(ctx, http.MethodGet, path, nil, &key)
	switch {
	case code == http.StatusNotFound:
		imp := map[string]string{"accessKeyId": s.AccessKeyID, "secretAccessKey": s.SecretAccessKey, "name": "expanse"}
		if _, err := g.call(ctx, http.MethodPost, "/v1/key/import", imp, nil); err != nil {
			return err
		}
	case err != nil:
		return err
	case key.SecretAccessKey != s.SecretAccessKey:
		return fmt.Errorf("secretAccessKey differs from key %s's on the volume; Garage cannot re-import a key id", s.AccessKeyID)
	}
	allow := map[string]any{"allow": map[string]bool{"createBucket": true}}
	_, err = g.call(ctx, http.MethodPost, "/v1/key?id="+url.QueryEscape(s.AccessKeyID), allow, nil)
	return err
}

func (g garageAdmin) ensureBucket(ctx context.Context, alias, keyID string) error {
	var b struct {
		ID string `json:"id"`
	}
	code, err := g.call(ctx, http.MethodGet, "/v1/bucket?globalAlias="+url.QueryEscape(alias), nil, &b)
	if code == http.StatusNotFound {
		_, err = g.call(ctx, http.MethodPost, "/v1/bucket", map[string]string{"globalAlias": alias}, &b)
	}
	if err != nil {
		return err
	}
	grant := map[string]any{
		"bucketId": b.ID, "accessKeyId": keyID,
		"permissions": map[string]bool{"read": true, "write": true, "owner": true},
	}
	_, err = g.call(ctx, http.MethodPost, "/v1/bucket/allow", grant, nil)
	return err
}

// volumeCapacity is the layout capacity: the volume's size, or 1 GiB if unknown.
func volumeCapacity(path string) uint64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil || st.Blocks == 0 {
		return 1 << 30
	}
	return st.Blocks * uint64(st.Bsize)
}

// syncFS flushes every dirty page of the filesystem holding path.
func syncFS(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Syncfs(int(f.Fd()))
}

// runS3 serves storage/s3 with a single-node Garage whose metadata, data,
// node key and generated secrets all live on the bound volume.
func runS3(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	mountPath := firstMount(mountPaths(args))
	if mountPath == "" {
		return fmt.Errorf("storage/s3: no bound storage mount yet")
	}
	if err := waitForMount(mountPath, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
		return fmt.Errorf("storage/s3: %w", err)
	}
	s, err := s3SetupFrom(mountPath, cfg)
	if err != nil {
		return err
	}
	for _, dir := range []string{s.MetaDir, s.DataDir, s.SecretsDir} {
		if err := mkdirAllRetrying(os.MkdirAll, dir, 0o700, 10, 500*time.Millisecond); err != nil {
			return fmt.Errorf("storage/s3: %w", err)
		}
	}
	if err := ensureNodeKey(s.MetaDir); err != nil {
		return fmt.Errorf("storage/s3: %w", err)
	}
	rpcSecret, err := loadOrCreateSecret(filepath.Join(s.SecretsDir, "rpc_secret"))
	if err != nil {
		return err
	}
	adminToken, err := loadOrCreateSecret(filepath.Join(s.SecretsDir, "admin_token"))
	if err != nil {
		return err
	}
	work := "/tmp/expblk-" + instance
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "garage.toml")
	if err := os.WriteFile(conf, []byte(garageConfig(s, rpcSecret, adminToken)), 0o600); err != nil {
		return err
	}
	bin, err := resolveBin("garage")
	if err != nil {
		return fmt.Errorf("block runtime: garage not found in PATH (ship the package): %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, "-c", conf, "server")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	admin := garageAdmin{base: "http://127.0.0.1:" + s.AdminPort, token: adminToken, hc: &http.Client{Timeout: 10 * time.Second}}
	bctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	booted := make(chan error, 1)
	go func() { booted <- admin.bootstrap(bctx, s, volumeCapacity(mountPath)) }()
	select {
	case err := <-exited:
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			err = errors.New("exit status 0")
		}
		return fmt.Errorf("storage/s3: garage exited during bootstrap: %w", err)
	case err := <-booted:
		if err != nil && ctx.Err() == nil {
			_ = cmd.Process.Kill()
			<-exited
			return fmt.Errorf("storage/s3: %w", err)
		}
	}
	// Garage writes its layout without fsync; flush it before clients arrive.
	if err := syncFS(mountPath); err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: storage/s3: syncfs: %v\n", err)
	}
	fmt.Printf("expanse-block-run: s3 serving on :%s (region %s, buckets %v)\n", s.Port, s.Region, s.Buckets)
	err = <-exited
	if ctx.Err() != nil {
		return nil //nolint:nilerr // deliberate stop
	}
	return err
}
