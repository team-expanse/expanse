package metrics

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/expanse/expanse/internal/store"
)

// Serve runs the metrics endpoint on addr until ctx is done. It is a
// dedicated listener (D2), not internal/web's session-authenticated
// mux -- a Prometheus scrape config carries a static bearer token, not a
// cookie, so it gets its own auth check here rather than reusing the
// UI's CSRF/session middleware, which assumes a browser.
func Serve(ctx context.Context, addr string, tlsCfg *tls.Config, collector prometheus.Collector, st store.Store) error {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collector)

	mux := http.NewServeMux()
	mux.Handle("/metrics", requireBearerToken(st, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})))

	ln, err := tls.Listen("tcp", addr, tlsCfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	err = srv.Serve(ln)
	if err != nil && ctx.Err() != nil {
		return nil // shutting down, not a real failure
	}
	return err
}

// requireBearerToken checks the Authorization: Bearer <token> header
// against the cluster's scrape token (EnsureToken/set-token, D2) -- the
// same header Prometheus's own bearer_token_file scrape option sends,
// so no client-side change is needed to authenticate a real Prometheus.
func requireBearerToken(st store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, prefix) {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(auth, prefix)
		// Stale, deliberately (the same reasoning as collector.go's
		// resource/volume/quorum reads): a token record changes rarely,
		// and a scrape's *auth check* must not itself block on a
		// leader that a degraded cluster doesn't have -- that is
		// exactly when a scrape (and any alert built on it) matters
		// most. A linearizable check here would 401 every scrape the
		// instant quorum is lost, taking the whole endpoint dark.
		if !VerifyToken(store.WithStale(r.Context()), st, token) {
			http.Error(w, "invalid bearer token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
