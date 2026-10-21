// Package web serves the Expanse web management interface
// (ROADMAP.md Phase 2): a Go html/template + HTMX front end, TLS
// terminated with the node's own cluster-CA-issued certificate. It is a
// thin rendering layer — every handler calls into the same
// control-plane packages the CLI already uses (internal/blocks,
// internal/storage), never a parallel implementation of them.
//
// Every cluster node runs a Server; agent.go reaches it through a
// dedicated VIP (Stream A3) so losing the node currently holding that
// VIP does not take the interface down. Sessions (internal/web/auth)
// live in the cluster store, not process memory, so a cookie issued by
// one node authorizes a request answered by another after that failover.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/version"
	"github.com/expanse/expanse/internal/web/auth"
	"github.com/expanse/expanse/internal/web/oidc"
	pb "github.com/expanse/expanse/proto"
)

//go:embed static
var staticFS embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// sessionCookie is the HttpOnly cookie carrying the opaque session ID.
const sessionCookie = "expanse_session"

// csrfCookie mirrors the session's CSRF token so HTMX can read it
// (via hx-headers) and echo it back on mutating requests (D7,
// double-submit cookie). It is NOT HttpOnly for that reason; it is not
// itself a bearer credential without the paired session cookie.
const csrfCookie = "expanse_csrf"

// csrfHeader is the header HTMX must echo the csrfCookie value back in.
const csrfHeader = "X-CSRF-Token"

// csrfField carries the same token in plain (non-HTMX) form posts, which cannot set headers.
const csrfField = "csrf_token"

// oidcStateCookie and oidcNonceCookie carry the authorization-code
// flow's CSRF state and ID-token replay nonce (oidc.StartLogin) across
// the redirect to the IdP and back. SameSiteLaxMode, not Strict: the
// callback arrives as a top-level cross-site GET navigation from the
// IdP's own origin, which Strict cookies are never sent on.
const (
	oidcStateCookie = "expanse_oidc_state"
	oidcNonceCookie = "expanse_oidc_nonce"
	oidcCookiePath  = "/login/oidc"
)

// Server is the UI's HTTP server.
type Server struct {
	names *nodeNames // node IDs to hostnames, for display
	// NodeID is this node's identity, shown in the footer so an
	// operator reaching the UI through the VIP can tell which node
	// answered.
	NodeID string
	// DataDir is the agent's data directory, shown on the settings
	// page as where the UI CA certificate lives on disk; optional.
	DataDir string
	// VIPPoolFallback is this node's --external-vip-pool, in use while the cluster setting is unset.
	VIPPoolFallback string
	// GitHubURL serves <user>.keys for the gh:<user> SSH key shortcut; "" is https://github.com.
	GitHubURL string

	store         store.Store
	blocks        pb.BlockServiceServer
	catalog       pb.CatalogServiceServer
	cluster       pb.NodeServiceServer
	clusterSecret []byte
	tmpl          *templates
	events        *eventLog
	loginSlots    chan struct{} // bounds concurrent argon2id checks (32 MiB each)
	routes        *http.ServeMux
	mux           http.Handler
}

// New parses the embedded templates and registers routes. st is the
// (Raft-replicated in cluster mode) store backing sessions and the
// admin credential (internal/web/auth) — callers should ensure
// auth.EnsureAdmin has run against it before serving traffic. blocks,
// catalog and cluster are the same in-process servers the local gRPC
// socket registers (D1): nil on a node where the corresponding API is
// disabled or this is a non-cluster agent, in which case the dependent
// routes answer 503 rather than panic. clusterSecret unseals the OIDC
// client secret (internal/web/oidc, X3); nil disables OIDC login
// (the SSO button never renders, /login/oidc/* 404) without otherwise
// affecting a non-cluster agent's password login.
func New(nodeID string, st store.Store, blocks pb.BlockServiceServer, catalog pb.CatalogServiceServer, cluster pb.NodeServiceServer, clusterSecret []byte) (*Server, error) {
	names := &nodeNames{st: st}
	tmpl, err := loadTemplates(templateFS, names)
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	s := &Server{
		NodeID: nodeID, store: st, blocks: blocks, catalog: catalog, cluster: cluster,
		clusterSecret: clusterSecret, tmpl: tmpl, names: names, events: newEventLog(eventLogSize),
		loginSlots: make(chan struct{}, 2),
	}

	s.routes = http.NewServeMux()
	s.mux = secureHeaders(s.routes)
	s.routes.Handle("/static/", http.FileServer(http.FS(staticFS)))
	s.routes.HandleFunc("/login", s.handleLogin)
	s.routes.HandleFunc("/login/oidc/start", s.handleOIDCStart)
	s.routes.HandleFunc("/login/oidc/callback", s.handleOIDCCallback)
	s.routes.Handle("/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))
	s.routes.Handle("/", s.requireAuth(http.HandlerFunc(s.handleIndex)))
	s.registerDashboardRoutes()
	s.registerBlockRoutes()
	s.registerDeployRoutes()
	s.registerClusterRoutes()
	s.registerNodeRoutes()
	s.registerVolumeRoutes()
	s.registerHealthRoutes()
	s.registerGenerationRoutes()
	s.registerEventRoutes()
	s.registerSettingsRoutes()
	s.registerNetworkRoutes()
	s.registerUpgradeRoutes()
	return s, nil
}

// handleIndex serves the dashboard at exactly "/" and a styled 404 for
// any other unrouted path.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.renderError(w, r, http.StatusNotFound, "There is nothing at "+r.URL.Path+".")
		return
	}
	s.handleDashboard(w, r)
}

