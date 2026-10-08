package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	testKeyID  = "GK0123456789abcdef01234567"
	testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func TestS3SetupDefaults(t *testing.T) {
	s, err := s3SetupFrom("/vol", map[string]any{"accessKeyId": testKeyID, "secretAccessKey": testSecret})
	if err != nil {
		t.Fatal(err)
	}
	want := s3Setup{
		Port: "13900", RPCPort: "13901", AdminPort: "13903", Region: "garage",
		MetaDir: "/vol/.garage/meta", DataDir: "/vol/.garage/data", SecretsDir: "/vol/.garage/secrets",
		AccessKeyID: testKeyID, SecretAccessKey: testSecret,
	}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("setup = %+v\nwant    %+v", s, want)
	}
}

func TestS3SetupFromConfig(t *testing.T) {
	s, err := s3SetupFrom("/vol", map[string]any{
		"port": float64(23900), "rpcPort": float64(23901), "adminPort": float64(23903), "region": "eu-1",
		"accessKeyId": testKeyID, "secretAccessKey": testSecret, "buckets": []any{"media", "logs"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "23900" || s.RPCPort != "23901" || s.AdminPort != "23903" || s.Region != "eu-1" ||
		!reflect.DeepEqual(s.Buckets, []string{"media", "logs"}) {
		t.Errorf("setup = %+v", s)
	}
}

func TestS3SetupRejectsBadInput(t *testing.T) {
	ok := map[string]any{"accessKeyId": testKeyID, "secretAccessKey": testSecret}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for k2, v2 := range ok {
			m[k2] = v2
		}
		m[k] = v
		return m
	}
	for name, cfg := range map[string]map[string]any{
		"no key":            {"secretAccessKey": testSecret},
		"no secret":         {"accessKeyId": testKeyID},
		"key not GK+24 hex": with("accessKeyId", "AKIAEXAMPLE"),
		"secret not hex":    with("secretAccessKey", strings.Repeat("z", 64)),
		"bad bucket name":   with("buckets", []any{"Not_Valid"}),
		"same ports":        with("rpcPort", float64(13900)),
	} {
		if _, err := s3SetupFrom("/vol", cfg); err == nil {
			t.Errorf("%s: accepted %v", name, cfg)
		}
	}
}

func TestGarageConfigKeepsEverythingOnTheVolume(t *testing.T) {
	s, _ := s3SetupFrom("/vol", map[string]any{"accessKeyId": testKeyID, "secretAccessKey": testSecret})
	conf := garageConfig(s, "rpcsecret", "admintoken")
	for _, want := range []string{
		`metadata_dir = "/vol/.garage/meta"`, `data_dir = "/vol/.garage/data"`,
		`db_engine = "lmdb"`, "replication_factor = 1",
		"metadata_fsync = true", "data_fsync = true",
		`rpc_bind_addr = "127.0.0.1:13901"`, `rpc_public_addr = "127.0.0.1:13901"`,
		`rpc_secret = "rpcsecret"`,
		`s3_region = "garage"`, `api_bind_addr = "0.0.0.0:13900"`,
		`api_bind_addr = "127.0.0.1:13903"`, `admin_token = "admintoken"`,
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q:\n%s", want, conf)
		}
	}
}

func TestLoadOrCreateSecretIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets", "rpc")
	a, err := loadOrCreateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Errorf("secret %q is not 32 hex bytes", a)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("secret mode = %v, want 0600", st.Mode().Perm())
	}
	if b, _ := loadOrCreateSecret(path); b != a {
		t.Errorf("secret changed on reload: %q then %q", a, b)
	}
}

func TestLoadOrCreateSecretReplacesATornFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpc")
	// A crash between rename and writeback leaves the file empty.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadOrCreateSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if !garageSecret.MatchString(got) {
		t.Errorf("torn secret file kept: %q", got)
	}
}

func TestEnsureNodeKeyWritesAnEd25519PairOnce(t *testing.T) {
	dir := t.TempDir()
	if err := ensureNodeKey(dir); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(dir, "node_key"))
	pub, _ := os.ReadFile(filepath.Join(dir, "node_key.pub"))
	if len(key) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.PrivateKey(key).Public().(ed25519.PublicKey), pub) {
		t.Fatalf("node_key (%d bytes) and node_key.pub (%d bytes) are not one pair", len(key), len(pub))
	}
	if err := ensureNodeKey(dir); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(dir, "node_key")); !bytes.Equal(again, key) {
		t.Error("node_key regenerated: the node id would change")
	}
}

func TestEnsureNodeKeyRefusesATornKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node_key"), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureNodeKey(dir); err == nil {
		t.Error("torn node_key accepted")
	}
}

// fakeGarage is the slice of Garage's v1 admin API the bootstrap uses.
type fakeGarage struct {
	mu            sync.Mutex
	layoutVersion int
	hasRole       bool
	keys          map[string]string // id -> secret
	createBucket  bool
	buckets       map[string]string // alias -> id
	allowed       map[string]bool   // bucket id
	calls         []string
}

