package apply

import (
	"testing"

	"github.com/expanse/expanse/internal/blocks/catalog"
	"github.com/expanse/expanse/internal/blocks/validate"
)

// requiredConfig holds a value for each required config field with no default; a new type must add its row.
var requiredConfig = map[string]map[string]any{
	"ai/ollama":             nil,
	"db/mariadb":            {"rootPassword": "root-secret-1"},
	"db/postgres":           {"replicationPassword": "repl-secret-1", "superuserPassword": "super-secret-1"},
	"db/redis":              nil,
	"dev/forgejo":           {"rootURL": "https://git.example.org/", "adminUser": "gitadmin", "adminPassword": "admin-secret-1", "adminEmail": "admin@example.org"},
	"iscsi/target":          nil,
	"media/immich":          nil,
	"media/jellyfin":        nil,
	"monitor/grafana":       {"prometheusUrl": "http://prometheus.default.expanse:9090", "adminPassword": "admin-secret-1"},
	"monitor/node-exporter": nil,
	"monitor/prometheus":    nil,
	"monitor/uptime-kuma":   nil,
	"net/haproxy":           {"frontends": []any{map[string]any{"name": "web", "port": 18080, "servers": []any{"app1:8080"}}}},
	"security/vaultwarden":  {"domain": "https://vault.example.org"},
	"share/nfs":             nil,
	"share/smb":             nil,
	"storage/s3":            {"accessKeyId": "GK0123456789abcdef01234567", "secretAccessKey": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	"util/echo":             nil,
	"util/restic-backup":    {"repository": "sftp:backup@host:/srv/restic", "password": "restic-secret-1", "volumes": []any{"data"}},
	"vm/instance":           nil,
	"web/caddy":             {"sites": []any{map[string]any{"host": "app.example.org", "respond": "ok"}}},
	"web/nginx":             {"serverName": "www.example.org"},
	"web/static-site":       nil,
	"web/whoami":            nil,
}

// Every shipped type has a template that, with its defaults and the required fields, passes admission.
func TestEveryShippedTypeHasAnAdmissibleTemplate(t *testing.T) {
	cat, err := catalog.Load("../../../nix/blocks")
	if err != nil {
		t.Fatal(err)
	}
	var caps []string
	for _, id := range cat.Types() {
		typ, _ := cat.GetType(id)
		caps = append(caps, typ.Capabilities...)
	}
	ctx := validate.Context{Catalog: cat, NodeCount: 3, KnownCapabilities: caps}
	for _, id := range cat.Types() {
		t.Run(id, func(t *testing.T) {
			cfg, known := requiredConfig[id]
			if !known {
				t.Fatalf("add %s to requiredConfig", id)
			}
			typ, _ := cat.GetType(id)
			if typ.Template == nil {
				t.Fatalf("nix/blocks/%s has no template.yaml", id)
			}
			b, err := FromTemplate(typ, "default", "t1", cfg)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range validate.Validate(b, ctx) {
				t.Errorf("%v", e)
			}
		})
	}
}

// A template's first port forwards to the config port, and a VIP port never equals it (the VIP holder runs a replica).
func TestShippedTemplatesForwardToTheConfigPort(t *testing.T) {
	cat, err := catalog.Load("../../../nix/blocks")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cat.Types() {
		typ, _ := cat.GetType(id)
		b, err := FromTemplate(typ, "", "t1", requiredConfig[id])
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		ports := b.GetSpec().GetNetwork().GetPorts()
		cfgPort, ok := b.GetSpec().GetConfig().AsMap()["port"].(float64)
		if len(ports) == 0 || !ok {
			continue
		}
		if p := ports[0]; int32(cfgPort) != p.GetTargetPort() {
			t.Errorf("%s: port %s targets %d, but the block listens on config.port %v", id, p.GetName(), p.GetTargetPort(), cfgPort)
		}
		for _, p := range ports {
			if p.GetExpose().String() == "EXPOSE_VIP" && p.GetPort() == p.GetTargetPort() {
				t.Errorf("%s: VIP port %d equals its target port", id, p.GetPort())
			}
		}
	}
}

// Every template volume has a mountPath: the bridge binds no volume without one, raw ones included.
func TestShippedTemplateVolumesHaveAMountPath(t *testing.T) {
	cat, err := catalog.Load("../../../nix/blocks")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range cat.Types() {
		typ, _ := cat.GetType(id)
		b, err := FromTemplate(typ, "", "t1", requiredConfig[id])
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for _, st := range b.GetSpec().GetStorage() {
			if st.GetMountPath() == "" {
				t.Errorf("%s: volume %s has no mountPath, so it is never bound to the replica", id, st.GetName())
			}
		}
	}
}
