package main

import (
	"strings"
	"testing"
)

func feCfg(kv ...any) map[string]any {
	m := map[string]any{"name": "web", "port": 18080.0, "servers": []any{"10.0.0.5:8080", "app2:8080"}}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestHAProxySetupAppliesDefaults(t *testing.T) {
	s, err := haproxySetupFrom(map[string]any{"frontends": []any{feCfg()}})
	if err != nil {
		t.Fatal(err)
	}
	if s.MaxConn != 4096 || s.StatsPort != "" {
		t.Errorf("maxconn %d statsPort %q", s.MaxConn, s.StatsPort)
	}
	f := s.Frontends[0]
	if f.Mode != "http" || f.Balance != "roundrobin" || !f.Check || f.CheckPath != "/" || f.Port != 18080 {
		t.Errorf("frontend = %+v", f)
	}
}

func TestHAProxySetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		{},
		{"frontends": []any{}},
		{"frontends": []any{feCfg("name", "a b")}},
		{"frontends": []any{feCfg("mode", "udp")}},
		{"frontends": []any{feCfg("balance", "random-ish")}},
		{"frontends": []any{feCfg("servers", []any{})}},
		{"frontends": []any{feCfg("servers", []any{"10.0.0.5"})}},
		{"frontends": []any{feCfg("port", 0.0)}},
		{"frontends": []any{feCfg("checkPath", "x y")}},
		{"frontends": []any{feCfg(), feCfg("port", 18081.0)}}, // duplicate name
		{"frontends": []any{feCfg(), feCfg("name", "api")}},   // duplicate port
		{"frontends": []any{feCfg("port", 18404.0)}, "statsPort": 18404.0},
		{"frontends": []any{feCfg()}, "config": "global\n"},
	} {
		if _, err := haproxySetupFrom(cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestHAProxyConfigHTTPFrontend(t *testing.T) {
	s, err := haproxySetupFrom(map[string]any{"maxconn": 100.0, "frontends": []any{feCfg("checkPath", "/healthz")}})
	if err != nil {
		t.Fatal(err)
	}
	got := haproxyConfig(s)
	for _, want := range []string{
		"global\n\tmaxconn 100\n",
		"log stdout format raw local0 notice",
		// An unresolvable server name must not stop the other frontends starting.
		"default-server init-addr last,libc,none",
		"frontend fe_web\n\tbind :18080\n\tmode http\n\toption forwardfor\n\tdefault_backend be_web\n",
		"backend be_web\n\tmode http\n\tbalance roundrobin\n\toption httpchk\n\thttp-check send meth GET uri /healthz\n",
		"\tserver s1 10.0.0.5:8080 check inter 2s fall 2 rise 2\n\tserver s2 app2:8080 check inter 2s fall 2 rise 2\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("haproxy.cfg missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "frontend stats") {
		t.Errorf("stats frontend without statsPort:\n%s", got)
	}
}

func TestHAProxyConfigTCPFrontendWithoutChecks(t *testing.T) {
	s, err := haproxySetupFrom(map[string]any{"frontends": []any{
		feCfg("name", "pg", "mode", "tcp", "balance", "leastconn", "check", false),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := haproxyConfig(s)
	if !strings.Contains(got, "backend be_pg\n\tmode tcp\n\tbalance leastconn\n\tserver s1 10.0.0.5:8080\n") {
		t.Errorf("tcp backend:\n%s", got)
	}
	if strings.Contains(got, "forwardfor") || strings.Contains(got, "httpchk") {
		t.Errorf("tcp frontend got HTTP options:\n%s", got)
	}
}

func TestHAProxyConfigStatsAndMetrics(t *testing.T) {
	s, err := haproxySetupFrom(map[string]any{"statsPort": 18404.0, "frontends": []any{feCfg()}})
	if err != nil {
		t.Fatal(err)
	}
	got := haproxyConfig(s)
	for _, want := range []string{
		"frontend stats\n\tbind :18404\n\tmode http\n",
		"http-request use-service prometheus-exporter if { path /metrics }",
		"stats uri /stats",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("haproxy.cfg missing %q:\n%s", want, got)
		}
	}
}

func TestHAProxyConfigRawIsUsedVerbatim(t *testing.T) {
	raw := "global\n\tmaxconn 10\n"
	s, err := haproxySetupFrom(map[string]any{"config": raw})
	if err != nil {
		t.Fatal(err)
	}
	if got := haproxyConfig(s); got != raw {
		t.Errorf("config = %q, want %q", got, raw)
	}
}
