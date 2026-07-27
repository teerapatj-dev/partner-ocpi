// Package client is the initiator side of the mock: it dials Evolt for the
// partner-initiated handshake and for CPO pushes (evse status, tariffs).
//
// Evolt answers two different shapes (verified on origin/develop):
//   - GET /api/ocpi/versions → raw JSON array, no envelope
//   - version details / credentials → BaseApiResponse{code:"1000"(string), data}
//
// while everything the mock serves uses the OCPI envelope (status_code int).
// parseEnvelope accepts all of these.
package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"mock-ocpi-partner/internal/config"
	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

const maxRespBytes = 1 << 20

type Client struct {
	cfg   config.Config
	st    *store.Store
	httpc *http.Client
}

func New(cfg config.Config, st *store.Store) *Client {
	return &Client{
		cfg: cfg,
		st:  st,
		httpc: &http.Client{
			Timeout: cfg.OutboundTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

type callResult struct {
	HTTPStatus int
	Body       []byte
	Header     http.Header
}

func (c *Client) do(ctx context.Context, method, url, token string, body any, toCC, toPID string) (callResult, error) {
	var (
		reqBody io.Reader
		excerpt string
	)
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return callResult{}, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
		excerpt = ocpi.RedactTokens(string(b))
		if len(excerpt) > 2048 {
			excerpt = excerpt[:2048]
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return callResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+base64.StdEncoding.EncodeToString([]byte(token)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Request-ID", uuid.NewString())
	req.Header.Set("X-Correlation-ID", uuid.NewString())
	req.Header.Set("OCPI-from-country-code", c.cfg.CountryCode)
	req.Header.Set("OCPI-from-party-id", c.cfg.PartyID)
	if toCC != "" && toPID != "" {
		req.Header.Set("OCPI-to-country-code", toCC)
		req.Header.Set("OCPI-to-party-id", toPID)
	}

	resp, err := c.httpc.Do(req)
	entry := store.LogEntry{
		Direction:    "out",
		Method:       method,
		Path:         url,
		Counterparty: strings.TrimSuffix(toCC+"-"+toPID, "-"),
		BodyExcerpt:  excerpt,
		AuthPresent:  true,
	}
	if err != nil {
		if logErr := c.st.AppendLog(ctx, entry); logErr != nil {
			log.Warn().Err(logErr).Msg("append outbound log failed")
		}
		return callResult{}, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	entry.HTTPStatus = resp.StatusCode
	if code, ok := parseAnyStatus(respBody); ok {
		entry.OCPIStatusCode = code
	}
	if logErr := c.st.AppendLog(ctx, entry); logErr != nil {
		log.Warn().Err(logErr).Msg("append outbound log failed")
	}
	if err != nil {
		return callResult{}, fmt.Errorf("read response of %s %s: %w", method, url, err)
	}
	return callResult{HTTPStatus: resp.StatusCode, Body: respBody, Header: resp.Header}, nil
}

// flexEnvelope covers both the OCPI envelope (status_code int) and Evolt's
// internal BaseApiResponse (code string).
type flexEnvelope struct {
	Data          json.RawMessage `json:"data"`
	StatusCode    int             `json:"status_code"`
	Code          string          `json:"code"`
	StatusMessage string          `json:"status_message"`
	Message       string          `json:"message"`
}

func (e flexEnvelope) ok() bool {
	if e.StatusCode != 0 {
		return e.StatusCode == ocpi.StatusSuccess
	}
	return e.Code == "1000"
}

func (e flexEnvelope) describe() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("status_code=%d %s", e.StatusCode, e.StatusMessage)
	}
	return fmt.Sprintf("code=%s %s", e.Code, e.Message)
}

func parseAnyStatus(body []byte) (int, bool) {
	var e flexEnvelope
	if json.Unmarshal(body, &e) != nil {
		return 0, false
	}
	if e.StatusCode != 0 {
		return e.StatusCode, true
	}
	if n, err := strconv.Atoi(e.Code); err == nil && n != 0 {
		return n, true
	}
	return 0, false
}

// parseEnvelope unwraps a response into target: raw payload, OCPI envelope, or
// BaseApiResponse — whichever matches.
func parseEnvelope(res callResult, target any) error {
	if res.HTTPStatus < 200 || res.HTTPStatus > 299 {
		return fmt.Errorf("http status %d", res.HTTPStatus)
	}
	var env flexEnvelope
	if err := json.Unmarshal(res.Body, &env); err == nil && (env.StatusCode != 0 || env.Code != "") {
		if !env.ok() {
			return fmt.Errorf("rejected: %s", env.describe())
		}
		if len(env.Data) == 0 {
			return fmt.Errorf("success envelope without data")
		}
		return json.Unmarshal(env.Data, target)
	}
	if err := json.Unmarshal(res.Body, target); err != nil {
		return fmt.Errorf("unrecognized response shape: %w", err)
	}
	return nil
}

type HandshakeResult struct {
	VersionsURL    string   `json:"versions_url"`
	VersionURL     string   `json:"version_url"`
	CredentialsURL string   `json:"credentials_url"`
	Counterparty   string   `json:"counterparty"`
	Steps          []string `json:"steps"`
}

// Handshake runs the partner-initiated registration against Evolt:
// GET versions → GET version details → POST credentials.
// The freshly generated inbound token is persisted BEFORE the POST because
// Evolt calls back GET /ocpi/versions with it while handling the request.
func (c *Client) Handshake(ctx context.Context, versionsURL, tokenA string) (HandshakeResult, error) {
	out := HandshakeResult{VersionsURL: versionsURL}

	res, err := c.do(ctx, http.MethodGet, versionsURL, tokenA, nil, "", "")
	if err != nil {
		return out, fmt.Errorf("get versions: %w", err)
	}
	var versions []ocpi.Version
	if err := parseEnvelope(res, &versions); err != nil {
		return out, fmt.Errorf("parse versions: %w", err)
	}
	out.Steps = append(out.Steps, "GET versions ok")
	for _, v := range versions {
		if v.Version == "2.2.1" {
			out.VersionURL = v.URL
			break
		}
	}
	if out.VersionURL == "" {
		return out, fmt.Errorf("counterparty does not offer version 2.2.1")
	}

	res, err = c.do(ctx, http.MethodGet, out.VersionURL, tokenA, nil, "", "")
	if err != nil {
		return out, fmt.Errorf("get version details: %w", err)
	}
	var details ocpi.VersionDetails
	if err := parseEnvelope(res, &details); err != nil {
		return out, fmt.Errorf("parse version details: %w", err)
	}
	out.Steps = append(out.Steps, fmt.Sprintf("GET version details ok (%d endpoints)", len(details.Endpoints)))
	for _, ep := range details.Endpoints {
		if strings.EqualFold(ep.Identifier, "credentials") {
			out.CredentialsURL = ep.URL
			break
		}
	}
	if out.CredentialsURL == "" {
		return out, fmt.Errorf("counterparty offers no credentials endpoint")
	}

	newInbound := uuid.NewString()
	endpointsJSON, err := json.Marshal(details.Endpoints)
	if err != nil {
		return out, fmt.Errorf("marshal endpoints: %w", err)
	}
	// Snapshot the current registration: the new inbound token must be live
	// before the POST (Evolt calls back with it mid-request), but if the POST
	// then fails we must restore the old token or a previously REGISTERED
	// counterparty is locked out.
	prev, hadPrev, err := c.currentRegistration(ctx)
	if err != nil {
		return out, fmt.Errorf("read current registration: %w", err)
	}
	reg, err := c.upsertPending(ctx, versionsURL, newInbound, endpointsJSON)
	if err != nil {
		return out, fmt.Errorf("persist pending registration: %w", err)
	}
	rollback := func() {
		if !hadPrev {
			return
		}
		if rbErr := c.st.UpdateRegistration(ctx, prev); rbErr != nil {
			log.Error().Err(rbErr).Msg("handshake rollback failed — registration may need manual repair")
		}
	}

	body := ocpi.Credentials{
		Token: newInbound,
		URL:   c.cfg.BaseURL + "/ocpi/versions",
		Roles: ocpi.SelfRoles(c.cfg.CountryCode, c.cfg.PartyID, c.cfg.PartnerName),
	}
	res, err = c.do(ctx, http.MethodPost, out.CredentialsURL, tokenA, body, "", "")
	if err != nil {
		rollback()
		return out, fmt.Errorf("post credentials: %w", err)
	}
	var granted ocpi.Credentials
	if err := parseEnvelope(res, &granted); err != nil {
		rollback()
		return out, fmt.Errorf("parse credentials response: %w", err)
	}
	if granted.Token == "" {
		rollback()
		return out, fmt.Errorf("counterparty returned empty token")
	}
	out.Steps = append(out.Steps, "POST credentials ok")

	reg.TokenOutbound = granted.Token
	reg.Status = store.StatusRegistered
	if len(granted.Roles) > 0 {
		reg.CountryCode = strings.ToUpper(granted.Roles[0].CountryCode)
		reg.PartyID = strings.ToUpper(granted.Roles[0].PartyID)
		reg.PartyName = granted.Roles[0].BusinessDetails.Name
	}
	if err := c.st.UpdateRegistration(ctx, reg); err != nil {
		return out, fmt.Errorf("persist registration: %w", err)
	}
	out.Counterparty = reg.CountryCode + "-" + reg.PartyID
	out.Steps = append(out.Steps, "REGISTERED")
	return out, nil
}

func (c *Client) currentRegistration(ctx context.Context) (store.Registration, bool, error) {
	regs, err := c.st.ListRegistrations(ctx)
	if err != nil {
		return store.Registration{}, false, err
	}
	if len(regs) == 0 {
		return store.Registration{}, false, nil
	}
	return regs[0], true, nil
}

// upsertPending reuses the existing registration row if one exists — the mock
// only ever talks to a single Evolt environment.
func (c *Client) upsertPending(ctx context.Context, versionsURL, tokenInbound string, endpoints json.RawMessage) (store.Registration, error) {
	regs, err := c.st.ListRegistrations(ctx)
	if err != nil {
		return store.Registration{}, err
	}
	if len(regs) > 0 {
		reg := regs[0]
		reg.TokenInbound = tokenInbound
		reg.Status = store.StatusPending
		reg.VersionsURL = versionsURL
		reg.Endpoints = endpoints
		reg.InitiatedBy = "SELF"
		if err := c.st.UpdateRegistration(ctx, reg); err != nil {
			return store.Registration{}, err
		}
		return reg, nil
	}
	return c.st.CreateRegistration(ctx, store.Registration{
		TokenInbound: tokenInbound,
		Status:       store.StatusPending,
		VersionsURL:  versionsURL,
		Endpoints:    endpoints,
		InitiatedBy:  "SELF",
	})
}

// FetchCounterparty pulls versions+endpoints of the party that just POSTed
// credentials to us (the OCPI-mandated callback). Best-effort by default:
// Evolt's orch URL on dev is cluster-internal and often unreachable from a
// cloud-hosted mock.
func (c *Client) FetchCounterparty(ctx context.Context, versionsURL, token string) (json.RawMessage, error) {
	res, err := c.do(ctx, http.MethodGet, versionsURL, token, nil, "", "")
	if err != nil {
		return nil, fmt.Errorf("callback get versions: %w", err)
	}
	var versions []ocpi.Version
	if err := parseEnvelope(res, &versions); err != nil {
		return nil, fmt.Errorf("callback parse versions: %w", err)
	}
	var detailsURL string
	for _, v := range versions {
		if v.Version == "2.2.1" {
			detailsURL = v.URL
			break
		}
	}
	if detailsURL == "" {
		return nil, fmt.Errorf("callback: no 2.2.1 version offered")
	}
	res, err = c.do(ctx, http.MethodGet, detailsURL, token, nil, "", "")
	if err != nil {
		return nil, fmt.Errorf("callback get version details: %w", err)
	}
	var details ocpi.VersionDetails
	if err := parseEnvelope(res, &details); err != nil {
		return nil, fmt.Errorf("callback parse version details: %w", err)
	}
	return json.Marshal(details.Endpoints)
}

type PushResult struct {
	URL            string `json:"url"`
	HTTPStatus     int    `json:"http_status"`
	OCPIStatusCode int    `json:"ocpi_status_code"`
	Accepted       bool   `json:"accepted"`
}

func (c *Client) endpointURL(reg store.Registration, identifier, role string) (string, error) {
	var endpoints []ocpi.Endpoint
	if len(reg.Endpoints) > 0 {
		if err := json.Unmarshal(reg.Endpoints, &endpoints); err != nil {
			return "", fmt.Errorf("stored endpoints unreadable: %w", err)
		}
	}
	for _, ep := range endpoints {
		if strings.EqualFold(ep.Identifier, identifier) && strings.EqualFold(ep.Role, role) {
			return ep.URL, nil
		}
	}
	return "", fmt.Errorf("counterparty has no %s %s endpoint (callback may have been skipped)", identifier, role)
}

func (c *Client) receiverURL(reg store.Registration, identifier string) (string, error) {
	return c.endpointURL(reg, identifier, "RECEIVER")
}

func (c *Client) pushObject(ctx context.Context, method, identifier, suffix string, body any) (PushResult, error) {
	reg, err := c.st.FirstRegistered(ctx)
	if err != nil {
		return PushResult{}, fmt.Errorf("no REGISTERED counterparty: %w", err)
	}
	base, err := c.receiverURL(reg, identifier)
	if err != nil {
		return PushResult{}, err
	}
	url := strings.TrimRight(base, "/") + "/" + c.cfg.CountryCode + "/" + c.cfg.PartyID + suffix
	res, err := c.do(ctx, method, url, reg.TokenOutbound, body, reg.CountryCode, reg.PartyID)
	if err != nil {
		return PushResult{URL: url}, err
	}
	code, _ := parseAnyStatus(res.Body)
	accepted := res.HTTPStatus >= 200 && res.HTTPStatus <= 299 && code == ocpi.StatusSuccess
	if method == http.MethodDelete && res.HTTPStatus >= 200 && res.HTTPStatus <= 299 && code == ocpi.StatusUnknownResource {
		accepted = true
	}
	return PushResult{URL: url, HTTPStatus: res.HTTPStatus, OCPIStatusCode: code, Accepted: accepted}, nil
}

func (c *Client) PushLocation(ctx context.Context, locationID string, payload json.RawMessage) (PushResult, error) {
	return c.pushObject(ctx, http.MethodPut, "locations", "/"+locationID, payload)
}

func (c *Client) PushEvseStatus(ctx context.Context, locationID, evseUID, status string) (PushResult, error) {
	body := map[string]string{"status": status, "last_updated": ocpi.Now()}
	return c.pushObject(ctx, http.MethodPatch, "locations", "/"+locationID+"/"+evseUID, body)
}

func (c *Client) PushTariff(ctx context.Context, tariffID string, payload json.RawMessage) (PushResult, error) {
	return c.pushObject(ctx, http.MethodPut, "tariffs", "/"+tariffID, payload)
}

func (c *Client) DeleteTariff(ctx context.Context, tariffID string) (PushResult, error) {
	return c.pushObject(ctx, http.MethodDelete, "tariffs", "/"+tariffID, nil)
}

type PullResult struct {
	URL            string          `json:"url"`
	HTTPStatus     int             `json:"http_status"`
	OCPIStatusCode int             `json:"ocpi_status_code"`
	Total          *int            `json:"total,omitempty"`
	Items          json.RawMessage `json:"items,omitempty"`
	Stored         *int            `json:"stored,omitempty"`
}

// PullFromCounterparty GETs the first page of the counterparty's SENDER list
// (locations or tariffs) with the outbound token — the same request Evolt's
// future pull cron will make against us, pointed the other way.
func (c *Client) PullFromCounterparty(ctx context.Context, identifier string, limit int) (PullResult, error) {
	reg, err := c.st.FirstRegistered(ctx)
	if err != nil {
		return PullResult{}, fmt.Errorf("no REGISTERED counterparty: %w", err)
	}
	base, err := c.endpointURL(reg, identifier, "SENDER")
	if err != nil {
		return PullResult{}, err
	}
	url := fmt.Sprintf("%s?offset=0&limit=%d", strings.TrimRight(base, "/"), limit)
	res, err := c.do(ctx, http.MethodGet, url, reg.TokenOutbound, nil, reg.CountryCode, reg.PartyID)
	if err != nil {
		return PullResult{URL: url}, err
	}
	out := PullResult{URL: url, HTTPStatus: res.HTTPStatus}
	out.OCPIStatusCode, _ = parseAnyStatus(res.Body)
	if v := res.Header.Get("X-Total-Count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			out.Total = &n
		}
	}
	var items json.RawMessage
	if err := parseEnvelope(res, &items); err != nil {
		return out, fmt.Errorf("parse %s page: %w", identifier, err)
	}
	out.Items = items
	// A real eMSP caches what it pulls; do the same so the pulled objects land in received_* and the
	// demo's partner-side panels/tables show them (best-effort — a store slip never fails the pull).
	if n := c.cachePulled(ctx, identifier, items, reg.CountryCode, reg.PartyID); n > 0 {
		out.Stored = &n
	}
	return out, nil
}

// PullAllResult summarises a whole-catalog sync: every page walked, everything cached.
type PullAllResult struct {
	URL            string `json:"url"`
	Pages          int    `json:"pages"`
	HTTPStatus     int    `json:"http_status"`      // last page's
	OCPIStatusCode int    `json:"ocpi_status_code"` // last page's
	Total          *int   `json:"total,omitempty"`
	Stored         int    `json:"stored"`
}

// PullAllFromCounterparty walks the counterparty's whole SENDER list the way a real eMSP runs its
// initial sync after registration: page after page until the feed is exhausted (or maxPages, a
// runaway guard). Every object lands in received_* exactly as a single-page pull would store it.
func (c *Client) PullAllFromCounterparty(ctx context.Context, identifier string, pageLimit, maxPages int) (PullAllResult, error) {
	reg, err := c.st.FirstRegistered(ctx)
	if err != nil {
		return PullAllResult{}, fmt.Errorf("no REGISTERED counterparty: %w", err)
	}
	base, err := c.endpointURL(reg, identifier, "SENDER")
	if err != nil {
		return PullAllResult{}, err
	}
	out := PullAllResult{URL: strings.TrimRight(base, "/")}
	for offset := 0; out.Pages < maxPages; offset += pageLimit {
		url := fmt.Sprintf("%s?offset=%d&limit=%d", out.URL, offset, pageLimit)
		res, err := c.do(ctx, http.MethodGet, url, reg.TokenOutbound, nil, reg.CountryCode, reg.PartyID)
		if err != nil {
			return out, err
		}
		out.Pages++
		out.HTTPStatus = res.HTTPStatus
		out.OCPIStatusCode, _ = parseAnyStatus(res.Body)
		if v := res.Header.Get("X-Total-Count"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				out.Total = &n
			}
		}
		var items json.RawMessage
		if err := parseEnvelope(res, &items); err != nil {
			return out, fmt.Errorf("parse %s page at offset %d: %w", identifier, offset, err)
		}
		out.Stored += c.cachePulled(ctx, identifier, items, reg.CountryCode, reg.PartyID)
		var arr []json.RawMessage
		if err := json.Unmarshal(items, &arr); err != nil || len(arr) < pageLimit {
			break // short (or empty) page = the feed is done
		}
		if out.Total != nil && offset+pageLimit >= *out.Total {
			break
		}
	}
	return out, nil
}

// cachePulled writes each pulled CPO object into received_* (locations explode into evses/connectors).
// country_code/party_id come off each object, falling back to the counterparty's when absent.
func (c *Client) cachePulled(ctx context.Context, kind string, items json.RawMessage, fbCC, fbPID string) int {
	var arr []json.RawMessage
	if err := json.Unmarshal(items, &arr); err != nil {
		return 0
	}
	stored := 0
	for _, raw := range arr {
		var obj struct {
			CountryCode string `json:"country_code"`
			PartyID     string `json:"party_id"`
			ID          string `json:"id"`
			LastUpdated string `json:"last_updated"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil || obj.ID == "" {
			continue
		}
		cc, pid := strings.ToUpper(obj.CountryCode), strings.ToUpper(obj.PartyID)
		if cc == "" {
			cc = strings.ToUpper(fbCC)
		}
		if pid == "" {
			pid = strings.ToUpper(fbPID)
		}
		lu, err := time.Parse(time.RFC3339, obj.LastUpdated)
		if err != nil {
			lu = time.Now().UTC()
		}
		switch kind {
		case "locations":
			err = c.st.PutReceivedLocationObject(ctx, store.ObjectKey{CountryCode: cc, PartyID: pid, LocationID: obj.ID}, raw, lu)
		case "tariffs":
			err = c.st.PutReceivedTariff(ctx, cc, pid, obj.ID, raw, lu)
		}
		if err != nil {
			log.Warn().Err(err).Str("kind", kind).Str("id", obj.ID).Msg("pull: cache object failed")
			continue
		}
		stored++
	}
	return stored
}
