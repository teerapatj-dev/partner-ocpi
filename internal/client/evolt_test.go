package client

import (
	"encoding/json"
	"testing"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

// Evolt answers three different shapes; the initiator must survive all of them.
func TestParseEnvelopeShapes(t *testing.T) {
	t.Run("raw array (Evolt GET /api/ocpi/versions)", func(t *testing.T) {
		var versions []ocpi.Version
		res := callResult{HTTPStatus: 200, Body: []byte(`[{"version":"2.2.1","url":"http://e/2.2.1"}]`)}
		if err := parseEnvelope(res, &versions); err != nil {
			t.Fatalf("raw array: %v", err)
		}
		if len(versions) != 1 || versions[0].Version != "2.2.1" {
			t.Fatalf("parsed %+v", versions)
		}
	})

	t.Run("BaseApiResponse code string (Evolt orch)", func(t *testing.T) {
		var details ocpi.VersionDetails
		res := callResult{HTTPStatus: 200, Body: []byte(
			`{"code":"1000","message":"Success","data":{"version":"2.2.1","endpoints":[{"identifier":"credentials","role":"SENDER","url":"http://e/cred"}]}}`)}
		if err := parseEnvelope(res, &details); err != nil {
			t.Fatalf("BaseApiResponse: %v", err)
		}
		if len(details.Endpoints) != 1 {
			t.Fatalf("parsed %+v", details)
		}
	})

	t.Run("OCPI envelope status_code int (another partner/mock)", func(t *testing.T) {
		var creds ocpi.Credentials
		res := callResult{HTTPStatus: 200, Body: []byte(
			`{"status_code":1000,"status_message":"Success","timestamp":"2026-07-01T00:00:00Z","data":{"token":"tok-c","url":"http://p/ocpi/versions","roles":[]}}`)}
		if err := parseEnvelope(res, &creds); err != nil {
			t.Fatalf("OCPI envelope: %v", err)
		}
		if creds.Token != "tok-c" {
			t.Fatalf("parsed %+v", creds)
		}
	})

	t.Run("rejects business error even on HTTP 200", func(t *testing.T) {
		var creds ocpi.Credentials
		res := callResult{HTTPStatus: 200, Body: []byte(`{"code":"7010","message":"already registered"}`)}
		if err := parseEnvelope(res, &creds); err == nil {
			t.Fatal("code 7010 must be an error — HTTP 200 does not mean success")
		}
	})

	t.Run("rejects OCPI 2xxx", func(t *testing.T) {
		var creds ocpi.Credentials
		res := callResult{HTTPStatus: 200, Body: []byte(`{"status_code":2001,"timestamp":"2026-07-01T00:00:00Z"}`)}
		if err := parseEnvelope(res, &creds); err == nil {
			t.Fatal("status_code 2001 must be an error")
		}
	})

	t.Run("rejects HTTP error status", func(t *testing.T) {
		var v []ocpi.Version
		res := callResult{HTTPStatus: 404, Body: []byte(`[]`)}
		if err := parseEnvelope(res, &v); err == nil {
			t.Fatal("HTTP 404 must be an error")
		}
	})

	t.Run("rejects success envelope without data", func(t *testing.T) {
		var creds ocpi.Credentials
		res := callResult{HTTPStatus: 200, Body: []byte(`{"status_code":1000,"timestamp":"2026-07-01T00:00:00Z"}`)}
		if err := parseEnvelope(res, &creds); err == nil {
			t.Fatal("empty data on credentials response must be an error")
		}
	})

	t.Run("rejects non-JSON", func(t *testing.T) {
		var v []ocpi.Version
		res := callResult{HTTPStatus: 200, Body: []byte(`<html>gateway timeout</html>`)}
		if err := parseEnvelope(res, &v); err == nil {
			t.Fatal("non-JSON must be an error")
		}
	})
}

func TestParseAnyStatus(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
		ok   bool
	}{
		{"ocpi int", `{"status_code":1000}`, 1000, true},
		{"ocpi 2003", `{"status_code":2003}`, 2003, true},
		{"base api response string code", `{"code":"1000","message":"ok"}`, 1000, true},
		{"non-numeric code", `{"code":"OK"}`, 0, false},
		{"no envelope", `[]`, 0, false},
		{"non-json", `oops`, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAnyStatus([]byte(tc.body))
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseAnyStatus(%s) = (%d,%v), want (%d,%v)", tc.body, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestEndpointURL(t *testing.T) {
	c := &Client{}
	reg := store.Registration{Endpoints: json.RawMessage(`[
		{"identifier":"locations","role":"SENDER","url":"http://evolt/cpo/locations"},
		{"identifier":"locations","role":"RECEIVER","url":"http://evolt/emsp/locations"},
		{"identifier":"tariffs","role":"sender","url":"http://evolt/cpo/tariffs"}
	]`)}

	cases := []struct {
		name, identifier, role, want string
		wantErr                      bool
	}{
		{"sender picked over receiver", "locations", "SENDER", "http://evolt/cpo/locations", false},
		{"receiver picked over sender", "locations", "RECEIVER", "http://evolt/emsp/locations", false},
		{"role match is case-insensitive", "tariffs", "SENDER", "http://evolt/cpo/tariffs", false},
		{"missing role", "tariffs", "RECEIVER", "", true},
		{"missing identifier", "cdrs", "SENDER", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.endpointURL(reg, tc.identifier, tc.role)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := c.endpointURL(store.Registration{Endpoints: json.RawMessage(`{bad`)}, "locations", "SENDER"); err == nil {
		t.Fatal("unreadable endpoints must error")
	}
}

// An override replaces only the fields it carries: a PUT that renames the partner must not blank the
// website it never mentioned, and a rotate-only PUT must keep the configured identity intact.
func TestBusinessOverrideApply(t *testing.T) {
	roles := func() []ocpi.Role {
		r := ocpi.SelfRoles("TH", "PLG", "PlugSiam")
		for i := range r {
			r[i].BusinessDetails.Website = "https://old.example"
		}
		return r
	}

	got := BusinessOverride{Name: "PlugSiam Energy", LogoURL: "https://cdn.example/l.png"}.apply(roles())
	for _, r := range got {
		if r.BusinessDetails.Name != "PlugSiam Energy" {
			t.Errorf("%s: name not applied: %q", r.Role, r.BusinessDetails.Name)
		}
		if r.BusinessDetails.LogoURL != "https://cdn.example/l.png" {
			t.Errorf("%s: logo not applied: %q", r.Role, r.BusinessDetails.LogoURL)
		}
		if r.BusinessDetails.Website != "https://old.example" {
			t.Errorf("%s: an omitted field must be left alone, got %q", r.Role, r.BusinessDetails.Website)
		}
	}

	for _, r := range (BusinessOverride{}).apply(roles()) {
		if r.BusinessDetails.Name != "PlugSiam" || r.BusinessDetails.Website != "https://old.example" {
			t.Errorf("%s: empty override changed the identity: %+v", r.Role, r.BusinessDetails)
		}
	}
}
