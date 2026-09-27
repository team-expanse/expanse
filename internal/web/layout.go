// The shared app shell: one layout template every authenticated page
// renders inside (sidebar, header, breadcrumbs, footer), the per-page
// chrome data that fills it, flash toasts across redirects, and the
// styled error pages.
package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/expanse/expanse/internal/cluster/nodelc"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/version"
)

// crumb is one breadcrumb; the last one has no Href.
type crumb struct {
	Label string
	Href  string
}

// flash is a one-shot notice carried across a redirect (flashCookie).
type flash struct {
	Kind string `json:"kind"` // success | error | info
	Text string `json:"text"`
}

// clusterChrome is the header's cluster name and quorum indicator.
type clusterChrome struct {
	Name   string
	Quorum string // "3/3", empty when unavailable
	Pill   pill
}

// page is the chrome every authenticated page shares; page data structs
// embed it as their Page field.
type page struct {
	Title     string
	Active    string // sidebar key: dashboard, cluster, nodes, blocks, volumes, generations, events, health, settings
	Crumbs    []crumb
	NodeID    string
	CSRFToken string
	User      string
	Version   string
	Cluster   clusterChrome
	Flash     *flash
}

// flashCookie carries a flash across the redirect that follows a
// successful non-HTMX form post; read and cleared by the next page.
const flashCookie = "expanse_flash"

// templates holds one template set per page (the shared layout and
// partials cloned, plus that page's own "content" and SSE fragments).
type templates struct {
	byName map[string]*template.Template
}

// loadTemplates parses layout.html and every _partial into a base set,
// then clones it per page file so each page's "content" is distinct.
func loadTemplates(fsys fs.FS) (*templates, error) {
	base, err := template.New("").Funcs(templateFuncs()).ParseFS(fsys, "templates/layout.html", "templates/_*.html")
	if err != nil {
		return nil, err
	}
	pages, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	t := &templates{byName: map[string]*template.Template{}}
	for _, p := range pages {
		name := strings.TrimPrefix(p, "templates/")
		if name == "layout.html" || strings.HasPrefix(name, "_") {
			continue
		}
		set, err := base.Clone()
		if err != nil {
			return nil, err
		}
		if _, err := set.ParseFS(fsys, p); err != nil {
			return nil, err
		}
		t.byName[name] = set
		for _, def := range set.Templates() {
			if _, taken := t.byName[def.Name()]; !taken && def.Name() != "content" {
				t.byName[def.Name()] = set
			}
		}
	}
	return t, nil
}

// execute renders name: a page file (its "content" inside "layout", or
// the file itself when it defines no content, e.g. login) or a fragment.
func (t *templates) execute(w io.Writer, name string, data any) error {
	set, ok := t.byName[name]
	if !ok {
		return fmt.Errorf("web: no template %q", name)
	}
	if strings.HasSuffix(name, ".html") && set.Lookup("content") != nil {
		return set.ExecuteTemplate(w, "layout", data)
	}
	return set.ExecuteTemplate(w, name, data)
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"phase":      phasePill,
		"volstate":   volumePill,
		"health":     healthPill,
		"nodeState":  nodePill,
		"nodeHealth": nodeHealthPill,
		"agentState": agentPill,
		"role":       roleLabel,
		"isLeader":   isLeader,
		"bytes":      humanBytesAny,
		"mib":        func(n int64) string { return humanBytesAny(n << 20) },
		"kib":        func(n uint64) string { return humanBytesAny(n << 10) },
		"ago":        ago,
		"stamp":      stamp,
		"nanos":      func(ns int64) time.Time { return time.Unix(0, ns) },
		"lower":      strings.ToLower,
		"join":       strings.Join,
		"add":        func(a, b int) int { return a + b },
		"sub64":      func(a, b int64) int64 { return a - b },
		"pct":        func(a, b int64) int { return int(pctOf(a, b)) },
		"nav":        navGroups,
		"short": func(s string, n int) string {
			if len(s) > n {
				return s[:n]
			}
			return s
		},
	}
}

// humanBytesAny renders a measured byte count rounded to one decimal in binary units ("894.3 GiB").
func humanBytesAny(v any) string {
	var n float64
	switch x := v.(type) {
	case int64:
		n = float64(x)
	case uint64:
		n = float64(x)
	case int:
		n = float64(x)
	default:
		return fmt.Sprint(v)
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}
	i := 0
	for ; n >= 1024 && i < len(units)-1; i++ {
		n /= 1024
	}
	s := strconv.FormatFloat(n, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + units[i]
}

func pctOf(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) * 100 / float64(b)
}

