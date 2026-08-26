// Package httpserver runs the shared health + Prometheus metrics endpoint that
// every service exposes. Kubernetes uses /healthz (liveness) and /readyz
// (readiness); Prometheus scrapes /metrics.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stephencshelton/discord-dnd-bot/internal/metrics"
)

// ReadinessFunc reports whether the service is ready to serve (deps reachable).
type ReadinessFunc func(ctx context.Context) error

// Server bundles the health/metrics HTTP server.
type Server struct {
	srv     *http.Server
	handler http.Handler
	ready   ReadinessFunc
	log     *slog.Logger
}

// Handler exposes the underlying request handler (health, readiness, metrics)
// so it can be exercised in tests without binding a socket.
func (s *Server) Handler() http.Handler { return s.handler }

// statusRecorder captures the response status code for access logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// New builds the server bound to addr. ready may be nil (always ready). log may
// be nil, in which case a no-op default logger is used.
func New(addr string, log *slog.Logger, ready ReadinessFunc) *Server {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	s := &Server{ready: ready, log: log}

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

	// Wrap the mux with recovery + access-logging middleware.
	s.handler = s.instrument(mux)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s
}

// instrument wraps a handler with panic recovery, access logging, and metrics.
// Health/metrics probes are noisy, so successful health probes log at Debug
// while errors and metrics scrapes log at Info; any 5xx logs at Warn.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if rv := recover(); rv != nil {
				metrics.PanicsRecovered.WithLabelValues("http").Inc()
				s.log.Error("panic in http handler; recovered",
					"path", r.URL.Path, "method", r.Method, "panic", rv)
				// Best effort: the handler may have already written a header.
				rec.status = http.StatusInternalServerError
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			metrics.HTTPRequests.WithLabelValues(r.URL.Path, http.StatusText(rec.status)).Inc()

			lvl := slog.LevelDebug
			if rec.status >= 500 {
				lvl = slog.LevelWarn
			} else if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
				lvl = slog.LevelInfo
			}
			s.log.Log(r.Context(), lvl, "http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rec.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}()

		next.ServeHTTP(rec, r)
	})
}

// Start runs the server until the context is cancelled, then shuts it down.
func (s *Server) Start(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("health/metrics server listening", "addr", s.srv.Addr)
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
