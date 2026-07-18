// Package httpserver assembles the chi router and HTTP server: health
// endpoints, /api mount point, middleware, and embedded SPA serving.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/BryanBaluyut/slatedesk/internal/auth"
	"github.com/BryanBaluyut/slatedesk/internal/events"
	"github.com/BryanBaluyut/slatedesk/internal/handlers"
	"github.com/BryanBaluyut/slatedesk/internal/problem"
	"github.com/BryanBaluyut/slatedesk/internal/storage"
	"github.com/BryanBaluyut/slatedesk/internal/web"
)

// Server wraps http.Server with SlateDesk's router.
type Server struct {
	httpServer *http.Server
	pool       *pgxpool.Pool
}

// New builds the router and returns a Server listening on addr when Run is
// called. secret is the instance root secret (session cookie signing);
// cookieSecure controls the session cookie's Secure attribute; hub feeds
// the SSE endpoint (run an events.Listener into it); blobs stores
// attachment bytes.
func New(addr string, pool *pgxpool.Pool, secret []byte, cookieSecure auth.CookieSecureMode, hub *events.Hub, blobs storage.Storage) *Server {
	s := &Server{pool: pool}

	r := chi.NewRouter()
	r.Use(requestLogger)
	r.Use(recoverer)

	// Liveness: process is up. Always 200; no dependencies.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// Readiness: can we serve traffic? Checks the database.
	r.Get("/readyz", s.handleReadyz)

	// API mount point. Unknown /api paths get problem+json (never the SPA
	// fallback).
	r.Route("/api", func(api chi.Router) {
		api.NotFound(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, http.StatusNotFound, "Not Found", "no such API route")
		})
		api.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
			problem.Write(w, r, http.StatusMethodNotAllowed, "Method Not Allowed", "")
		})
		api.Mount("/v1", handlers.New(pool, secret, cookieSecure, hub, blobs).Router())
	})

	// Everything else: embedded SPA with client-side-routing fallback.
	r.Handle("/*", web.Handler())

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s
}

// Run serves until ctx is canceled, then shuts down gracefully with a
// 10-second drain deadline.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("httpserver: listen on %s: %w", s.httpServer.Addr, err)
		}
		return nil
	case <-ctx.Done():
	}

	slog.Info("shutting down http server")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
		// Drain deadline exceeded: force-close the remaining connections.
		// Closing them cancels their request contexts, which unwinds
		// handlers stuck on slow queries — otherwise they keep holding
		// pool connections and the deferred pool.Close() in main blocks
		// the process from ever exiting.
		if cerr := s.httpServer.Close(); cerr != nil {
			slog.Error("force-close http server", "error", cerr)
		}
		return fmt.Errorf("httpserver: shutdown: %w", err)
	}
	return nil
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.pool.Ping(pingCtx); err != nil {
		slog.Warn("readyz failed", "error", err)
		problem.Write(w, r, http.StatusServiceUnavailable, "Not Ready", "database unreachable")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