func newFakeGarage() *fakeGarage {
	return &fakeGarage{keys: map[string]string{}, buckets: map[string]string{}, allowed: map[string]bool{}}
}

func (f *fakeGarage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	var body map[string]any
	var roles []map[string]any
	if r.URL.Path == "/v1/layout" && r.Method == http.MethodPost {
		_ = json.NewDecoder(r.Body).Decode(&roles)
	} else {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	reply := func(code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	roleOf := func() any {
		if f.hasRole {
			return map[string]any{"zone": "expanse"}
		}
		return nil
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/status":
		reply(200, map[string]any{
			"node": "abc", "layoutVersion": f.layoutVersion,
			"nodes": []any{map[string]any{"id": "abc", "role": roleOf()}},
		})
	case "POST /v1/layout":
		if len(roles) != 1 || roles[0]["id"] != "abc" || roles[0]["zone"] != "expanse" {
			reply(400, map[string]any{"message": "bad roles"})
			return
		}
		reply(200, map[string]any{})
	case "POST /v1/layout/apply":
		if int(body["version"].(float64)) != f.layoutVersion+1 {
			reply(400, map[string]any{"message": "wrong version"})
			return
		}
		f.layoutVersion++
		f.hasRole = true
		reply(200, map[string]any{})
	case "GET /v1/key":
		secret, ok := f.keys[r.URL.Query().Get("id")]
		if !ok {
			reply(404, map[string]any{"code": "NoSuchAccessKey"})
			return
		}
		reply(200, map[string]any{"accessKeyId": r.URL.Query().Get("id"), "secretAccessKey": secret})
	case "POST /v1/key/import":
		f.keys[body["accessKeyId"].(string)] = body["secretAccessKey"].(string)
		reply(200, map[string]any{})
	case "POST /v1/key":
		allow, _ := body["allow"].(map[string]any)
		f.createBucket = allow["createBucket"] == true
		reply(200, map[string]any{})
	case "GET /v1/bucket":
		id, ok := f.buckets[r.URL.Query().Get("globalAlias")]
		if !ok {
			reply(404, map[string]any{"code": "NoSuchBucket"})
			return
		}
		reply(200, map[string]any{"id": id})
	case "POST /v1/bucket":
		alias := body["globalAlias"].(string)
		f.buckets[alias] = "id-" + alias
		reply(200, map[string]any{"id": "id-" + alias})
	case "POST /v1/bucket/allow":
		p, _ := body["permissions"].(map[string]any)
		if body["accessKeyId"] != testKeyID || p["read"] != true || p["write"] != true || p["owner"] != true {
			reply(400, map[string]any{"message": "bad allow"})
			return
		}
		f.allowed[body["bucketId"].(string)] = true
		reply(200, map[string]any{})
	default:
		reply(404, map[string]any{"message": "unexpected " + r.Method + " " + r.URL.Path})
	}
}

func bootstrapAgainst(t *testing.T, f *fakeGarage, s s3Setup) error {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	return garageAdmin{base: srv.URL, token: "tok", hc: srv.Client()}.bootstrap(context.Background(), s, 1<<30)
}

func TestGarageBootstrapProvisionsAFreshNode(t *testing.T) {
	f := newFakeGarage()
	s := s3Setup{AccessKeyID: testKeyID, SecretAccessKey: testSecret, Buckets: []string{"media", "logs"}}
	if err := bootstrapAgainst(t, f, s); err != nil {
		t.Fatal(err)
	}
	if f.layoutVersion != 1 || f.keys[testKeyID] != testSecret || !f.createBucket {
		t.Errorf("layout v%d, keys %v, createBucket %v", f.layoutVersion, f.keys, f.createBucket)
	}
	if !f.allowed["id-media"] || !f.allowed["id-logs"] {
		t.Errorf("buckets not granted to the key: %v", f.allowed)
	}
}

func TestGarageBootstrapIsIdempotent(t *testing.T) {
	f := newFakeGarage()
	s := s3Setup{AccessKeyID: testKeyID, SecretAccessKey: testSecret, Buckets: []string{"media"}}
	if err := bootstrapAgainst(t, f, s); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if err := bootstrapAgainst(t, f, s); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c == "POST /v1/layout" || c == "POST /v1/key/import" || c == "POST /v1/bucket" {
			t.Errorf("second bootstrap repeated %s: %v", c, f.calls)
		}
	}
	if f.layoutVersion != 1 {
		t.Errorf("layout re-applied: v%d", f.layoutVersion)
	}
}

func TestGarageBootstrapRefusesAChangedSecret(t *testing.T) {
	f := newFakeGarage()
	f.keys[testKeyID] = strings.Repeat("f", 64)
	f.layoutVersion, f.hasRole = 1, true
	err := bootstrapAgainst(t, f, s3Setup{AccessKeyID: testKeyID, SecretAccessKey: testSecret})
	if err == nil || !strings.Contains(err.Error(), "secretAccessKey") {
		t.Errorf("err = %v, want one naming secretAccessKey", err)
	}
}
