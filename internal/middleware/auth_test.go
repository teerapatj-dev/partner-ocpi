package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestDecodeTokenHeader(t *testing.T) {
	std := base64.StdEncoding.EncodeToString([]byte("my-token"))
	raw := base64.RawStdEncoding.EncodeToString([]byte("my-token"))
	cases := []struct {
		name   string
		header string
		want   string
		ok     bool
	}{
		{"std encoding", "Token " + std, "my-token", true},
		{"raw std encoding (no padding)", "Token " + raw, "my-token", true},
		{"lowercase scheme", "token " + std, "my-token", true},
		{"missing header", "", "", false},
		{"wrong scheme", "Bearer " + std, "", false},
		{"not base64", "Token %%%not-base64%%%", "", false},
		{"empty token", "Token ", "", false},
		{"scheme only", "Token", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decodeTokenHeader(tc.header)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("decodeTokenHeader(%q) = (%q,%v), want (%q,%v)", tc.header, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func callAPIKey(t *testing.T, configured, presented string) int {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/admin/state", nil)
	if presented != "" {
		req.Header.Set("X-API-Key", presented)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	handler := APIKeyAuth(configured)(func(c echo.Context) error {
		return c.NoContent(http.StatusOK)
	})
	if err := handler(c); err != nil {
		if he, ok := err.(*echo.HTTPError); ok {
			return he.Code
		}
		t.Fatalf("unexpected error: %v", err)
	}
	return rec.Code
}

func TestAPIKeyAuth(t *testing.T) {
	if got := callAPIKey(t, "k1", "k1"); got != http.StatusOK {
		t.Fatalf("valid key: %d, want 200", got)
	}
	if got := callAPIKey(t, "k1", "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong key: %d, want 401", got)
	}
	if got := callAPIKey(t, "k1", ""); got != http.StatusUnauthorized {
		t.Fatalf("missing key: %d, want 401", got)
	}
	// fail-closed: no configured key rejects everything, even an empty match
	if got := callAPIKey(t, "", ""); got != http.StatusUnauthorized {
		t.Fatalf("unconfigured key must fail closed: %d, want 401", got)
	}
}
