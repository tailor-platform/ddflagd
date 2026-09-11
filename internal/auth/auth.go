// Package auth guards the evaluation listener with an optional shared secret.
package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
)

// HeaderAPIKey is the OFREP API key header.
const HeaderAPIKey = "X-API-Key" //nolint:gosec // G101: a header name, not a credential

// APIKey wraps next so that it is only reached by a request carrying the given
// key. An empty key returns next unchanged, which is the sidecar case: the
// evaluation listener is bound to loopback and needs no secret.
func APIKey(key string, next http.Handler) http.Handler {
	if key == "" {
		return next
	}
	expected := []byte(key)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get(HeaderAPIKey))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			// The body is small and already in memory, so an encoding failure
			// can only mean the client went away.
			_ = json.NewEncoder(w).Encode(map[string]any{ //nostyle:handlerrors
				"errorCode":    "GENERAL",
				"errorDetails": "a valid " + HeaderAPIKey + " header is required",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}
