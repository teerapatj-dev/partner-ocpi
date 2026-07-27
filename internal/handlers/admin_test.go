package handlers

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func adminCtx(t *testing.T, method, path, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// Validation paths short-circuit before store/client are touched, so a zero
// Handlers is enough.
func TestAdminPushLocationValidation(t *testing.T) {
	h := &Handlers{}
	cases := []struct{ name, body string }{
		{"empty body", ""},
		{"missing location_id", `{}`},
		{"blank location_id", `{"location_id":""}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := adminCtx(t, http.MethodPost, "/admin/push/location", tc.body)
			if err := h.AdminPushLocation(c); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// source names a seed batch, so anything outside the two known names is rejected before it can
// reach a query.
func TestAdminOwnRejectsUnknownSource(t *testing.T) {
	h := &Handlers{}
	for _, source := range []string{"bogus", "BASE", "' OR 1=1--"} {
		c, rec := adminCtx(t, http.MethodGet, "/admin/own/locations?source="+url.QueryEscape(source), "")
		c.SetParamNames("kind")
		c.SetParamValues("locations")
		if err := h.AdminOwn(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("source %q: status = %d, want 400", source, rec.Code)
		}
	}
}

func TestAdminPullCounterpartyRejectsUnknownKind(t *testing.T) {
	h := &Handlers{}
	for _, kind := range []string{"", "sessions", "LOCATIONS"} {
		c, rec := adminCtx(t, http.MethodPost, "/admin/pull/x", `{}`)
		c.SetParamNames("kind")
		c.SetParamValues(kind)
		if err := h.AdminPullCounterparty(c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("kind %q: status = %d, want 400", kind, rec.Code)
		}
	}
}
