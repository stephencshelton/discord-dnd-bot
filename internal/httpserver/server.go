// Package httpserver runs the shared health + Prometheus metrics endpoint that
// every service exposes. Kubernetes uses /healthz (liveness) and /readyz
// (readiness); Prometheus scrapes /metrics.
package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ReadinessFunc reports whether the service is ready to serve (deps reachable).
type ReadinessFunc func(ctx context.Context) error

// Server bundles the health/metrics HTTP server.
type Server struct {
	srv     *http.Server
	handler http.Handler
	ready   ReadinessFunc
}

// Handler exposes the underlying request handler (health, readiness, metrics)
// so it can be exercised in tests without binding a socket.
func (s *Server) Handler() http.Handler { return s.handler }

// New builds the server bound to addr. ready may be nil (always ready).
func New(addr string, ready ReadinessFunc) *Server {
	mux := http.NewServeMux()
	s := &Server{ready: ready}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.ready != nil {
			ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			defer cancel()
			if err := s.ready(ctx); err != nil {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(err.Error()))
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/metrics", promhttp.Handler())

	s.handler = mux
	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Start runs the server until the context is cancelled, then shuts it down.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
