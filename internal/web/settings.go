// Settings: the account's admin password (the browser-side counterpart
// of `expanse ctl admin reset-password`), and Cluster › Settings: the
// OIDC configuration as stored (never its secret), the UI CA
// certificate operators import into a browser, and the LAN range.
package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/expanse/expanse/internal/cluster/control"
	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/auth"
	"github.com/expanse/expanse/internal/web/oidc"
)

// minPasswordLen matches nothing stricter than the generated password
// is; it only rules out trivially short choices.
const minPasswordLen = 12

func (s *Server) registerSettingsRoutes() {
	s.routes.Handle("GET /settings", s.requireAuth(http.HandlerFunc(s.handleSettings)))
	s.routes.Handle("POST /settings/password", s.requireAuth(http.HandlerFunc(s.handlePasswordChange)))
	s.routes.Handle("GET /cluster/settings", s.requireAuth(http.HandlerFunc(s.handleClusterSettings)))
	s.routes.Handle("GET /cluster/ui-ca.pem", s.requireAuth(http.HandlerFunc(s.handleUICADownload)))
}

type settingsData struct {
	Page page
	// Password form state: which field the error belongs to, if any.
	Error   string
	Field   string
	SSOUser bool
}

type clusterSettingsData struct {
	Page page
	OIDC *oidc.Config
	// CAPath is where the certificate is written on each node's disk;
	// CAAvailable says the store holds it, so the download link works.
	CAPath      string
	CAAvailable bool
	Network     networkView
}

func (s *Server) settingsData(w http.ResponseWriter, r *http.Request) settingsData {
	d := settingsData{Page: s.newPage(w, r, "Settings", "settings", crumb{Label: "Settings"})}
	d.SSOUser = d.Page.User != auth.AdminUsername
	return d
}

func (s *Server) clusterSettingsData(w http.ResponseWriter, r *http.Request) clusterSettingsData {
	d := clusterSettingsData{Page: s.newPage(w, r, "Cluster settings", "cluster-settings", crumb{Label: "Cluster settings"})}
	if cfg, err := oidc.LoadConfig(r.Context(), s.store); err == nil {
		d.OIDC = cfg
	}
	d.CAPath = filepath.Join("<data-dir>", control.UICAFile)
	if s.DataDir != "" {
		d.CAPath = filepath.Join(s.DataDir, control.UICAFile)
	}
	_, err := s.store.Get(store.WithStale(r.Context()), store.Key(control.UICAKey))
	d.CAAvailable = err == nil
	d.Network = s.networkView(r.Context())
	return d
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "settings.html", s.settingsData(w, r))
}

func (s *Server) handleClusterSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "cluster_settings.html", s.clusterSettingsData(w, r))
}

// handlePasswordChange re-verifies the current password before writing
// the new hash through the same auth.SetAdminPassword the CLI reset uses.
func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	current, password, confirm := r.PostForm.Get("current"), r.PostForm.Get("password"), r.PostForm.Get("confirm")
	fail := func(field, msg string) {
		d := s.settingsData(w, r)
		d.Error, d.Field = msg, field
		s.render(w, http.StatusBadRequest, "settings.html", d)
	}
	ok, busy := s.checkLogin(r.Context(), auth.AdminUsername, current)
	switch {
	case busy:
		http.Error(w, "Too many password checks at once; try again shortly.", http.StatusTooManyRequests)
		return
	case !ok:
		time.Sleep(loginFailDelay)
		fail("current", "The current password is not correct.")
		return
	case utf8.RuneCountInString(password) < minPasswordLen:
		fail("password", "Use at least 12 characters.")
		return
	case password != confirm:
		fail("confirm", "The two new passwords do not match.")
		return
	}
	if err := auth.SetAdminPassword(r.Context(), s.store, password); err != nil {
		fail("", errText(err))
		return
	}
	s.done(w, r, "success", "Password changed. Existing sessions stay signed in until they expire.", "/settings")
}

// handleUICADownload serves only the CA certificate from the store's
// sealed record; the private key stays sealed and is never read here.
func (s *Server) handleUICADownload(w http.ResponseWriter, r *http.Request) {
	e, err := s.store.Get(store.WithStale(r.Context()), store.Key(control.UICAKey))
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "The UI CA has not been created on this cluster.")
		return
	}
	var rec control.CAEntry
	if json.Unmarshal(e.Value, &rec) != nil || len(rec.CertPEM) == 0 {
		s.renderError(w, r, http.StatusInternalServerError, "The UI CA record is corrupt.")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="ui-ca.pem"`)
	_, _ = w.Write(rec.CertPEM)
}
