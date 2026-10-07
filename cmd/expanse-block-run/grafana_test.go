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