// ago renders a time relative to now ("3m ago"); zero is "never".
func ago(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// stamp is the one absolute timestamp format the UI uses.
func stamp(t time.Time) string {
	if t.IsZero() || t.Unix() <= 0 {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// newPage builds the shared chrome for a request: session, cluster
// indicator, any pending flash (which it clears), and the breadcrumbs.
func (s *Server) newPage(w http.ResponseWriter, r *http.Request, title, active string, crumbs ...crumb) page {
	sess := sessionFromContext(r.Context())
	return page{
		Title: title, Active: active, Crumbs: crumbs,
		NodeID: s.NodeID, CSRFToken: sess.CSRFToken, User: sess.UserID,
		Version: version.Get().Version,
		Cluster: s.clusterChrome(r),
		Flash:   takeFlash(w, r),
	}
}

// clusterChrome summarises quorum for the header from the same report
// the cluster page renders, read stale so a degraded node still paints.
func (s *Server) clusterChrome(r *http.Request) clusterChrome {
	if s.cluster == nil {
		return clusterChrome{Name: "Expanse", Pill: pill{"Standalone", "neutral"}}
	}
	rep, err := s.getClusterReport(store.WithStale(r.Context()))
	if err != nil {
		return clusterChrome{Name: "Expanse", Pill: pill{"Status unavailable", "warn"}}
	}
	c := clusterChrome{Name: rep.Name, Quorum: fmt.Sprintf("%d/%d", rep.QuorumHave, rep.QuorumNeed), Pill: pill{"Healthy", "ok"}}
	if c.Name == "" {
		c.Name = "Expanse"
	}
	switch {
	case rep.Leader == "":
		c.Pill = pill{"No leader", "crit"}
	case rep.Degraded:
		c.Pill = pill{"Degraded", "warn"}
	default:
		for _, n := range rep.Nodes {
			if n.Lifecycle == nodelc.StateUnreachable || n.Lifecycle == nodelc.StateFailed {
				c.Pill = pill{"Node down", "warn"}
			}
		}
	}
	return c
}

func setFlash(w http.ResponseWriter, kind, text string) {
	raw, _ := json.Marshal(flash{Kind: kind, Text: text})
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: base64.RawURLEncoding.EncodeToString(raw), Path: "/", MaxAge: 60,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

// takeFlash reads and clears the flash cookie, if any.
func takeFlash(w http.ResponseWriter, r *http.Request) *flash {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return nil
	}
	var f flash
	if json.Unmarshal(raw, &f) != nil || f.Text == "" {
		return nil
	}
	return &f
}

// isHTMX reports whether htmx issued the request (a fragment or a
// hx-boosted navigation), which changes how results are signalled.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// done finishes a successful mutation: htmx callers get a toast via
// HX-Trigger (and HX-Redirect when the page changes), plain form posts
// get a flash cookie and a 303 — the same outcome either way.
func (s *Server) done(w http.ResponseWriter, r *http.Request, kind, text, redirect string) {
	if !isHTMX(r) {
		setFlash(w, kind, text)
		http.Redirect(w, r, redirect, http.StatusSeeOther)
		return
	}
	if redirect != "" && redirect != r.Header.Get("HX-Current-URL") && !strings.HasSuffix(r.Header.Get("HX-Current-URL"), redirect) {
		setFlash(w, kind, text)
		w.Header().Set("HX-Redirect", redirect)
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("HX-Trigger", toastTrigger(kind, text))
	w.WriteHeader(http.StatusNoContent)
}

// toastTrigger encodes a toast for the HX-Trigger header; app.js
// listens for the "toast" event on the body.
func toastTrigger(kind, text string) string {
	raw, _ := json.Marshal(map[string]flash{"toast": {Kind: kind, Text: text}})
	return string(raw)
}

type errorData struct {
	Page    page
	Status  int
	Title   string
	Message string
}

// renderError answers a failed page request: plain text for htmx (so it
// surfaces as a toast, never a page swapped into a fragment), otherwise
// the styled error page inside the shell.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if isHTMX(r) || r.Method != http.MethodGet {
		http.Error(w, msg, status)
		return
	}
	title := http.StatusText(status)
	if status == http.StatusNotFound {
		title = "Page not found"
	}
	s.render(w, status, "error.html", errorData{
		Page: s.newPage(w, r, title, ""), Status: status, Title: title, Message: msg,
	})
}

// secureHeaders adds the CSP that keeps every script and style in the
// embedded static files (no inline script, no CDN), plus the usual
// framing/sniffing guards.
func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// navItem is one sidebar destination; navGroups is the sidebar, in
// display order, exposed to the layout as the "nav" template func.
type navItem struct {
	Key, Label, Href, Icon string
}

type navGroup struct {
	Label string
	Items []navItem
}

func navGroups() []navGroup {
	return []navGroup{
		{Label: "Overview", Items: []navItem{
			{Key: "dashboard", Label: "Dashboard", Href: "/", Icon: "i-dashboard"},
			{Key: "cluster", Label: "Cluster", Href: "/cluster", Icon: "i-cluster"},
			{Key: "nodes", Label: "Nodes", Href: "/nodes", Icon: "i-nodes"},
			{Key: "health", Label: "Health & alerts", Href: "/health", Icon: "i-health"},
		}},
		{Label: "Workloads", Items: []navItem{
			{Key: "blocks", Label: "Blocks", Href: "/blocks", Icon: "i-blocks"},
			{Key: "volumes", Label: "Volumes", Href: "/volumes", Icon: "i-volumes"},
		}},
		{Label: "Operations", Items: []navItem{
			{Key: "generations", Label: "Generations", Href: "/generations", Icon: "i-generations"},
			{Key: "events", Label: "Events", Href: "/events", Icon: "i-events"},
			{Key: "settings", Label: "Settings", Href: "/settings", Icon: "i-settings"},
		}},
	}
}
