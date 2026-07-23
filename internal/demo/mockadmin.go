package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const maxRespBytes = 1 << 20

// MockAdmin talks to the mock partner's admin API with the admin key. Errors
// carry the mock's response body so the UI can show what actually went wrong.
type MockAdmin struct {
	baseURL string
	key     string
	httpc   *http.Client
}

func NewMockAdmin(cfg Config) *MockAdmin {
	return &MockAdmin{
		baseURL: cfg.MockBaseURL,
		key:     cfg.MockAdminKey,
		httpc:   &http.Client{Timeout: cfg.Timeout},
	}
}

type DownstreamError struct {
	Source string
	Status int
	Body   json.RawMessage
}

func (e *DownstreamError) Error() string {
	return fmt.Sprintf("%s returned http %d", e.Source, e.Status)
}

func (m *MockAdmin) call(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, reqBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", m.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mock %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return nil, fmt.Errorf("read mock response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &DownstreamError{Source: "mock", Status: resp.StatusCode, Body: jsonOrString(respBody)}
	}
	return respBody, nil
}

func (m *MockAdmin) Get(ctx context.Context, path string) (json.RawMessage, error) {
	return m.call(ctx, http.MethodGet, path, nil)
}

func (m *MockAdmin) Post(ctx context.Context, path string, body any) (json.RawMessage, error) {
	return m.call(ctx, http.MethodPost, path, body)
}

func (m *MockAdmin) Delete(ctx context.Context, path string) (json.RawMessage, error) {
	return m.call(ctx, http.MethodDelete, path, nil)
}

// CurrentTokens exposes the mock's raw token pair to the demo process only —
// used to impersonate Evolt's missing pull cron; never returned to a browser.
func (m *MockAdmin) CurrentTokens(ctx context.Context) (inbound, outbound, status string, err error) {
	raw, err := m.Get(ctx, "/admin/tokens/current")
	if err != nil {
		return "", "", "", err
	}
	var out struct {
		Status        string `json:"status"`
		TokenInbound  string `json:"token_inbound"`
		TokenOutbound string `json:"token_outbound"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", "", "", fmt.Errorf("parse tokens/current: %w", err)
	}
	return out.TokenInbound, out.TokenOutbound, out.Status, nil
}

func jsonOrString(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	q, _ := json.Marshal(s)
	return q
}