// render executes a page (inside the shared layout) or a fragment with
// the given status and data.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.execute(&buf, name, data); err != nil {
		http.Error(w, "render failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderFragment executes an SSE/HTMX fragment into buf.
func (s *Server) renderFragment(buf *bytes.Buffer, name string, data any) error {
	return s.tmpl.execute(buf, name, data)
}

// fragment renders a named fragment to a string, for streamFragments.
func (s *Server) fragment(name string, data any) (string, error) {
	var buf bytes.Buffer
	err := s.renderFragment(&buf, name, data)
	return buf.String(), err
}

type loginData struct {
	Error      string
	SSOEnabled bool
	Version    string
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderLogin(w, r, http.StatusOK, "")
	case http.MethodPost:
		s.handleLoginSubmit(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// badPassword is the one answer for any failed password sign-in.
const badPassword = "Invalid username or password."

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, status, "login.html", loginData{Error: msg, SSOEnabled: oidc.IsConfigured(r.Context(), s.store), Version: version.Get().Version})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, r, http.StatusBadRequest, badPassword)
		return
	}
	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	ok, busy := s.checkLogin(r.Context(), username, password)
	if busy {
		http.Error(w, "Too many sign-in attempts at once; try again shortly.", http.StatusTooManyRequests)
		return
	}
	if !ok {
		time.Sleep(loginFailDelay) // slows guessing; the slot is already free
		s.renderLogin(w, r, http.StatusUnauthorized, badPassword)
		return
	}
	sess, err := auth.IssueSession(r.Context(), s.store, username)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	setAuthCookies(w, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// loginWait is how long a sign-in waits for a free slot; loginFailDelay
// is added to every failed attempt.
var loginWait, loginFailDelay = 3 * time.Second, time.Second

// checkLogin verifies the admin password in one of the bounded slots, so a
// flood of logins cannot exhaust memory; busy means no slot came free.
func (s *Server) checkLogin(ctx context.Context, username, password string) (ok, busy bool) {
	select {
	case s.loginSlots <- struct{}{}:
		defer func() { <-s.loginSlots }()
	case <-time.After(loginWait):
		return false, true
	case <-ctx.Done():
		return false, true
	}
	return username == auth.AdminUsername && auth.VerifyAdminPassword(ctx, s.store, password), false
}

// handleOIDCStart begins the authorization-code flow (oidc.StartLogin):
// a live discovery round trip to the configured issuer, then a redirect
// to its authorization endpoint. 404s if OIDC login isn't configured,
// matching every other feature-disabled route in this server.
func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	cfg, err := oidc.LoadConfig(r.Context(), s.store)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	authURL, state, nonce, err := oidc.StartLogin(r.Context(), cfg, s.clusterSecret)
	if err != nil {
		s.renderLogin(w, r, http.StatusServiceUnavailable, "The sign-in provider could not be reached; try again, or sign in with the password.")
		return
	}
	setOIDCCookies(w, state, nonce)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOIDCCallback completes the flow (oidc.HandleCallback): validates
// the state cookie against the query parameter (CSRF), exchanges the
// code, verifies the ID token against the nonce cookie, and — on an
// authorized email — issues the exact same auth.Session password login
// does.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	cfg, err := oidc.LoadConfig(r.Context(), s.store)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	stateCookie, errState := r.Cookie(oidcStateCookie)
	nonceCookie, errNonce := r.Cookie(oidcNonceCookie)
	clearOIDCCookies(w)
	if errState != nil || errNonce != nil || r.URL.Query().Get("state") == "" ||
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(stateCookie.Value)) != 1 {
		s.renderLogin(w, r, http.StatusForbidden, "That sign-in expired or began in another tab; start the sign-in again.")
		return
	}
	if refused := r.URL.Query().Get("error"); refused != "" {
		s.renderLogin(w, r, http.StatusUnauthorized, "The sign-in provider refused: "+short(refused, 64)+".")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.renderLogin(w, r, http.StatusBadRequest, "The sign-in provider sent no code; start the sign-in again.")
		return
	}
	userID, err := oidc.HandleCallback(r.Context(), cfg, s.clusterSecret, code, nonceCookie.Value)
	if err != nil {
		s.renderLogin(w, r, http.StatusUnauthorized, "Sign-in refused: "+errMessage(err)+".")
		return
	}
	sess, err := auth.IssueSession(r.Context(), s.store, userID)
	if err != nil {
		http.Error(w, "login failed", http.StatusInternalServerError)
		return
	}
	setAuthCookies(w, sess)
	// A redirect chain the IdP started withholds SameSite=Strict cookies, so
	// our own page moves on to /, which a browser treats as same-site.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html><meta http-equiv="refresh" content="0;url=/"><a href="/">Continue</a>`)
}

