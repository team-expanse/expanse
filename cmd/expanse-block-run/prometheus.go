package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/expanse/expanse/deploy"
	"github.com/expanse/expanse/internal/config"
)

// scrapeNode is one cluster node the bridge handed us (--scrape-node id=host).
type scrapeNode struct {
	ID   string
	Host string
}

func scrapeNodes(args []string) []scrapeNode {
	var out []scrapeNode
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--scrape-node" {
			continue
		}
		if id, host, ok := strings.Cut(args[i+1], "="); ok && id != "" && host != "" {
			out = append(out, scrapeNode{id, host})
		}
	}
	return out
}

func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// promSetup is everything prometheus.yml is generated from.
type promSetup struct {
	Nodes            []scrapeNode
	Interval         string
	SelfPort         string
	NodeExporterPort int // 0 disables the node-exporter job
	CAFile           string
	TokenFile        string // "" skips the agents' /metrics (no token configured)
	RulesFile        string
}

type promStatic struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels,omitempty"`
}

type promJob struct {
	Name          string            `yaml:"job_name"`
	Scheme        string            `yaml:"scheme,omitempty"`
	TLSConfig     map[string]string `yaml:"tls_config,omitempty"`
	Authorization map[string]string `yaml:"authorization,omitempty"`
	Static        []promStatic      `yaml:"static_configs"`
}

// prometheusConfig renders prometheus.yml: one job per node for the agent's
// /metrics, since each node's cert names only that node.
func prometheusConfig(s promSetup) []byte {
	jobs := []promJob{{Name: "prometheus", Static: []promStatic{{Targets: []string{"127.0.0.1:" + s.SelfPort}}}}}
	if s.TokenFile != "" && s.CAFile != "" {
		for _, n := range s.Nodes {
			jobs = append(jobs, promJob{
				Name:          "expanse-" + n.ID,
				Scheme:        "https",
				TLSConfig:     map[string]string{"ca_file": s.CAFile, "server_name": n.ID},
				Authorization: map[string]string{"credentials_file": s.TokenFile},
				Static: []promStatic{{
					Targets: []string{n.Host + ":" + strconv.Itoa(config.PortMetrics)},
					Labels:  map[string]string{"node": n.ID},
				}},
			})
		}
	}
	if s.NodeExporterPort > 0 && len(s.Nodes) > 0 {
		ne := promJob{Name: "node-exporter"}
		for _, n := range s.Nodes {
			ne.Static = append(ne.Static, promStatic{
				Targets: []string{n.Host + ":" + strconv.Itoa(s.NodeExporterPort)},
				Labels:  map[string]string{"node": n.ID},
			})
		}
		jobs = append(jobs, ne)
	}
	doc := map[string]any{
		"global":         map[string]string{"scrape_interval": s.Interval, "evaluation_interval": s.Interval},
		"scrape_configs": jobs,
	}
	if s.RulesFile != "" {
		doc["rule_files"] = []string{s.RulesFile}
	}
	out, _ := yaml.Marshal(doc) // plain maps and structs always marshal
	return out
}

// writePrometheusFiles puts the CA bundle, token and shipped rules in dir
// and returns the setup that references them.
func writePrometheusFiles(dir string, cfg map[string]any, args []string, port string) (promSetup, error) {
	s := promSetup{
		Nodes:            scrapeNodes(args),
		Interval:         cfgStr(cfg, "scrapeInterval"),
		SelfPort:         port,
		NodeExporterPort: cfgInt(cfg, "nodeExporterPort", 9100),
		RulesFile:        filepath.Join(dir, "expanse-alerts.rules.yml"),
	}
	if !cfgBool(cfg, "scrapeNodeExporter", true) {
		s.NodeExporterPort = 0
	}
	if s.Interval == "" {
		s.Interval = "5s" // OBSERVABILITY.md: ≤5s keeps alerts inside the 30s budget
	}
	files := map[string][]byte{s.RulesFile: deploy.AlertRules}
	if ca := argValue(args, "--scrape-ca"); ca != "" {
		s.CAFile = filepath.Join(dir, "ca.pem")
		files[s.CAFile] = []byte(ca)
	}
	if tok := cfgStr(cfg, "metricsToken"); tok != "" {
		s.TokenFile = filepath.Join(dir, "metrics-token")
		files[s.TokenFile] = []byte(tok)
	}
	for path, body := range files {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return promSetup{}, err
		}
	}
	return s, nil
}

// runPrometheus serves monitor/prometheus: scrape every node's agent and
// node-exporter, evaluate the shipped alert rules, keep the TSDB on the volume.
func runPrometheus(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	port := cfgPortOr(cfg, "9090")
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
	s, err := writePrometheusFiles(work, cfg, args, port)
	if err != nil {
		return err
	}
	conf := filepath.Join(work, "prometheus.yml")
	if err := os.WriteFile(conf, prometheusConfig(s), 0o600); err != nil {
		return err
	}
	retention := cfgStr(cfg, "retention")
	if retention == "" {
		retention = "15d"
	}
	fmt.Printf("expanse-block-run: prometheus serving on :%s, scraping %d nodes\n", port, len(s.Nodes))
	return execWorkload(ctx, "prometheus", []string{
		"--config.file=" + conf,
		"--storage.tsdb.path=" + data,
		"--storage.tsdb.retention.time=" + retention,
		"--web.listen-address=0.0.0.0:" + port,
	})
}
