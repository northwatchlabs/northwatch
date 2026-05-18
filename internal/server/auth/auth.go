// Package auth provides the bearer-token middleware that gates
// every write endpoint behind a shared secret.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// BearerToken returns middleware that requires
// `Authorization: Bearer <expected>` on every request. The provided
// token is compared with subtle.ConstantTimeCompare to avoid leaking
// the expected value via timing.
//
// An empty expected token disables the wrapped routes: every request
// returns 401 with body {"error":"write endpoints disabled"}. This
// matches the "writes disabled" boot mode used when no
// NORTHWATCH_API_TOKEN is configured.
//
// Failure responses always set Content-Type: application/json and
// WWW-Authenticate: Bearer (RFC 6750 §3). The middleware never logs
// token values — only the failure reason.
func BearerToken(expected string, logger *slog.Logger) func(http.Handler) http.Handler {
	// Compare fixed-size SHA-256 digests rather than the raw tokens.
	// subtle.ConstantTimeCompare returns 0 immediately when lengths
	// differ, which would leak the expected token's length via timing.
	// Hashing first equalizes input size so the comparison is
	// length-independent.
	expectedHash := sha256.Sum256([]byte(expected))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if expected == "" {
				writeUnauthorized(w, "write endpoints disabled")
				logger.Warn("auth_denied", "reason", "writes_disabled",
					"method", r.Method, "path", r.URL.Path)
				return
			}

			h := r.Header.Get("Authorization")
			if h == "" {
				writeUnauthorized(w, "unauthorized")
				logger.Warn("auth_denied", "reason", "missing_header",
					"method", r.Method, "path", r.URL.Path)
				return
			}

			fields := strings.Fields(h)
			if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
				writeUnauthorized(w, "unauthorized")
				logger.Warn("auth_denied", "reason", "bad_scheme",
					"method", r.Method, "path", r.URL.Path)
				return
			}

			providedHash := sha256.Sum256([]byte(fields[1]))
			if subtle.ConstantTimeCompare(providedHash[:], expectedHash[:]) != 1 {
				writeUnauthorized(w, "unauthorized")
				logger.Warn("auth_denied", "reason", "mismatch",
					"method", r.Method, "path", r.URL.Path)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
