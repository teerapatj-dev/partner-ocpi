package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/config"
	"mock-ocpi-partner/internal/store"
)

func listCtx(t *testing.T, query string) echo.Context {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/ocpi/cpo/2.2.1/locations"+query, nil)
	return e.NewContext(req, httptest.NewRecorder())
}

func TestParseListQuery(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantErr   bool
		wantLimit int
		wantOff   int
	}{
		{"defaults", "", false, 50, 0},
		{"normal", "?offset=10&limit=20", false, 20, 10},
		{"limit above max clamps to 100", "?limit=500", false, 100, 0},
		{"limit zero falls back to default", "?limit=0", false, 50, 0},
		{"limit negative falls back to default", "?limit=-5", false, 50, 0},
		{"limit not a number", "?limit=abc", true, 0, 0},
		{"offset negative", "?offset=-1", true, 0, 0},
		{"offset not a number", "?offset=x", true, 0, 0},
		{"date_from valid", "?date_from=2026-07-01T00:00:00Z", false, 50, 0},
		{"date_from garbage", "?date_from=yesterday", true, 0, 0},
		{"date_to garbage", "?date_to=2026-13-99", true, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseListQuery(listCtx(t, tc.query))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if f.Limit != tc.wantLimit || f.Offset != tc.wantOff {
				t.Fatalf("filter = limit %d offset %d, want limit %d offset %d", f.Limit, f.Offset, tc.wantLimit, tc.wantOff)
			}
		})
	}
}

func TestPaginationHeaders(t *testing.T) {
	h := &Handlers{cfg: config.Config{BaseURL: "http://partner.test"}}
	cases := []struct {
		name     string
		filter   store.ListFilter
		total    int
		wantLink string
	}{
		{"more pages", store.ListFilter{Offset: 0, Limit: 2}, 5, `<http://partner.test/ocpi/cpo/2.2.1/locations?limit=2&offset=2>; rel="next"`},
		{"exactly last page", store.ListFilter{Offset: 4, Limit: 2}, 5, ""},
		{"single page", store.ListFilter{Offset: 0, Limit: 50}, 4, ""},
		{"offset beyond total", store.ListFilter{Offset: 100, Limit: 50}, 4, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := listCtx(t, "")
			h.setPaginationHeaders(c, tc.filter, tc.total)
			hd := c.Response().Header()
			if got := hd.Get("X-Total-Count"); got != jsonInt(tc.total) {
				t.Fatalf("X-Total-Count = %q, want %d", got, tc.total)
			}
			if got := hd.Get("X-Limit"); got != jsonInt(tc.filter.Limit) {
				t.Fatalf("X-Limit = %q, want %d", got, tc.filter.Limit)
			}
			if got := hd.Get("Link"); got != tc.wantLink {
				t.Fatalf("Link = %q, want %q", got, tc.wantLink)
			}
		})
	}
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestPaginationLinkKeepsDateFilter(t *testing.T) {
	h := &Handlers{cfg: config.Config{BaseURL: "http://partner.test"}}
	c := listCtx(t, "")
	f, err := parseListQuery(listCtx(t, "?date_from=2026-07-01T00:00:00Z&limit=1"))
	if err != nil {
		t.Fatal(err)
	}
	h.setPaginationHeaders(c, f, 3)
	link := c.Response().Header().Get("Link")
	if !strings.Contains(link, "date_from=2026-07-01T00%3A00%3A00Z") {
		t.Fatalf("Link next page dropped the date_from filter: %q", link)
	}
}

func TestFindInArray(t *testing.T) {
	payload := json.RawMessage(`{"evses":[
		{"uid":"EVSE-1","connectors":[{"id":"1"},{"id":2}]},
		{"uid":"EVSE-2","connectors":[]}
	]}`)
	if _, ok := findInArray(payload, "evses", "uid", "EVSE-2"); !ok {
		t.Fatal("string id not found")
	}
	evse, ok := findInArray(payload, "evses", "uid", "EVSE-1")
	if !ok {
		t.Fatal("EVSE-1 not found")
	}
	if _, ok := findInArray(evse, "connectors", "id", "2"); !ok {
		t.Fatal("numeric connector id should match its string form")
	}
	if _, ok := findInArray(evse, "connectors", "id", "9"); ok {
		t.Fatal("unknown connector id must not match")
	}
	if _, ok := findInArray(payload, "evses", "uid", "EVSE-9"); ok {
		t.Fatal("unknown evse uid must not match")
	}
}
