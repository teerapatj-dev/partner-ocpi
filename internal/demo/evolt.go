package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// Evolt calls the three internal Evolt services the demo drives. Every call
// checks the envelope code, not just the HTTP status — orch/roaming answer
// BaseApiResponse (code "1000" string), the adapter answers the OCPI envelope
// (status_code 1000 int).
type Evolt struct {
	cfg   Config
	httpc *http.Client

	probeMu      sync.Mutex
	probeOK      bool
	probeChecked time.Time
}

func NewEvolt(cfg Config) *Evolt {
	return &Evolt{cfg: cfg, httpc: &http.Client{Timeout: cfg.Timeout}}
}

type evoltEnvelope struct {
	Data          json.RawMessage `json:"data"`
	StatusCode    int             `json:"status_code"`
	Code          string          `json:"code"`
	StatusMessage string          `json:"status_message"`
	Message       string          `json:"message"`
}

func (e evoltEnvelope) ok() bool {
	if e.StatusCode != 0 {
		return e.StatusCode == 1000
	}
	return e.Code == "1000"
}

func (e evoltEnvelope) describe() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("status_code=%d %s", e.StatusCode, e.StatusMessage)
	}
	return fmt.Sprintf("code=%s %s", e.Code, e.Message)
}

func (ev *Evolt) call(ctx context.Context, method, url, apiKey string, body any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ev.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("evolt %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("read evolt response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &DownstreamError{Source: "evolt", Status: resp.StatusCode, Body: jsonOrString(respBody)}
	}
	var env evoltEnvelope
	if err := json.Unmarshal(respBody, &env); err != nil || (env.StatusCode == 0 && env.Code == "") {
		return nil, fmt.Errorf("evolt returned an unrecognized response shape")
	}
	if !env.ok() {
		return nil, &DownstreamError{Source: "evolt (" + env.describe() + ")", Status: resp.StatusCode, Body: jsonOrString(respBody)}
	}
	return env.Data, nil
}

// callRaw is for endpoints that answer bare JSON on success (the adapter's pull passes
// TariffsPullResponse/LocationsPullResponse through with no envelope — verified against
// adapter-ocpi handler.go: errors come wrapped and non-2xx, success is c.JSON(200, res) raw).
func (ev *Evolt) callRaw(ctx context.Context, method, url, apiKey string, body any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := ev.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("evolt %s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("read evolt response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &DownstreamError{Source: "evolt", Status: resp.StatusCode, Body: jsonOrString(respBody)}
	}
	if !json.Valid(respBody) {
		return nil, fmt.Errorf("evolt returned a non-JSON body")
	}
	return respBody, nil
}

var errNotConfigured = fmt.Errorf("evolt url not configured — set the EVOLT_* env vars")

// PartnerInitial asks orch for a fresh Token A (Evolt side of a
// partner-initiated handshake).
func (ev *Evolt) PartnerInitial(ctx context.Context, partnerName string) (string, error) {
	if ev.cfg.EvoltOrchURL == "" {
		return "", errNotConfigured
	}
	data, err := ev.call(ctx, http.MethodPost, ev.cfg.EvoltOrchURL+"/ocpi/partner/initial", ev.cfg.OrchAPIKey,
		map[string]string{"partner_name": partnerName})
	if err != nil {
		return "", err
	}
	var out struct {
		TokenA string `json:"token_a"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.TokenA == "" {
		return "", fmt.Errorf("orch returned no token_a")
	}
	return out.TokenA, nil
}

// PartnerOutbound hands the mock's Token A to orch so Evolt drives the whole
// handshake (Roaming-Out entry point).
func (ev *Evolt) PartnerOutbound(ctx context.Context, partnerName, tokenA, versionsURL string) (json.RawMessage, error) {
	if ev.cfg.EvoltOrchURL == "" {
		return nil, errNotConfigured
	}
	return ev.call(ctx, http.MethodPost, ev.cfg.EvoltOrchURL+"/ocpi/partner/outbound", ev.cfg.OrchAPIKey,
		map[string]string{
			"partner_name":         partnerName,
			"token_a":              tokenA,
			"partner_versions_url": versionsURL,
		})
}

// AdapterPull makes Evolt fetch one page of the mock's CPO list — the demo
// stands in for the roaming pull cron that owns this call in production.
func (ev *Evolt) AdapterPull(ctx context.Context, module, url, token string, limit int) (json.RawMessage, error) {
	if ev.cfg.EvoltAdapterURL == "" {
		return nil, errNotConfigured
	}
	return ev.callRaw(ctx, http.MethodPost, ev.cfg.EvoltAdapterURL+"/ocpi/"+module+"/pull", "",
		map[string]any{"url": url, "token": token, "limit": limit})
}

// RoamingTariffPush triggers the same materialize+fanout the tariff_update
// consumer runs — idempotent on Evolt's side.
func (ev *Evolt) RoamingTariffPush(ctx context.Context, stationID string) (json.RawMessage, error) {
	if ev.cfg.EvoltRoamingURL == "" {
		return nil, errNotConfigured
	}
	return ev.call(ctx, http.MethodPost, ev.cfg.EvoltRoamingURL+"/internal/tariffs/push?station_id="+stationID,
		ev.cfg.RoamingAPIKey, nil)
}

// Reachable probes the public versions endpoint, cached for 30s — the dev
// cluster is switched off outside working hours and the UI needs to say so.
func (ev *Evolt) Reachable(ctx context.Context) bool {
	if ev.cfg.EvoltVersionsURL == "" {
		return false
	}
	// The stamp is refreshed before probing so concurrent state polls return
	// the cached value instead of queueing behind a slow probe — one request
	// pays for the refresh, the rest see up-to-35s-old data at worst.
	ev.probeMu.Lock()
	if time.Since(ev.probeChecked) < 30*time.Second {
		ok := ev.probeOK
		ev.probeMu.Unlock()
		return ok
	}
	ev.probeChecked = time.Now()
	ev.probeMu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, ev.cfg.EvoltVersionsURL, nil)
	if err != nil {
		return false
	}
	resp, err := ev.httpc.Do(req)
	if err == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	ok := err == nil && resp.StatusCode < 500

	ev.probeMu.Lock()
	ev.probeOK = ok
	ev.probeMu.Unlock()
	return ok
}
