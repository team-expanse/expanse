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
	"context"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"

	"github.com/expanse/expanse/internal/store"
	"github.com/expanse/expanse/internal/web/auth"
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

// Server is the UI's HTTP server.
type Server struct {
	// NodeID is this node's identity, shown in the footer so an
	// operator reaching the UI through the VIP can tell which node
	// answered.
	NodeID string

	store   store.Store
	blocks  pb.BlockServiceServer
	catalog pb.CatalogServiceServer
	tmpl    *template.Template
	mux     *http.ServeMux
}

// New parses the embedded templates and registers routes. st is the
// (Raft-replicated in cluster mode) store backing sessions and the
// admin credential (internal/web/auth) — callers should ensure
// auth.EnsureAdmin has run against it before serving traffic. blocks and
// catalog are the same in-process servers the local gRPC socket
// registers (D1): nil on a node with the block API disabled, in which
// case the block routes answer 503 rather than panic.
func New(nodeID string, st store.Store, blocks pb.BlockServiceServer, catalog pb.CatalogServiceServer) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	s := &Server{NodeID: nodeID, store: st, blocks: blocks, catalog: catalog, tmpl: tmpl}

	s.mux = http.NewServeMux()
	s.mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.Handle("/logout", s.requireAuth(http.HandlerFunc(s.handleLogout)))
	s.mux.Handle("/", s.requireAuth(http.HandlerFunc(s.handleIndex)))
	s.registerBlockRoutes()
	return s, nil
}

// indexData is the template data for the placeholder page.
type indexData struct {
	NodeID    string
	CSRFToken string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sess := sessionFromContext(r.Context())
	s.render(w, http.StatusOK, "index.html", indexData{NodeID: s.NodeID, CSRFToken: sess.CSRFToken})
}

// render executes a named template with the given status and data,
// shared by every page handler (index/login handle their own bodies
// pre-dating this helper; C1's block pages use it directly).
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

type loginData struct {
	Error bool
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderLogin(w, http.StatusOK, false)
	case http.MethodPost:
		s.handleLoginSubmit(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) renderLogin(w http.ResponseWriter, status int, failed bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.tmpl.ExecuteTemplate(w, "login.html", loginData{Error: failed}); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, true)
		return
	}
	username := r.PostForm.Get("username")
	password := r.PostForm.Get("password")
	if username != auth.AdminUsername || !auth.VerifyAdminPassword(r.Context(), s.store, password) {
		s.renderLogin(w, http.StatusUnauthorized, true)
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
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: sess.CSRFToken, Path: "/",
		HttpOnly: false, Secure: true, SameSite: http.SameSiteStrictMode,
	})
}

func clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, Secure: true, SameSite: http.SameSiteStrictMode})
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

// requireAuth gates a handler behind a valid session cookie (X3) and, for
// mutating methods, a matching CSRF header (D7, double-submit cookie). An
// unauthenticated request is redirected to /login, never served the page.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		sess, err := auth.GetSession(r.Context(), s.store, c.Value)
		if err != nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if isMutating(r.Method) {
			got := r.Header.Get(csrfHeader)
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

// Serve runs the TLS listener on addr until ctx is canceled.
func (s *Server) Serve(ctx context.Context, addr string, tlsCfg *tls.Config) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("web: listen %s: %w", addr, err)
	}
	httpSrv := &http.Server{Handler: s.mux}
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
