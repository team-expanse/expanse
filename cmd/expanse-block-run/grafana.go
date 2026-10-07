package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/expanse/expanse/deploy"
)

// grafanaHomepath is <pkg>/share/grafana for the resolved <pkg>/bin/grafana.
func grafanaHomepath(bin string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(bin)), "share", "grafana")
}

// writeGrafanaProvisioning provisions the Expanse datasource and the shipped
// dashboard under dir; the dashboard's panels reference the fixed uid.
func writeGrafanaProvisioning(dir, prometheusURL string) error {
	dashDir := filepath.Join(dir, "dashboards")
	datasources := map[string]any{
		"apiVersion": 1,
		"datasources": []map[string]any{{
			"name": "Expanse Prometheus", "uid": "expanse-prometheus", "type": "prometheus",
			"access": "proxy", "url": prometheusURL, "isDefault": true, "editable": false,
		}},
	}
	providers := map[string]any{
		"apiVersion": 1,
		"providers": []map[string]any{{
			"name": "Expanse", "orgId": 1, "type": "file", "disableDeletion": true,
			"options": map[string]string{"path": dashDir},
		}},
	}
	files := map[string][]byte{filepath.Join(dashDir, "expanse-cluster-health.json"): deploy.ClusterHealthDashboard}
	for path, doc := range map[string]any{
		filepath.Join(dir, "provisioning/datasources/expanse.yaml"): datasources,
		filepath.Join(dir, "provisioning/dashboards/expanse.yaml"):  providers,
	} {
		raw, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		files[path] = raw
	}
	// Grafana resolves every provisioning subdir; empty ones must exist.
	for _, sub := range []string{"plugins", "alerting", "notifiers"} {
		if err := os.MkdirAll(filepath.Join(dir, "provisioning", sub), 0o700); err != nil {
			return err
		}
	}
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// grafanaEnv configures Grafana through GF_* variables instead of grafana.ini.
func grafanaEnv(work, data, port string, cfg map[string]any) []string {
	env := []string{
		"GF_PATHS_DATA=" + data,
		"GF_PATHS_PROVISIONING=" + filepath.Join(work, "provisioning"),
		"GF_PATHS_LOGS=" + filepath.Join(work, "logs"),
		"GF_PATHS_PLUGINS=" + filepath.Join(data, "plugins"),
		"GF_SERVER_HTTP_PORT=" + port,
		"GF_ANALYTICS_REPORTING_ENABLED=false",
		"GF_ANALYTICS_CHECK_FOR_UPDATES=false",
		"GF_ANALYTICS_CHECK_FOR_PLUGIN_UPDATES=false",
		// Offline clusters: the preinstaller's grafana.com retries stall startup.
		"GF_PLUGINS_PREINSTALL_DISABLED=true",
	}
	if pw := cfgStr(cfg, "adminPassword"); pw != "" {
		env = append(env, "GF_SECURITY_ADMIN_PASSWORD="+pw)
	}
	if cfgBool(cfg, "anonymousViewer", false) {
		env = append(env, "GF_AUTH_ANONYMOUS_ENABLED=true", "GF_AUTH_ANONYMOUS_ORG_ROLE=Viewer")
	}
	return env
}

// runGrafana serves monitor/grafana with the shipped Expanse datasource and
// dashboard; its database lives on the volume when one is bound.
func runGrafana(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPortOr(cfg, "3000")
	work := "/tmp/expblk-" + instance
	data := firstMount(mountPaths(args))
	if data == "" {
		data = work + "-data"
	}
	for _, d := range []string{work, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := writeGrafanaProvisioning(work, cfgStr(cfg, "prometheusUrl")); err != nil {
		return err
	}
	bin, err := resolveBin("grafana")
	if err != nil {
		return fmt.Errorf("block runtime: grafana not found in PATH (ship the package): %w", err)
	}
	fmt.Printf("expanse-block-run: grafana serving on :%s\n", port)
	return execWorkload(ctx, bin, []string{"server", "--homepath=" + grafanaHomepath(bin)},
		grafanaEnv(work, data, port, cfg)...)
}
