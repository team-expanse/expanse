package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGrafanaHomepathIsTheShareDirBesideTheBinary(t *testing.T) {
	if got := grafanaHomepath("/nix/store/abc-grafana/bin/grafana"); got != "/nix/store/abc-grafana/share/grafana" {
		t.Errorf("homepath = %q", got)
	}
}

func TestWriteGrafanaProvisioningPointsAtThePrometheusURL(t *testing.T) {
	dir := t.TempDir()
	if err := writeGrafanaProvisioning(dir, "http://10.0.0.50:9090"); err != nil {
		t.Fatal(err)
	}
	var ds struct {
		Datasources []struct {
			UID       string `yaml:"uid"`
			Type      string `yaml:"type"`
			URL       string `yaml:"url"`
			IsDefault bool   `yaml:"isDefault"`
		} `yaml:"datasources"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "provisioning/datasources/expanse.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	// The shipped dashboard's panels all reference this uid.
	if len(ds.Datasources) != 1 || ds.Datasources[0].UID != "expanse-prometheus" ||
		ds.Datasources[0].Type != "prometheus" || ds.Datasources[0].URL != "http://10.0.0.50:9090" || !ds.Datasources[0].IsDefault {
		t.Errorf("datasource = %+v", ds)
	}

	var prov struct {
		Providers []struct {
			Options struct {
				Path string `yaml:"path"`
			} `yaml:"options"`
		} `yaml:"providers"`
	}
	raw, err = os.ReadFile(filepath.Join(dir, "provisioning/dashboards/expanse.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, &prov); err != nil {
		t.Fatal(err)
	}
	if len(prov.Providers) != 1 || prov.Providers[0].Options.Path != filepath.Join(dir, "dashboards") {
		t.Fatalf("dashboard provider = %+v", prov)
	}
	dash, err := os.ReadFile(filepath.Join(dir, "dashboards/expanse-cluster-health.json"))
	if err != nil || !strings.Contains(string(dash), "expanse_node_health") {
		t.Errorf("shipped dashboard not written: %v", err)
	}
}

func TestGrafanaEnvConfiguresPathsPortAndAdmin(t *testing.T) {
	env := grafanaEnv("/w", "/data", "3000", map[string]any{"adminPassword": "pw"})
	for _, want := range []string{
		"GF_PATHS_DATA=/data",
		"GF_PATHS_PROVISIONING=/w/provisioning",
		"GF_PATHS_LOGS=/w/logs",
		"GF_PATHS_PLUGINS=/data/plugins",
		"GF_SERVER_HTTP_PORT=3000",
		"GF_SECURITY_ADMIN_PASSWORD=pw",
		"GF_ANALYTICS_REPORTING_ENABLED=false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES=false",
		"GF_PLUGINS_PREINSTALL_DISABLED=true",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
}

func TestGrafanaEnvAllowsAnonymousViewersWhenAsked(t *testing.T) {
	env := grafanaEnv("/w", "/data", "3000", map[string]any{"adminPassword": "pw", "anonymousViewer": true})
	for _, want := range []string{"GF_AUTH_ANONYMOUS_ENABLED=true", "GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, env)
		}
	}
	if slices.Contains(grafanaEnv("/w", "/d", "3000", map[string]any{}), "GF_AUTH_ANONYMOUS_ENABLED=true") {
		t.Error("anonymous access must be off by default")
	}
}

func TestGrafanaShippedPluginsLiveBesideTheProfileBinary(t *testing.T) {
	got := grafanaShippedPlugins("/run/current-system/sw/bin/grafana")
	if want := "/run/current-system/sw/lib/grafana/plugins"; got != want {
		t.Errorf("shipped plugins = %s, want %s", got, want)
	}
}

func TestLinkGrafanaPluginsRefreshesShippedOnesAndKeepsInstalledOnes(t *testing.T) {
	root := t.TempDir()
	src, dst := filepath.Join(root, "src"), filepath.Join(root, "dst")
	prom := filepath.Join(root, "store", "prometheus-13.2.2")
	for _, d := range []string{src, dst, prom, filepath.Join(dst, "installed-panel")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(prom, filepath.Join(src, "prometheus")); err != nil {
		t.Fatal(err)
	}
	// Left by an earlier start: a plugin no longer shipped and an outdated link.
	for name, target := range map[string]string{"retired": "/nix/store/gone-retired", "prometheus": "/nix/store/gone-old-prometheus"} {
		if err := os.Symlink(target, filepath.Join(dst, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := linkGrafanaPlugins(src, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(filepath.Join(dst, "prometheus")); got != prom {
		t.Errorf("prometheus links to %q, want %q", got, prom)
	}
	if _, err := os.Lstat(filepath.Join(dst, "retired")); !os.IsNotExist(err) {
		t.Errorf("retired plugin link kept: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "installed-panel")); err != nil || !fi.IsDir() {
		t.Errorf("a plugin installed through the UI was touched: %v", err)
	}
}

func TestLinkGrafanaPluginsWithoutShippedPlugins(t *testing.T) {
	dst := t.TempDir()
	if err := linkGrafanaPlugins(filepath.Join(dst, "missing"), dst); err != nil {
		t.Errorf("err = %v, want nil when the node ships no plugins", err)
	}
}
