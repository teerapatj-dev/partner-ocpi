package ocpi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

// Evolt decodes with `StatusCode int` and DateTime must be RFC3339 UTC with a
// trailing Z (no offset) — a wrong shape here breaks every flow at once.
func TestNowIsUTCZ(t *testing.T) {
	got := Now()
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`).MatchString(got) {
		t.Fatalf("Now() = %q, want RFC3339 UTC ending in Z without offset", got)
	}
}

func record(t *testing.T, fn func(c echo.Context) error) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if err := fn(e.NewContext(req, rec)); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return rec
}

func decodeEnvelope(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return m
}

func TestOKEnvelopeShape(t *testing.T) {
	rec := record(t, func(c echo.Context) error { return OK(c, []string{"x"}) })
	if rec.Code != http.StatusOK {
		t.Fatalf("http = %d, want 200", rec.Code)
	}
	m := decodeEnvelope(t, rec.Body.String())
	if string(m["status_code"]) != "1000" {
		t.Fatalf("status_code = %s, want bare number 1000", m["status_code"])
	}
	if strings.Contains(rec.Body.String(), `"status_code":"`) {
		t.Fatal("status_code serialized as string — Evolt decode would fail")
	}
}

// Functional errors must ride HTTP 200: Evolt's fanout requires 2xx AND the
// OCPI code (tariff DELETE convergence is 2xx + 2003).
func TestErrKeepsHTTP200(t *testing.T) {
	rec := record(t, func(c echo.Context) error { return Err(c, StatusUnknownResource, "nope") })
	if rec.Code != http.StatusOK {
		t.Fatalf("http = %d, want 200", rec.Code)
	}
	m := decodeEnvelope(t, rec.Body.String())
	if string(m["status_code"]) != "2003" {
		t.Fatalf("status_code = %s, want 2003", m["status_code"])
	}
	if _, hasData := m["data"]; hasData {
		t.Fatal("error envelope must not carry data")
	}
}

func TestErrHTTPStatus(t *testing.T) {
	rec := record(t, func(c echo.Context) error {
		return ErrHTTP(c, http.StatusUnauthorized, StatusInvalidParams, "bad token")
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("http = %d, want 401", rec.Code)
	}
	m := decodeEnvelope(t, rec.Body.String())
	if string(m["status_code"]) != "2001" {
		t.Fatalf("status_code = %s, want 2001", m["status_code"])
	}
}

func TestRedactTokens(t *testing.T) {
	in := `{"token":"secret-uuid","url":"x","roles":[{"token": "another"}]}`
	out := RedactTokens(in)
	if strings.Contains(out, "secret-uuid") || strings.Contains(out, "another") {
		t.Fatalf("token leaked: %s", out)
	}
	if !strings.Contains(out, `"token":"[redacted]"`) {
		t.Fatalf("redaction marker missing: %s", out)
	}
}
