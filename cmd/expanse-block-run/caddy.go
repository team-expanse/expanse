package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Mirrors nix/blocks/web/caddy/schema.json, so a Caddyfile is never built from unchecked text.
var (
	caddyHost     = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	caddyUpstream = regexp.MustCompile(`^(https?://)?[A-Za-z0-9.-]+:[0-9]{1,5}$`)
	caddyRespond  = regexp.MustCompile(`^[^\n\r"]*$`)
	caddyEmail    = regexp.MustCompile(`^[^@\s"]+@[^@\s"]+$`)
	caddyACMECA   = regexp.MustCompile(`^https://[^\s"]+$`)
)

// caddySite is one entry of spec.config.sites.
type caddySite struct {
	Host, TLS, Respond string
	ReverseProxy       []string
}

// caddySetup is everything one web/caddy instance needs.
type caddySetup struct {
	Port, HTTPSPort, Email, ACMECA string
	Sites                          []caddySite
	Raw                            string
}

// caddySetupFrom applies the block's defaults to spec.config and validates it.
func caddySetupFrom(cfg map[string]any) (caddySetup, error) {
	s := caddySetup{
		Port:      cfgPortOr(cfg, "18080"),
		HTTPSPort: fmt.Sprint(cfgInt(cfg, "httpsPort", 18443)),
		Email:     cfgStr(cfg, "email"),
		ACMECA:    cfgStr(cfg, "acmeCA"),
		Raw:       cfgStr(cfg, "caddyfile"),
	}
	list, hasSites := cfg["sites"].([]any)
	switch {
	case s.Raw != "" && hasSites:
		return caddySetup{}, errors.New("web/caddy: set sites or caddyfile, not both")
	case s.Raw != "":
		return s, nil
	case len(list) == 0:
		return caddySetup{}, errors.New("web/caddy: sites (or caddyfile) is required")
	case s.Email != "" && !caddyEmail.MatchString(s.Email):
		return caddySetup{}, fmt.Errorf("web/caddy: email %q is not an address", s.Email)
	case s.ACMECA != "" && !caddyACMECA.MatchString(s.ACMECA):
		return caddySetup{}, fmt.Errorf("web/caddy: acmeCA %q must be an https URL", s.ACMECA)
	}
	for i, raw := range list {
		site, err := caddySiteFrom(raw)
		if err != nil {
			return caddySetup{}, fmt.Errorf("web/caddy: sites[%d]: %w", i, err)
		}
		s.Sites = append(s.Sites, site)
	}
	return s, nil
}

func caddySiteFrom(raw any) (caddySite, error) {
	m, _ := raw.(map[string]any)
	site := caddySite{Host: cfgStr(m, "host"), TLS: cfgStr(m, "tls"), Respond: cfgStr(m, "respond")}
	if site.TLS == "" {
		site.TLS = "acme"
	}
	ups, _ := m["reverseProxy"].([]any)
	for _, u := range ups {
		str, _ := u.(string)
		if !caddyUpstream.MatchString(str) {
			return caddySite{}, fmt.Errorf("upstream %q must be host:port", str)
		}
		site.ReverseProxy = append(site.ReverseProxy, str)
	}
	_, hasRespond := m["respond"]
	switch {
	case !caddyHost.MatchString(site.Host):
		return caddySite{}, fmt.Errorf("host %q is not a host name", site.Host)
	case site.TLS != "acme" && site.TLS != "internal" && site.TLS != "off":
		return caddySite{}, fmt.Errorf("tls %q must be acme, internal or off", site.TLS)
	case hasRespond == (len(site.ReverseProxy) > 0):
		return caddySite{}, errors.New("set exactly one of reverseProxy and respond")
	case !caddyRespond.MatchString(site.Respond):
		return caddySite{}, errors.New("respond must be one line without double quotes")
	}
	return site, nil
}

// caddyfile renders the Caddyfile, or returns the raw one unchanged.
func caddyfile(s caddySetup) string {
	if s.Raw != "" {
		return s.Raw
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{\n\thttp_port %s\n\thttps_port %s\n", s.Port, s.HTTPSPort)
	if s.Email != "" {
		fmt.Fprintf(&b, "\temail %s\n", s.Email)
	}
	if s.ACMECA != "" {
		fmt.Fprintf(&b, "\tacme_ca %s\n", s.ACMECA)
	}
	// Caddy's own redirects would name https_port; clients reach VIP:443.
	b.WriteString("\tauto_https disable_redirects\n\tskip_install_trust\n\tpersist_config off\n\tadmin off\n}\n")
	for _, site := range s.Sites {
		scheme := "https"
		if site.TLS == "off" {
			scheme = "http"
		} else {
			fmt.Fprintf(&b, "\nhttp://%s {\n\tredir https://{host}{uri} 308\n}\n", site.Host)
		}
		fmt.Fprintf(&b, "\n%s://%s {\n", scheme, site.Host)
		if site.TLS == "internal" {
			b.WriteString("\ttls internal\n")
		}
		if site.Respond != "" || len(site.ReverseProxy) == 0 {
			fmt.Fprintf(&b, "\trespond \"%s\" 200\n", site.Respond)
		} else {
			fmt.Fprintf(&b, "\treverse_proxy %s {\n\t\tlb_policy round_robin\n\t\tfail_duration 30s\n\t}\n",
				strings.Join(site.ReverseProxy, " "))
		}
		b.WriteString("}\n")
	}
	return b.String()
}

// caddyEnv points Caddy's data directory (certificates, keys, internal CA) at the volume.
func caddyEnv(data, work string) []string {
	return []string{"XDG_DATA_HOME=" + data, "XDG_CONFIG_HOME=" + filepath.Join(work, "config"), "HOME=" + work}
}

// syncLoop flushes dir's filesystem every interval until ctx ends; Caddy never fsyncs
// the certificates and keys it writes, and a failover is a crash.
func syncLoop(ctx context.Context, dir string, interval time.Duration, syncfs func(fd int) error) {
	f, err := os.Open(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "expanse-block-run: sync loop: %v\n", err)
		return
	}
	defer f.Close()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := syncfs(int(f.Fd())); err != nil {
				fmt.Fprintf(os.Stderr, "expanse-block-run: syncfs %s: %v\n", dir, err)
			}
		}
	}
}

// runCaddy serves web/caddy with its data directory on the bound volume, if any.
func runCaddy(ctx context.Context, instance string, args []string) error {
	cfg, err := cfgMap(args)
	if err != nil {
		return err
	}
	s, err := caddySetupFrom(cfg)
	if err != nil {
		return err
	}
	work := "/tmp/expblk-" + instance
	data := firstMount(mountPaths(args))
	if data != "" {
		if err := waitForMount(data, 30*time.Second, 500*time.Millisecond, realStat); err != nil {
			return fmt.Errorf("web/caddy: %w", err)
		}
	} else {
		data = work + "-data"
	}
	if err := mkdirAllRetrying(os.MkdirAll, data, 0o700, 10, 500*time.Millisecond); err != nil {
		return fmt.Errorf("web/caddy: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "config"), 0o700); err != nil {
		return err
	}
	conf := filepath.Join(work, "Caddyfile")
	if err := os.WriteFile(conf, []byte(caddyfile(s)), 0o600); err != nil {
		return err
	}
	go syncLoop(ctx, data, 2*time.Second, unix.Syncfs)
	fmt.Printf("expanse-block-run: caddy serving on :%s and :%s\n", s.Port, s.HTTPSPort)
	return execWorkload(ctx, "caddy", []string{"run", "--adapter", "caddyfile", "--config", conf}, caddyEnv(data, work)...)
}
