// Package web serves the Expanse web management interface
// (ROADMAP.md Phase 2): a Go html/template + HTMX front end, TLS
// terminated with the node's own cluster-CA-issued certificate. It is a
// thin rendering layer — every handler calls into the same
// control-plane packages the CLI already uses (internal/blocks,
// internal/storage), never a parallel implementation of them.
//
// Every cluster node runs a Server; agent.go reaches it through a
// dedicated VIP (Stream A3) so losing the node currently holding that
// VIP does not take the interface down.
package web

import (
	"context"
	"crypto/tls"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
)

//go:embed static
var staticFS embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// Server is the UI's HTTP server.
type Server struct {
	// NodeID is this node's identity, shown in the footer so an
	// operator reaching the UI through the VIP can tell which node
	// answered.
	NodeID string

	tmpl *template.Template
	mux  *http.ServeMux
}

// New parses the embedded templates and registers routes.
func New(nodeID string) (*Server, error) {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse templates: %w", err)
	}
	s := &Server{NodeID: nodeID, tmpl: tmpl}

	s.mux = http.NewServeMux()
	s.mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	s.mux.HandleFunc("/", s.handleIndex)
	return s, nil
}

// indexData is the template data for the placeholder page. Auth
// (Stream A2) and live content (Streams B-D) replace this.
type indexData struct {
	NodeID string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", indexData{NodeID: s.NodeID}); err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
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
