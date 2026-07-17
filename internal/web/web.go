// Package web serves the embedded SPA.
//
// The frontend build output is embedded from internal/web/dist. Only a
// .gitkeep is tracked there (the all: embed pattern includes dotfiles, so
// `go build` works on a fresh clone); the Makefile `build` target copies
// frontend/dist here first when a frontend/ project is present. (go:embed
// cannot reach outside the package directory, hence the copy step.) Without
// the copy step the server runs but serves a 500 for SPA routes.
package web

import (
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// Handler serves static assets from the embedded dist directory. Any path
// that does not match a file falls back to index.html so client-side routes
// (e.g. /tickets/42) deep-link correctly. /api paths never reach this
// handler; the router owns them.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable with a committed dist/ directory; fail loudly if the
		// embed is ever broken rather than serving a blank site.
		panic("web: embedded dist directory missing: " + err.Error())
	}
	fileServer := http.FileServerFS(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" && !strings.HasSuffix(path, "/") {
			if info, err := fs.Stat(sub, path); err == nil && !info.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// Hashed build assets are never client-side routes: a miss there
		// (stale chunk URL from a previous deploy, typo) must be a real
		// 404, not index.html masquerading as JavaScript — browsers reject
		// the module with a cryptic MIME error otherwise.
		if strings.HasPrefix(path, "assets/") {
			http.NotFound(w, r)
			return
		}

		// SPA fallback: serve index.html for unknown paths.
		index, err := fs.ReadFile(sub, "index.html")
		if err != nil {
			slog.Error("embedded index.html missing", "error", err)
			http.Error(w, "index.html not embedded", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(index)
		}
	})
}
