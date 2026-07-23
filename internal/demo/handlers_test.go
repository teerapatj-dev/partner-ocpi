package demo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
)

type env struct {
	ok     bool
	data   json.RawMessage
	errMsg string
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) env {
	t.Helper()
	var out struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the demo envelope: %v — %s", err, rec.Body.String())
	}
	return env{ok: out.OK, data: out.Data, errMsg: out.Error}
}

func doReq(t *testing.T, h func(echo.Context) error, method, target, body string, params ...string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	for i := 0; i+1 < len(params); i += 2 {
		c.SetParamNames(params[i])
		c.SetParamValues(params[i+1])
	}
	if err := h(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

const adminKey = "test-admin-key"

// fakeMock answers the mock admin surface; every request must carry the key.
func fakeMock(t *testing.T, mux map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != adminKey {
			t.Errorf("mock called without admin key: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if h, okRoute := mux[r.Method+" "+r.URL.Path]; okRoute {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
}

func jsonResp(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}
}

func newHandlers(t *testing.T, cfg Config, mockSrv, evoltSrv *httptest.Server) *Handlers {
	t.Helper()
	if mockSrv != nil {
		cfg.MockBaseURL = mockSrv.URL
		t.Cleanup(mockSrv.Close)
	} else {
		cfg.MockBaseURL = "http://127.0.0.1:1" // nothing listens here
	}
	cfg.MockAdminKey = adminKey
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if evoltSrv != nil {
		t.Cleanup(evoltSrv.Close)
		cfg.EvoltOrchURL = evoltSrv.URL
		cfg.EvoltAdapterURL = evoltSrv.URL
		cfg.EvoltRoamingURL = evoltSrv.URL
		if cfg.EvoltVersionsURL == "" {
			cfg.EvoltVersionsURL = evoltSrv.URL + "/api/ocpi/versions"
		}
	}
	return NewHandlers(cfg, NewMockAdmin(cfg), NewEvolt(cfg), NewKafka(Config{}))
}

const stateBody = `{"partner":{"name":"PlugSiam","party_id":"PLG","country_code":"TH"},"registration_status":"REGISTERED","counts":{"own_locations":4}}`

func TestStateHappy(t *testing.T) {
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/state":         jsonResp(stateBody),
		"GET /admin/registrations": jsonResp(`[{"status":"REGISTERED"}]`),
	})
	h := newHandlers(t, Config{AllowedStations: []string{"s1"}}, mock, nil)
	rec := doReq(t, h.State, http.MethodGet, "/api/demo/state", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decode(t, rec)
	if !got.ok {
		t.Fatalf("ok = false: %s", rec.Body.String())
	}
	var data struct {
		Partner  json.RawMessage `json:"partner"`
		Degraded []string        `json:"degraded"`
		Kafka    struct {
			Enabled bool `json:"enabled"`
		} `json:"kafka"`
		AllowedStations []string `json:"allowed_stations"`
	}
	if err := json.Unmarshal(got.data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Degraded) != 0 || data.Kafka.Enabled || len(data.AllowedStations) != 1 {
		t.Fatalf("unexpected state: %s", got.data)
	}
}

func TestStateMockDownStaysOK(t *testing.T) {
	h := newHandlers(t, Config{}, nil, nil)
	rec := doReq(t, h.State, http.MethodGet, "/api/demo/state", "")
	got := decode(t, rec)
	if rec.Code != http.StatusOK || !got.ok {
		t.Fatalf("state must stay ok when mock is down: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(got.data), `"degraded":["mock"]`) {
		t.Fatalf("degraded flag missing: %s", got.data)
	}
}

func TestPartnerInitHandshake(t *testing.T) {
	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocpi/partner/initial" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "orch-key" {
			t.Errorf("orch called without api key")
		}
		var req struct {
			PartnerName string `json:"partner_name"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.PartnerName != "PlugSiam" {
			t.Errorf("partner_name = %q", req.PartnerName)
		}
		w.Write([]byte(`{"code":"1000","data":{"token_a":"tok-a-123"}}`))
	}))
	var handshakeBody []byte
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/state": jsonResp(stateBody),
		"POST /admin/handshake": func(w http.ResponseWriter, r *http.Request) {
			handshakeBody, _ = json.Marshal(mustDecode(r))
			w.Write([]byte(`{"counterparty":"TH-EVO","steps":["REGISTERED"]}`))
		},
	})
	h := newHandlers(t, Config{OrchAPIKey: "orch-key", EvoltVersionsURL: "http://bff/api/ocpi/versions"}, mock, evolt)
	rec := doReq(t, h.PartnerInitHandshake, http.MethodPost, "/api/demo/handshake/partner-init", "{}")
	got := decode(t, rec)
	if rec.Code != http.StatusOK || !got.ok {
		t.Fatalf("handshake failed: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(handshakeBody), `"token_a":"tok-a-123"`) ||
		!strings.Contains(string(handshakeBody), `"evolt_versions_url":"http://bff/api/ocpi/versions"`) {
		t.Fatalf("mock handshake got wrong body: %s", handshakeBody)
	}
	if strings.Contains(rec.Body.String(), "tok-a-123") {
		t.Fatal("raw token leaked to the browser response")
	}
}

func mustDecode(r *http.Request) map[string]any {
	var m map[string]any
	json.NewDecoder(r.Body).Decode(&m)
	return m
}

func TestPartnerInitHandshakeOrchRejects(t *testing.T) {
	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"code":"9999","message":"internal error"}`))
	}))
	mock := fakeMock(t, map[string]http.HandlerFunc{"GET /admin/state": jsonResp(stateBody)})
	h := newHandlers(t, Config{EvoltVersionsURL: "http://bff/api/ocpi/versions"}, mock, evolt)
	rec := doReq(t, h.PartnerInitHandshake, http.MethodPost, "/", "{}")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("HTTP 200 with code!=1000 must map to 502, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEvoltInitHandshakeNeedsPublicURL(t *testing.T) {
	mock := fakeMock(t, nil)
	h := newHandlers(t, Config{}, mock, nil)
	rec := doReq(t, h.EvoltInitHandshake, http.MethodPost, "/", "{}")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestEvoltInitHandshakeHappy(t *testing.T) {
	var outboundBody map[string]any
	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocpi/partner/outbound" {
			http.NotFound(w, r)
			return
		}
		outboundBody = mustDecode(r)
		w.Write([]byte(`{"code":"1000","data":{"status":"REGISTERED"}}`))
	}))
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/state":   jsonResp(stateBody),
		"POST /admin/tokens": jsonResp(`{"token_a":"fresh-token-a"}`),
	})
	h := newHandlers(t, Config{PublicBaseURL: "https://demo.example.com"}, mock, evolt)
	rec := doReq(t, h.EvoltInitHandshake, http.MethodPost, "/", "{}")
	got := decode(t, rec)
	if rec.Code != http.StatusOK || !got.ok {
		t.Fatalf("evolt-init failed: %d %s", rec.Code, rec.Body.String())
	}
	if outboundBody["partner_versions_url"] != "https://demo.example.com/ocpi/versions" {
		t.Fatalf("versions url = %v", outboundBody["partner_versions_url"])
	}
	if outboundBody["token_a"] != "fresh-token-a" {
		t.Fatalf("token_a not forwarded")
	}
}

