package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Mirrors nix/blocks/net/haproxy/schema.json, so haproxy.cfg is never built from unchecked text.
var (
	haproxyName      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	haproxyServer    = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
	haproxyCheckPath = regexp.MustCompile(`^/[^\s"]*$`)
	haproxyBalances  = map[string]bool{"roundrobin": true, "leastconn": true, "source": true, "first": true}
)

// haproxyFrontend is one entry of spec.config.frontends.
type haproxyFrontend struct {
	Name, Mode, Balance, CheckPath string
	Port                           int
	Servers                        []string
	Check                          bool
}

// haproxySetup is everything one net/haproxy instance needs.
type haproxySetup struct {
	MaxConn   int
	StatsPort string
	Frontends []haproxyFrontend
	Raw       string
}

// haproxySetupFrom applies the block's defaults to spec.config and validates it.
func haproxySetupFrom(cfg map[string]any) (haproxySetup, error) {
	s := haproxySetup{MaxConn: cfgInt(cfg, "maxconn", 4096), Raw: cfgStr(cfg, "config")}
	if p := cfgInt(cfg, "statsPort", 0); p > 0 {
		s.StatsPort = fmt.Sprint(p)
	}
	list, hasFrontends := cfg["frontends"].([]any)
	switch {
	case s.Raw != "" && hasFrontends:
		return haproxySetup{}, errors.New("net/haproxy: set frontends or config, not both")
	case s.Raw != "":
		return s, nil
	case len(list) == 0:
		return haproxySetup{}, errors.New("net/haproxy: frontends (or config) is required")
	}
	names, ports := map[string]bool{}, map[string]bool{s.StatsPort: s.StatsPort != ""}
	for i, raw := range list {
		f, err := haproxyFrontendFrom(raw)
		if err != nil {
			return haproxySetup{}, fmt.Errorf("net/haproxy: frontends[%d]: %w", i, err)
		}
		port := fmt.Sprint(f.Port)
		if names[f.Name] || ports[port] {
			return haproxySetup{}, fmt.Errorf("net/haproxy: frontends[%d]: name %s or port %s is already used", i, f.Name, port)
		}
		names[f.Name], ports[port] = true, true
		s.Frontends = append(s.Frontends, f)
	}
	return s, nil
}

func haproxyFrontendFrom(raw any) (haproxyFrontend, error) {
	m, _ := raw.(map[string]any)
	f := haproxyFrontend{
		Name: cfgStr(m, "name"), Mode: cfgStr(m, "mode"), Balance: cfgStr(m, "balance"),
		CheckPath: cfgStr(m, "checkPath"), Port: cfgInt(m, "port", 0), Check: cfgBool(m, "check", true),
	}
	if f.Mode == "" {
		f.Mode = "http"
	}
	if f.Balance == "" {
		f.Balance = "roundrobin"
	}
	if f.CheckPath == "" {
		f.CheckPath = "/"
	}
	servers, _ := m["servers"].([]any)
	for _, sv := range servers {
		str, _ := sv.(string)
		if !haproxyServer.MatchString(str) {
			return haproxyFrontend{}, fmt.Errorf("server %q must be host:port", str)
		}
		f.Servers = append(f.Servers, str)
	}
	switch {
	case !haproxyName.MatchString(f.Name):
		return haproxyFrontend{}, fmt.Errorf("name %q must match %s", f.Name, haproxyName)
	case f.Port < 1 || f.Port > 65535:
		return haproxyFrontend{}, fmt.Errorf("port %d is out of range", f.Port)
	case f.Mode != "http" && f.Mode != "tcp":
		return haproxyFrontend{}, fmt.Errorf("mode %q must be http or tcp", f.Mode)
	case !haproxyBalances[f.Balance]:
		return haproxyFrontend{}, fmt.Errorf("balance %q is not supported", f.Balance)
	case len(f.Servers) == 0:
		return haproxyFrontend{}, errors.New("servers is empty")
	case !haproxyCheckPath.MatchString(f.CheckPath):
		return haproxyFrontend{}, fmt.Errorf("checkPath %q must be a path", f.CheckPath)
	}
	return f, nil
}

// haproxyConfig renders haproxy.cfg, or returns the raw one unchanged.
func haproxyConfig(s haproxySetup) string {
	if s.Raw != "" {
		return s.Raw
	}
	var b strings.Builder
	fmt.Fprintf(&b, "global\n\tmaxconn %d\n\tlog stdout format raw local0 notice\n\n", s.MaxConn)
	b.WriteString("defaults\n\tlog global\n\ttimeout connect 5s\n\ttimeout client 60s\n\ttimeout server 60s\n" +
		"\tdefault-server init-addr last,libc,none\n")
	for _, f := range s.Frontends {
		fmt.Fprintf(&b, "\nfrontend fe_%s\n\tbind :%d\n\tmode %s\n", f.Name, f.Port, f.Mode)
		if f.Mode == "http" {
			b.WriteString("\toption forwardfor\n")
		}
		fmt.Fprintf(&b, "\tdefault_backend be_%s\n\nbackend be_%s\n\tmode %s\n\tbalance %s\n", f.Name, f.Name, f.Mode, f.Balance)
		if f.Check && f.Mode == "http" {
			fmt.Fprintf(&b, "\toption httpchk\n\thttp-check send meth GET uri %s\n", f.CheckPath)
		}
		for i, sv := range f.Servers {
			fmt.Fprintf(&b, "\tserver s%d %s", i+1, sv)
			if f.Check {
				b.WriteString(" check inter 2s fall 2 rise 2")
			}
			b.WriteString("\n")
		}
	}
	if s.StatsPort != "" {
		fmt.Fprintf(&b, "\nfrontend stats\n\tbind :%s\n\tmode http\n"+
			"\thttp-request use-service prometheus-exporter if { path /metrics }\n"+
			"\tstats enable\n\tstats uri /stats\n\tstats refresh 10s\n", s.StatsPort)
	}
	return b.String()
}

// runHAProxy serves net/haproxy in the foreground; it keeps no state.
func runHAProxy(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	s, err := haproxySetupFrom(cfg)
	if err != nil {
		return err
	}
	work := "/tmp/expblk-" + instance
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "haproxy.cfg")
	if err := os.WriteFile(conf, []byte(haproxyConfig(s)), 0o600); err != nil {
		return err
	}
	fmt.Printf("expanse-block-run: haproxy serving %d frontends\n", len(s.Frontends))
	return execWorkload(ctx, "haproxy", []string{"-db", "-f", conf})
}
