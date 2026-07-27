package api

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"orchestrator/internal/observability"
	"orchestrator/pkg/job"
	"strings"
	"time"
)

// LoggingMiddleware logs HTTP requests
func LoggingMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// Wrap response writer to capture status code
			wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			if r.URL.Path == "/livez" || r.URL.Path == "/readyz" {
				return
			}

			// Use context-aware logging to include trace_id and span_id
			slog.InfoContext(r.Context(), "HTTP request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", wrapped.statusCode,
				"duration", time.Since(start),
			)
		})
	}
}

// MetricsMiddleware records HTTP request metrics (latency, traffic, errors).
// Requests are labelled with the mux route they matched, not the raw URL, so
// label cardinality stays bounded by the route table — resource IDs and
// unrouted scanner traffic (/.env, /wp-includes/..., ...) all collapse.
func MetricsMiddleware(metrics *observability.Metrics, mux *http.ServeMux) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(wrapped, r)

			duration := time.Since(start).Seconds()
			metrics.RecordHTTPRequest(r.Context(), r.Method, routePattern(mux, r), wrapped.statusCode, duration)
		})
	}
}

// routePattern returns the path of the mux pattern the request matched, or
// "other" when nothing matched (404) or the method was wrong (405). The
// pattern is "METHOD /path", and the method is already its own attribute.
func routePattern(mux *http.ServeMux, r *http.Request) string {
	_, pattern := mux.Handler(r)
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	if pattern == "" {
		return "other"
	}
	return pattern
}

// RecoveryMiddleware recovers from panics
func RecoveryMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					slog.ErrorContext(r.Context(), "Panic recovered", "error", err)
					http.Error(w, "Internal server error", http.StatusInternalServerError)
				}
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// ContentTypeMiddleware ensures JSON content type for API requests
func ContentTypeMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Check content type for POST/PUT requests
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				contentType := r.Header.Get("Content-Type")
				if contentType != "" && contentType != "application/json" {
					writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}

// JSONErrorMiddleware rewrites the mux's plain-text fallbacks (404 for
// unknown routes, 405 for wrong methods) as {"error": ...} JSON so every
// error the API emits has one shape. Handler responses pass through
// untouched: writeError sets application/json before WriteHeader, so only
// the stdlib's text/plain 404/405 match. The Allow header on 405s survives.
func JSONErrorMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(&jsonErrorWriter{ResponseWriter: w}, r)
		})
	}
}

type jsonErrorWriter struct {
	http.ResponseWriter
	rewrote bool
}

func (w *jsonErrorWriter) WriteHeader(code int) {
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) &&
		strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		w.rewrote = true
		w.Header().Set("Content-Type", "application/json")
		w.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(code)
		msg := "not found"
		if code == http.StatusMethodNotAllowed {
			msg = "method not allowed"
		}
		_, _ = fmt.Fprintf(w.ResponseWriter, "{\"error\":%q}\n", msg)
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write swallows the stdlib's plain-text body after a rewrite.
func (w *jsonErrorWriter) Write(b []byte) (int, error) {
	if w.rewrote {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// CORSMiddleware adds CORS headers
func CORSMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// AuthMiddleware validates Bearer token authentication.
// If apiKey is empty, authentication is disabled.
func AuthMiddleware(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth if no API key is configured
			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, "Authorization header required", http.StatusUnauthorized)
				return
			}

			// Expect "Bearer <token>"
			parts := strings.SplitN(authHeader, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, "Invalid authorization header format", http.StatusUnauthorized)
				return
			}

			token := parts[1]
			if subtle.ConstantTimeCompare([]byte(token), []byte(apiKey)) != 1 {
				http.Error(w, "Invalid API key", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ArtifactAuthMiddleware validates the per-job bearer token on the internal
// artifact endpoint. The expected token is derived as HMAC-SHA256(apiKey,
// jobID), so it is bound to the job in the URL path — a token leaked from one
// job cannot report results for another. If apiKey is empty, authentication
// is disabled (matching AuthMiddleware).
func ArtifactAuthMiddleware(apiKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if apiKey == "" {
				next.ServeHTTP(w, r)
				return
			}

			parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				http.Error(w, "Bearer token required", http.StatusUnauthorized)
				return
			}

			expected := job.ArtifactToken(apiKey, r.PathValue("jobId"))
			if subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expected)) != 1 {
				http.Error(w, "Invalid artifact token", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