func TestPushEvseStatusValidation(t *testing.T) {
	h := newHandlers(t, Config{}, fakeMock(t, nil), nil)
	cases := []struct{ name, body string }{
		{"missing evse", `{"status":"AVAILABLE"}`},
		{"bad status", `{"evse_uid":"29","status":"NAPPING"}`},
		{"lowercase status", `{"evse_uid":"29","status":"available"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h.PushEvseStatus, http.MethodPost, "/", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d", rec.Code)
			}
		})
	}
}

func TestPushEvseStatusResolvesLocation(t *testing.T) {
	var pushed map[string]any
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/own/locations": jsonResp(`[{"id":"LOC1","evses":[{"uid":"29"}]},{"id":"LOC2","evses":[{"uid":"31"}]}]`),
		"POST /admin/push/evse-status": func(w http.ResponseWriter, r *http.Request) {
			pushed = mustDecode(r)
			w.Write([]byte(`{"accepted":true,"http_status":200,"ocpi_status_code":1000}`))
		},
	})
	h := newHandlers(t, Config{}, mock, nil)
	rec := doReq(t, h.PushEvseStatus, http.MethodPost, "/", `{"evse_uid":"31","status":"CHARGING"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if pushed["location_id"] != "LOC2" {
		t.Fatalf("resolved location = %v, want LOC2", pushed["location_id"])
	}

	rec = doReq(t, h.PushEvseStatus, http.MethodPost, "/", `{"evse_uid":"nope","status":"CHARGING"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown evse must 400, got %d", rec.Code)
	}
}

func TestPartnerPullValidation(t *testing.T) {
	h := newHandlers(t, Config{}, fakeMock(t, nil), nil)
	rec := doReq(t, h.PartnerPull, http.MethodPost, "/", `{}`, "kind", "cdrs")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", rec.Code)
	}
	rec = doReq(t, h.PartnerPull, http.MethodPost, "/", `{"limit":50}`, "kind", "locations")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit over max: %d", rec.Code)
	}
	rec = doReq(t, h.PartnerPull, http.MethodPost, "/", `{"limit":-1}`, "kind", "locations")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative limit: %d", rec.Code)
	}
}

func TestEvoltPullRequiresRegistration(t *testing.T) {
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/tokens/current": jsonResp(`{"status":"PENDING","token_inbound":"x","token_outbound":""}`),
	})
	h := newHandlers(t, Config{}, mock, nil)
	rec := doReq(t, h.EvoltPull, http.MethodPost, "/", `{}`, "kind", "locations")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEvoltPullHappy(t *testing.T) {
	var pullReq map[string]any
	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ocpi/tariffs/pull" {
			http.NotFound(w, r)
			return
		}
		pullReq = mustDecode(r)
		w.Write([]byte(`{"status_code":1000,"data":{"http_status":200,"body":[],"next_url":""}}`))
	}))
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/tokens/current": jsonResp(`{"status":"REGISTERED","token_inbound":"secret-inbound","token_outbound":"c"}`),
	})
	h := newHandlers(t, Config{PublicBaseURL: "https://demo.example.com"}, mock, evolt)
	rec := doReq(t, h.EvoltPull, http.MethodPost, "/", `{"limit":3}`, "kind", "tariffs")
	got := decode(t, rec)
	if rec.Code != http.StatusOK || !got.ok {
		t.Fatalf("pull failed: %d %s", rec.Code, rec.Body.String())
	}
	if pullReq["url"] != "https://demo.example.com/ocpi/cpo/2.2.1/tariffs" {
		t.Fatalf("pull url = %v", pullReq["url"])
	}
	if pullReq["token"] != "secret-inbound" {
		t.Fatal("inbound token not forwarded to adapter")
	}
	if pullReq["limit"] != float64(3) {
		t.Fatalf("limit = %v, want number 3", pullReq["limit"])
	}
	if strings.Contains(rec.Body.String(), "secret-inbound") {
		t.Fatal("inbound token leaked to the browser response")
	}
}

func TestEvoltTariffPushWhitelist(t *testing.T) {
	h := newHandlers(t, Config{AllowedStations: []string{"allowed-1"}}, fakeMock(t, nil), nil)
	rec := doReq(t, h.EvoltTariffPush, http.MethodPost, "/", `{"station_id":"other"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	rec = doReq(t, h.EvoltTariffPush, http.MethodPost, "/", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing station_id: %d", rec.Code)
	}
}

func TestEvoltTariffPushHappy(t *testing.T) {
	evolt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/tariffs/push" || r.URL.Query().Get("station_id") != "allowed-1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-API-Key") != "roaming-key" {
			t.Errorf("roaming called without api key")
		}
		w.Write([]byte(`{"code":"1000","data":{"synced":1}}`))
	}))
	mock := fakeMock(t, map[string]http.HandlerFunc{
		"GET /admin/received/tariffs": jsonResp(`{"tariffs":[{"tariff_id":"t1"}]}`),
	})
	h := newHandlers(t, Config{AllowedStations: []string{"allowed-1"}, RoamingAPIKey: "roaming-key"}, mock, evolt)
	rec := doReq(t, h.EvoltTariffPush, http.MethodPost, "/", `{"station_id":"allowed-1"}`)
	got := decode(t, rec)
	if rec.Code != http.StatusOK || !got.ok {
		t.Fatalf("tariff push failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestEvseStatusEventDisabledKafka(t *testing.T) {
	h := newHandlers(t, Config{}, fakeMock(t, nil), nil)
	rec := doReq(t, h.EvoltEvseStatusEvent, http.MethodPost, "/", `{"status":"AVAILABLE"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRequestsLimitValidation(t *testing.T) {
	h := newHandlers(t, Config{}, fakeMock(t, nil), nil)
	for _, q := range []string{"?limit=0", "?limit=201", "?limit=abc"} {
		rec := doReq(t, h.Requests, http.MethodGet, "/api/demo/requests"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d", q, rec.Code)
		}
	}
}

func TestOwnKindValidation(t *testing.T) {
	h := newHandlers(t, Config{}, fakeMock(t, nil), nil)
	rec := doReq(t, h.Own, http.MethodGet, "/", "", "kind", "secrets")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestDownstreamTimeoutMapsTo504(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{}`))
	}))
	cfg := Config{Timeout: 50 * time.Millisecond}
	h := newHandlers(t, cfg, nil, nil)
	h.mock = NewMockAdmin(Config{MockBaseURL: slow.URL, MockAdminKey: adminKey, Timeout: 50 * time.Millisecond})
	t.Cleanup(slow.Close)
	rec := doReq(t, h.Reset, http.MethodPost, "/", "")
	if rec.Code != http.StatusGatewayTimeout && rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !rl.allow("1.2.3.4", now) {
			t.Fatalf("request %d within burst must pass", i)
		}
	}
	if rl.allow("1.2.3.4", now) {
		t.Fatal("burst exceeded must be rejected")
	}
	if !rl.allow("5.6.7.8", now) {
		t.Fatal("another ip must have its own bucket")
	}
	if !rl.allow("1.2.3.4", now.Add(5*time.Second)) {
		t.Fatal("tokens must refill over time")
	}
}