func setOIDCCookies(w http.ResponseWriter, state, nonce string) {
	for name, v := range map[string]string{oidcStateCookie: state, oidcNonceCookie: nonce} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: v, Path: oidcCookiePath, MaxAge: 300,
			HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
	}
}

func clearOIDCCookies(w http.ResponseWriter) {
	for _, name := range []string{oidcStateCookie, oidcNonceCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: oidcCookiePath, MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sess := sessionFromContext(r.Context())
	_ = auth.InvalidateSession(r.Context(), s.store, sess.ID)
	clearAuthCookies(w)
	// HX-Redirect: htmx's client-side fetch follows a plain 303 itself
	// (silently re-rendering /login's body into the button's swap
	// target); this header tells it to navigate the whole page instead.
	w.Header().Set("HX-Redirect", "/login")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func setAuthCookies(w http.ResponseWriter, sess *auth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.ID, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // double-submit CSRF: the page's script must read it
		Name: csrfCookie, Value: sess.CSRFToken, Path: "/",
		HttpOnly: false, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	}
}

// sessionCtxKey is the context key under which requireAuth stores the
// validated *auth.Session for downstream handlers.
type sessionCtxKey struct{}

func sessionFromContext(ctx context.Context) *auth.Session {
	sess, _ := ctx.Value(sessionCtxKey{}).(*auth.Session)
	if sess == nil {
		return &auth.Session{}
	}
	return sess
}

// toLogin sends a request without a valid session to /login. htmx would follow
// a redirect silently and swap the login page in, so it is told to navigate.
func toLogin(w http.ResponseWriter, r *http.Request) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// requireAuth gates a handler behind a valid session cookie (X3) and, for
// mutating methods, a matching CSRF header (D7, double-submit cookie). An
// unauthenticated request is redirected to /login, never served the page.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			toLogin(w, r)
			return
		}
		sess, err := auth.GetSession(r.Context(), s.store, c.Value)
		if err != nil {
			toLogin(w, r)
			return
		}
		if isMutating(r.Method) {
			got := r.Header.Get(csrfHeader)
			if got == "" {
				got = r.PostFormValue(csrfField)
			}
			if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) != 1 {
				http.Error(w, "CSRF token mismatch", http.StatusForbidden)
				return
			}
		}
		ctx := context.WithValue(r.Context(), sessionCtxKey{}, sess)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// Serve runs the TLS listener on addr until ctx is canceled, along with
// the store-event recorder behind the dashboard and events pages.
func (s *Server) Serve(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", addr, err)
	}
	go s.events.run(ctx, s.store)
	httpSrv := &http.Server{Handler: s.mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = httpSrv.Close()
	}()
	err = httpSrv.Serve(tls.NewListener(ln, tlsCfg))
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
