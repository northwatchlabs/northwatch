package auth_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/northwatchlabs/northwatch/internal/server/auth"
)

// nextRecorder is a stub next handler that records whether it ran
// and writes 204 when it does. Tests assert on ran to distinguish
// "middleware passed through" from "middleware short-circuited".
type nextRecorder struct{ ran bool }

func (n *nextRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		n.ran = true
		w.WriteHeader(http.StatusNoContent)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBearerTokenNoHeader(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/incidents", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if rec.ran {
		t.Errorf("next handler should not have run")
	}
	if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "unauthorized" {
		t.Errorf("body error = %q, want %q", body["error"], "unauthorized")
	}
}

func TestBearerTokenBadScheme(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if rec.ran {
		t.Errorf("next handler should not have run")
	}
}

func TestBearerTokenWrongToken(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "Bearer wrongtoken9999wrong")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if rec.ran {
		t.Errorf("next handler should not have run")
	}
}

func TestBearerTokenRightToken(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "Bearer verysecrettoken1234")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if !rec.ran {
		t.Errorf("next handler should have run")
	}
}

func TestBearerTokenCaseInsensitiveScheme(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "bearer verysecrettoken1234")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (scheme is case-insensitive per RFC 6750)", rr.Code)
	}
	if !rec.ran {
		t.Errorf("next handler should have run")
	}
}

func TestBearerTokenEmptyExpectedDisablesRoutes(t *testing.T) {
	rec := &nextRecorder{}
	h := auth.BearerToken("", discardLogger())(rec.handler())

	req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
	req.Header.Set("Authorization", "Bearer anytoken")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if rec.ran {
		t.Errorf("next handler should not have run when writes are disabled")
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body["error"] != "write endpoints disabled" {
		t.Errorf("body error = %q, want %q", body["error"], "write endpoints disabled")
	}
}

func TestBearerTokenWWWAuthenticateOnEveryFailure(t *testing.T) {
	cases := []struct {
		name     string
		expected string
		header   string
	}{
		{"no header", "verysecrettoken1234", ""},
		{"bad scheme", "verysecrettoken1234", "Basic abc"},
		{"wrong token", "verysecrettoken1234", "Bearer wrongtoken9999wrong"},
		{"writes disabled", "", "Bearer anytoken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &nextRecorder{}
			h := auth.BearerToken(tc.expected, discardLogger())(rec.handler())

			req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if got := rr.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
			if !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
				t.Errorf("content-type = %q, want application/json prefix", rr.Header().Get("Content-Type"))
			}
		})
	}
}

func TestBearerTokenConstantTimeCompare(t *testing.T) {
	// Qualitative check: two equal-length wrong tokens both yield
	// 401 with the same response. Documents intent; the actual
	// timing property is not testable without instrumentation.
	rec := &nextRecorder{}
	h := auth.BearerToken("verysecrettoken1234", discardLogger())(rec.handler())

	for _, tok := range []string{"aaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbb"} {
		req := httptest.NewRequest(http.MethodPost, "/incidents", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("token %q: status = %d, want 401", tok, rr.Code)
		}
	}
}