func TestStationAllowed(t *testing.T) {
	cfg := Config{AllowedStations: []string{"a", "b"}}
	if !cfg.StationAllowed("a") || cfg.StationAllowed("c") || cfg.StationAllowed("") {
		t.Fatal("whitelist misbehaves")
	}
}

func TestOcpiProxyForwardsVerbatim(t *testing.T) {
	var got *http.Request
	var gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		b := make([]byte, 64)
		n, _ := r.Body.Read(b)
		gotBody = string(b[:n])
		w.Header().Set("X-Total-Count", "42")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status_code":1000}`))
	}))
	t.Cleanup(backend.Close)

	handler, err := ocpiProxy(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.Any("/ocpi/*", handler)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/ocpi/emsp/2.2.1/tariffs/TH/PLG/t1?x=1", strings.NewReader(`{"id":"t1"}`))
	req.Header.Set("Authorization", "Token c2VjcmV0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got.URL.Path != "/ocpi/emsp/2.2.1/tariffs/TH/PLG/t1" || got.URL.RawQuery != "x=1" {
		t.Fatalf("path not preserved: %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "Token c2VjcmV0" {
		t.Fatal("Authorization header not forwarded")
	}
	if gotBody != `{"id":"t1"}` {
		t.Fatalf("body not forwarded: %q", gotBody)
	}
	if resp.Header.Get("X-Total-Count") != "42" {
		t.Fatal("response header not passed back")
	}
}

func TestOcpiProxyMockDown(t *testing.T) {
	handler, err := ocpiProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	e.Any("/ocpi/*", handler)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/ocpi/versions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// Row shape here mirrors store.ReceivedRow exactly — its location id
// serializes as "key", and parsing any other field name broke baseline
// detection once already.
func TestEnsureKafkaBaseline(t *testing.T) {
	cfg := Config{
		KafkaStationID: "st-1", KafkaEvseUID: "29", KafkaEvseID: "uuid-29",
		KafkaPartyCC: "TH", KafkaPartyID: "EVO",
	}

	t.Run("baseline already present", func(t *testing.T) {
		injected := false
		mock := fakeMock(t, map[string]http.HandlerFunc{
			"GET /admin/received/locations": jsonResp(`{"locations":[{"country_code":"TH","party_id":"EVO","key":"st-1","payload":{}}]}`),
			"POST /admin/received/locations": func(w http.ResponseWriter, _ *http.Request) {
				injected = true
				w.Write([]byte(`{"stored":"st-1"}`))
			},
		})
		h := newHandlers(t, cfg, mock, nil)
		seeded, err := h.ensureKafkaBaseline(t.Context())
		if err != nil || seeded || injected {
			t.Fatalf("seeded=%v injected=%v err=%v — must detect existing baseline", seeded, injected, err)
		}
	})

	t.Run("baseline missing gets injected", func(t *testing.T) {
		var inject map[string]any
		mock := fakeMock(t, map[string]http.HandlerFunc{
			"GET /admin/received/locations": jsonResp(`{"locations":[{"key":"other-station"}]}`),
			"POST /admin/received/locations": func(w http.ResponseWriter, r *http.Request) {
				inject = mustDecode(r)
				w.Write([]byte(`{"stored":"st-1"}`))
			},
		})
		h := newHandlers(t, cfg, mock, nil)
		seeded, err := h.ensureKafkaBaseline(t.Context())
		if err != nil || !seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		if inject["country_code"] != "TH" || inject["party_id"] != "EVO" {
			t.Fatalf("inject party = %v/%v", inject["country_code"], inject["party_id"])
		}
		payload, _ := inject["payload"].(map[string]any)
		if payload["id"] != "st-1" {
			t.Fatalf("payload id = %v", payload["id"])
		}
	})
}

func TestOcpiProxyRejectsDotSegments(t *testing.T) {
	handler, err := ocpiProxy("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "http://demo/ocpi/../admin/state", nil)
	req.URL.Path = "/ocpi/../admin/state"
	rec := httptest.NewRecorder()
	if err := handler(e.NewContext(req, rec)); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
