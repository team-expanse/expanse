package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type promYAML struct {
	Global struct {
		ScrapeInterval     string `yaml:"scrape_interval"`
		EvaluationInterval string `yaml:"evaluation_interval"`
	} `yaml:"global"`
	RuleFiles []string `yaml:"rule_files"`
	Jobs      []struct {
		Name      string `yaml:"job_name"`
		Scheme    string `yaml:"scheme"`
		TLSConfig struct {
			CAFile     string `yaml:"ca_file"`
			ServerName string `yaml:"server_name"`
		} `yaml:"tls_config"`
		Authorization struct {
			CredentialsFile string `yaml:"credentials_file"`
		} `yaml:"authorization"`
		Static []struct {
			Targets []string          `yaml:"targets"`
			Labels  map[string]string `yaml:"labels"`
		} `yaml:"static_configs"`
	} `yaml:"scrape_configs"`
}

func parseProm(t *testing.T, raw []byte) promYAML {
	t.Helper()
	var p promYAML
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("generated config is not YAML: %v\n%s", err, raw)
	}
	return p
}

func TestScrapeNodesParsesIDHostPairs(t *testing.T) {
	got := scrapeNodes([]string{"--scrape-node", "n1=10.0.0.1", "--config", "{}", "--scrape-node", "bad", "--scrape-node", "n2=10.0.0.2"})
	if len(got) != 2 || got[0] != (scrapeNode{"n1", "10.0.0.1"}) || got[1] != (scrapeNode{"n2", "10.0.0.2"}) {
		t.Errorf("scrapeNodes = %v", got)
	}
}

func TestPrometheusConfigScrapesEachNodeWithItsOwnTLSName(t *testing.T) {
	p := parseProm(t, prometheusConfig(promSetup{
		Nodes:            []scrapeNode{{"n1", "10.0.0.1"}, {"n2", "10.0.0.2"}},
		Interval:         "5s",
		SelfPort:         "9090",
		NodeExporterPort: 9100,
		CAFile:           "/w/ca.pem",
		TokenFile:        "/w/token",
		RulesFile:        "/w/rules.yml",
	}))
	if p.Global.ScrapeInterval != "5s" || p.Global.EvaluationInterval != "5s" {
		t.Errorf("global = %+v, want 5s scrape and evaluation", p.Global)
	}
	if len(p.RuleFiles) != 1 || p.RuleFiles[0] != "/w/rules.yml" {
		t.Errorf("rule_files = %v", p.RuleFiles)
	}
	byName := map[string]int{}
	for i, j := range p.Jobs {
		byName[j.Name] = i
	}
	for _, n := range []scrapeNode{{"n1", "10.0.0.1"}, {"n2", "10.0.0.2"}} {
		i, ok := byName["expanse-"+n.ID]
		if !ok {
			t.Fatalf("no expanse job for %s: %v", n.ID, byName)
		}
		j := p.Jobs[i]
		if j.Scheme != "https" || j.TLSConfig.ServerName != n.ID || j.TLSConfig.CAFile != "/w/ca.pem" ||
			j.Authorization.CredentialsFile != "/w/token" {
			t.Errorf("job %s = %+v", j.Name, j)
		}
		if len(j.Static) != 1 || j.Static[0].Targets[0] != n.Host+":7447" || j.Static[0].Labels["node"] != n.ID {
			t.Errorf("job %s targets = %+v", j.Name, j.Static)
		}
	}
	ne := p.Jobs[byName["node-exporter"]]
	if len(ne.Static) != 2 || ne.Static[1].Targets[0] != "10.0.0.2:9100" || ne.Static[1].Labels["node"] != "n2" {
		t.Errorf("node-exporter job = %+v", ne)
	}
	self := p.Jobs[byName["prometheus"]]
	if len(self.Static) != 1 || self.Static[0].Targets[0] != "127.0.0.1:9090" {
		t.Errorf("self job = %+v", self)
	}
}

func TestPrometheusConfigSkipsTheAgentJobsWithoutAToken(t *testing.T) {
	p := parseProm(t, prometheusConfig(promSetup{
		Nodes: []scrapeNode{{"n1", "10.0.0.1"}}, Interval: "5s", SelfPort: "9090", CAFile: "/w/ca.pem",
	}))
	for _, j := range p.Jobs {
		if strings.HasPrefix(j.Name, "expanse-") || j.Name == "node-exporter" {
			t.Errorf("unexpected job %q: no token and node-exporter disabled", j.Name)
		}
	}
}

func TestWritePrometheusFilesLaysOutTheWorkDir(t *testing.T) {
	dir := t.TempDir()
	s, err := writePrometheusFiles(dir, map[string]any{"metricsToken": "s3cret", "scrapeInterval": "2s", "nodeExporterPort": float64(9100)},
		[]string{"--scrape-node", "n1=10.0.0.1", "--scrape-ca", "CA-PEM\n"}, "9090")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{s.CAFile: "CA-PEM\n", s.TokenFile: "s3cret"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
	if info, err := os.Stat(s.TokenFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, %v; want 0600", info.Mode(), err)
	}
	if rules, err := os.ReadFile(s.RulesFile); err != nil || !strings.Contains(string(rules), "ExpanseNodeUnhealthy") {
		t.Errorf("rules file missing the shipped rules: %v", err)
	}
	if s.Interval != "2s" || s.NodeExporterPort != 9100 || len(s.Nodes) != 1 || filepath.Dir(s.CAFile) != dir {
		t.Errorf("setup = %+v", s)
	}
}

func TestWritePrometheusFilesDefaultsTheIntervalAndOmitsAMissingToken(t *testing.T) {
	s, err := writePrometheusFiles(t.TempDir(), map[string]any{}, nil, "9090")
	if err != nil {
		t.Fatal(err)
	}
	if s.Interval != "5s" || s.NodeExporterPort != 9100 || s.TokenFile != "" || s.CAFile != "" {
		t.Errorf("setup = %+v, want 5s, node-exporter on 9100 and no token or CA", s)
	}
	off, err := writePrometheusFiles(t.TempDir(), map[string]any{"scrapeNodeExporter": false}, nil, "9090")
	if err != nil || off.NodeExporterPort != 0 {
		t.Errorf("scrapeNodeExporter false = %+v, %v; want the job disabled", off, err)
	}
}

func TestCfgPortOrFallsBackOnlyWhenUnset(t *testing.T) {
	if got := cfgPortOr(map[string]any{}, "9090"); got != "9090" {
		t.Errorf("unset port = %q, want the default", got)
	}
	if got := cfgPortOr(map[string]any{"port": float64(19090)}, "9090"); got != "19090" {
		t.Errorf("set port = %q", got)
	}
}
