// Package problem writes RFC 9457 problem details responses
// (application/problem+json). It is the single error-shape used by every
// HTTP surface: API handlers, health endpoints, and router fallbacks.
package problem

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Details is an RFC 9457 problem details object.
type Details struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// Write writes an RFC 9457 application/problem+json response. Title should
// be a short human-readable summary of the status; detail may add
// occurrence-specific information (never internal error text for 5xx).
func Write(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	p := Details{
		Type:     "about:blank",
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		slog.Error("write problem response", "error", err, "path", r.URL.Path)
	}
}
