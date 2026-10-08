package main

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCaddySetupAppliesDefaults(t *testing.T) {
	s, err := caddySetupFrom(map[string]any{"sites": []any{
		map[string]any{"host": "app.example.com", "reverseProxy": []any{"10.0.0.5:8080", "app2:8080"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if s.Port != "18080" || s.HTTPSPort != "18443" {
		t.Errorf("ports = %s/%s, want 18080/18443", s.Port, s.HTTPSPort)
	}
	if len(s.Sites) != 1 || s.Sites[0].TLS != "acme" || len(s.Sites[0].ReverseProxy) != 2 {
		t.Errorf("sites = %+v", s.Sites)
	}
}

func TestCaddySetupRejectsBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		{},
		{"sites": []any{}},
		{"sites": []any{map[string]any{"host": "a b", "respond": "ok"}}},
		{"sites": []any{map[string]any{"host": "a.example.com"}}},
		{"sites": []any{map[string]any{"host": "a.example.com", "respond": "ok", "reverseProxy": []any{"x:1"}}}},
		{"sites": []any{map[string]any{"host": "a.example.com", "respond": "ok", "tls": "maybe"}}},
		{"sites": []any{map[string]any{"host": "a.example.com", "reverseProxy": []any{"x:1 }"}}}},
		{"sites": []any{map[string]any{"host": "a.example.com", "respond": "a\"b"}}},
		{"sites": []any{map[string]any{"host": "a.example.com", "respond": "ok"}}, "caddyfile": ":80"},
		{"sites": []any{map[string]any{"host": "a.example.com", "respond": "ok"}}, "email": "x y@z"},
	} {
		if _, err := caddySetupFrom(cfg); err == nil {
			t.Errorf("%v accepted", cfg)
		}
	}
}

func TestCaddyfileRedirectsToTheDefaultHTTPSPort(t *testing.T) {
	s, err := caddySetupFrom(map[string]any{
		"port": 18080.0, "httpsPort": 18443.0, "email": "ops@example.com",
		"sites": []any{map[string]any{"host": "app.example.com", "tls": "internal", "reverseProxy": []any{"10.0.0.5:8080"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := caddyfile(s)
	for _, want := range []string{
		"http_port 18080", "https_port 18443", "email ops@example.com", "skip_install_trust",
		// Caddy's own redirect would name :18443; clients reach VIP:443.
		"auto_https disable_redirects",
		"http://app.example.com {\n\tredir https://{host}{uri} 308\n}",
		"https://app.example.com {\n\ttls internal\n\treverse_proxy 10.0.0.5:8080",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Caddyfile missing %q:\n%s", want, got)
		}
	}
}

func TestCaddyfilePlainSiteServesHTTPOnly(t *testing.T) {
	s, err := caddySetupFrom(map[string]any{"sites": []any{
		map[string]any{"host": "plain.example.com", "tls": "off", "respond": "hello there"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := caddyfile(s)
	if !strings.Contains(got, "http://plain.example.com {\n\trespond \"hello there\" 200\n}") {
		t.Errorf("plain site:\n%s", got)
	}
	if strings.Contains(got, "https://plain.example.com") || strings.Contains(got, "redir https") {
		t.Errorf("plain site got HTTPS:\n%s", got)
	}
}

func TestCaddyfileACMESiteOmitsTLSDirective(t *testing.T) {
	s, err := caddySetupFrom(map[string]any{
		"acmeCA": "https://acme-staging-v02.api.letsencrypt.org/directory",
		"sites":  []any{map[string]any{"host": "app.example.com", "reverseProxy": []any{"a:1", "b:2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := caddyfile(s)
	if !strings.Contains(got, "acme_ca https://acme-staging-v02.api.letsencrypt.org/directory") {
		t.Errorf("acme_ca missing:\n%s", got)
	}
	if !strings.Contains(got, "https://app.example.com {\n\treverse_proxy a:1 b:2 {\n\t\tlb_policy round_robin") {
		t.Errorf("acme site:\n%s", got)
	}
	if strings.Contains(got, "tls internal") {
		t.Errorf("acme site uses the internal CA:\n%s", got)
	}
}

func TestCaddyfileRawIsUsedVerbatim(t *testing.T) {
	raw := ":18080 {\n\trespond ok\n}\n"
	s, err := caddySetupFrom(map[string]any{"caddyfile": raw})
	if err != nil {
		t.Fatal(err)
	}
	if got := caddyfile(s); got != raw {
		t.Errorf("caddyfile = %q, want %q", got, raw)
	}
}

func TestCaddyEnvKeepsDataOnTheVolume(t *testing.T) {
	env := caddyEnv("/mnt/vol", "/tmp/expblk-x")
	for _, want := range []string{"XDG_DATA_HOME=/mnt/vol", "XDG_CONFIG_HOME=/tmp/expblk-x/config", "HOME=/tmp/expblk-x"} {
		found := false
		for _, e := range env {
			found = found || e == want
		}
		if !found {
			t.Errorf("env %v missing %s", env, want)
		}
	}
}

func TestSyncLoopSyncsUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	done := make(chan struct{})
	go func() {
		syncLoop(ctx, t.TempDir(), time.Millisecond, func(int) error { calls.Add(1); return nil })
		close(done)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if calls.Load() < 3 {
		t.Errorf("synced %d times, want at least 3", calls.Load())
	}
}
