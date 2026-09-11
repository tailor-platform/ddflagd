package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAPIKey states when the evaluation listener requires a shared secret. In a
// sidecar no secret is configured and the listener is reachable only from
// loopback, so the guard has to be transparent then.
func TestAPIKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		key        string
		header     string
		wantStatus int
		wantPassed bool
	}{
		{
			name:       "no configured key lets every request through",
			key:        "",
			header:     "",
			wantStatus: http.StatusOK,
			wantPassed: true,
		},
		{
			name:       "a matching key is accepted",
			key:        "secret",
			header:     "secret",
			wantStatus: http.StatusOK,
			wantPassed: true,
		},
		{
			name:       "a wrong key is rejected",
			key:        "secret",
			header:     "nope",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a missing key is rejected",
			key:        "secret",
			header:     "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			passed := false
			h := APIKey(tt.key, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				passed = true
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodPost, "/ofrep/v1/evaluate/flags/flag", nil)
			if tt.header != "" {
				req.Header.Set(HeaderAPIKey, tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if passed != tt.wantPassed {
				t.Errorf("reached the next handler: got %v, want %v", passed, tt.wantPassed)
			}
			if tt.wantStatus == http.StatusUnauthorized {
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type: got %q, want application/json", got)
				}
			}
		})
	}
}
