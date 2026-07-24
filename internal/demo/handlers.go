package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"
)

var evseStatuses = map[string]bool{
	"AVAILABLE": true, "CHARGING": true, "BLOCKED": true,
	"INOPERATIVE": true, "OUTOFORDER": true,
}

type Handlers struct {
	cfg     Config
	mock    *MockAdmin
	evolt   *Evolt
	kafka   *Kafka
	charger *ChargerDB // nil when the charger simulator is not configured
}

func NewHandlers(cfg Config, mock *MockAdmin, evolt *Evolt, kafka *Kafka, charger *ChargerDB) *Handlers {
	return &Handlers{cfg: cfg, mock: mock, evolt: evolt, kafka: kafka, charger: charger}
}

func ok(c echo.Context, data any) error {
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "data": data})
}

func fail(c echo.Context, status int, msg string, detail any) error {
	body := map[string]any{"ok": false, "error": msg}
	if detail != nil {
		body["detail"] = detail
	}
	return c.JSON(status, body)
}

// failFrom maps a downstream error onto the demo envelope: the caller's fault
// stays 4xx, a downstream rejection is 502, a timeout 504.
func failFrom(c echo.Context, err error) error {
	var de *DownstreamError
	if errors.As(err, &de) {
		return fail(c, http.StatusBadGateway, de.Error(), de.Body)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fail(c, http.StatusGatewayTimeout, "downstream timeout", nil)
	}
	return fail(c, http.StatusBadGateway, err.Error(), nil)
}

func (h *Handlers) State(c echo.Context) error {
	ctx := c.Request().Context()
	var degraded []string

	partnerState, err := h.mock.Get(ctx, "/admin/state")
	if err != nil {
		degraded = append(degraded, "mock")
	}
	registrations, err := h.mock.Get(ctx, "/admin/registrations")
	if err != nil && len(degraded) == 0 {
		degraded = append(degraded, "mock")
	}
	kafkaEnabled, kafkaReason := h.kafka.Enabled()

	return ok(c, map[string]any{
		"partner":       json.RawMessage(orNull(partnerState)),
		"registrations": json.RawMessage(orNull(registrations)),
		"evolt": map[string]any{
			"configured": h.cfg.EvoltVersionsURL != "",
			"reachable":  h.evolt.Reachable(ctx),
		},
		"kafka":            map[string]any{"enabled": kafkaEnabled, "reason": kafkaReason},
		"allowed_stations": h.cfg.AllowedStations,
		"public_base_url":  h.cfg.PublicBaseURL,
		"degraded":         degraded,
		"time":             time.Now().UTC().Format(time.RFC3339),
	})
}

func orNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// PartnerInitHandshake runs the partner-initiated registration end to end:
// a fresh Token A from orch, then the mock walks versions → details →
// credentials against Evolt.
func (h *Handlers) PartnerInitHandshake(c echo.Context) error {
	ctx := c.Request().Context()
	if h.cfg.EvoltVersionsURL == "" {
		return fail(c, http.StatusBadGateway, errNotConfigured.Error(), nil)
	}
	partnerName, err := h.partnerName(ctx)
	if err != nil {
		return failFrom(c, err)
	}
	tokenA, err := h.evolt.PartnerInitial(ctx, partnerName)
	if err != nil {
		return failFrom(c, err)
	}
	result, err := h.mock.Post(ctx, "/admin/handshake", map[string]string{
		"evolt_versions_url": h.cfg.EvoltVersionsURL,
		"token_a":            tokenA,
	})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

// EvoltInitHandshake is the opposite direction: the mock issues Token A and
// Evolt drives the whole handshake through orch.
func (h *Handlers) EvoltInitHandshake(c echo.Context) error {
	ctx := c.Request().Context()
	if h.cfg.PublicBaseURL == "" {
		return fail(c, http.StatusBadGateway, "PUBLIC_BASE_URL not configured — Evolt cannot reach the mock without the tunnel", nil)
	}
	partnerName, err := h.partnerName(ctx)
	if err != nil {
		return failFrom(c, err)
	}
	tokenRes, err := h.mock.Post(ctx, "/admin/tokens?force=true", nil)
	if err != nil {
		return failFrom(c, err)
	}
	var token struct {
		TokenA string `json:"token_a"`
	}
	if err := json.Unmarshal(tokenRes, &token); err != nil || token.TokenA == "" {
		return fail(c, http.StatusBadGateway, "mock returned no token_a", nil)
	}
	result, err := h.evolt.PartnerOutbound(ctx, partnerName, token.TokenA, h.cfg.PublicBaseURL+"/ocpi/versions")
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) partnerName(ctx context.Context) (string, error) {
	raw, err := h.mock.Get(ctx, "/admin/state")
	if err != nil {
		return "", err
	}
	var state struct {
		Partner struct {
			Name string `json:"name"`
		} `json:"partner"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || state.Partner.Name == "" {
		return "", fmt.Errorf("mock state has no partner name")
	}
	return state.Partner.Name, nil
}

func (h *Handlers) partnerParty(ctx context.Context) (string, string, error) {
	raw, err := h.mock.Get(ctx, "/admin/state")
	if err != nil {
		return "", "", err
	}
	var state struct {
		Partner struct {
			CountryCode string `json:"country_code"`
			PartyID     string `json:"party_id"`
		} `json:"partner"`
	}
	if err := json.Unmarshal(raw, &state); err != nil || state.Partner.PartyID == "" {
		return "", "", fmt.Errorf("mock state has no partner party")
	}
	return state.Partner.CountryCode, state.Partner.PartyID, nil
}

// EvoltMirror answers what Evolt itself holds about this partner: the registration id it filed us
// under, the cache watermark per module, and — when a tariff id is given — the stored Tariff read
// straight back out. It exists so a demo run can be believed without opening the database: a push
// that returned 1000 but never landed shows up here as an unchanged watermark.
//
// Every leg is optional. The dev cluster goes down outside working hours and a half-answer with a
// note beats an error page that hides the parts that did work.
func (h *Handlers) EvoltMirror(c echo.Context) error {
	ctx := c.Request().Context()
	out := map[string]any{"configured": h.cfg.EvoltCoreAuthURL != "" && h.cfg.EvoltRoamingURL != ""}
	var notes []string

	countryCode, partyID, err := h.partnerParty(ctx)
	if err != nil {
		return fail(c, http.StatusBadGateway, "mock unreachable", nil)
	}
	out["country_code"], out["party_id"] = countryCode, partyID

	credentialsID, err := h.evolt.PartnerCredentialsID(ctx, countryCode, partyID)
	if err != nil {
		out["registered"] = false
		if !errors.Is(err, errNotRegistered) {
			notes = append(notes, describeErr(err))
		}
		out["notes"] = notes
		return ok(c, out)
	}
	out["registered"], out["credentials_id"] = true, credentialsID

	for _, module := range []string{"locations", "tariffs"} {
		data, err := h.evolt.PartnerCacheWatermark(ctx, module, credentialsID, countryCode, partyID)
		if err != nil {
			notes = append(notes, module+": "+describeErr(err))
			continue
		}
		out[module] = json.RawMessage(orNull(data))
	}

	if tariffID := c.QueryParam("tariff_id"); tariffID != "" {
		data, err := h.evolt.PartnerTariffReadback(ctx, credentialsID, countryCode, partyID, tariffID)
		switch {
		case err != nil && !isEnvCode(err, codeTariffNotFound):
			notes = append(notes, "tariff "+tariffID+": "+describeErr(err))
		case err != nil:
			// Not-found is the expected answer before the first push; the UI says so in place of the
			// tariff rather than as a warning.
		default:
			out["tariff"] = json.RawMessage(orNull(data))
			out["tariff_id"] = tariffID
		}
	}

	out["notes"] = notes
	return ok(c, out)
}

// codeTariffNotFound is roaming's business code for a tariff the partner cache does not hold.
const codeTariffNotFound = "2800"

func isEnvCode(err error, code string) bool {
	var de *DownstreamError
	return errors.As(err, &de) && de.EnvCode == code
}

// describeErr keeps a downstream failure readable in the UI without leaking the response body a
// DownstreamError carries (it can hold whatever the upstream chose to echo back).
func describeErr(err error) string {
	var de *DownstreamError
	if errors.As(err, &de) {
		return de.Error()
	}
	return err.Error()
}

func (h *Handlers) PushLocation(c echo.Context) error {
	var req struct {
		LocationID string `json:"location_id"`
	}
	if err := c.Bind(&req); err != nil || req.LocationID == "" {
		return fail(c, http.StatusBadRequest, "location_id is required", nil)
	}
	result, err := h.mock.Post(c.Request().Context(), "/admin/push/location", map[string]string{"location_id": req.LocationID})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) PushEvseStatus(c echo.Context) error {
	var req struct {
		EvseUID string `json:"evse_uid"`
		Status  string `json:"status"`
	}
	if err := c.Bind(&req); err != nil || req.EvseUID == "" {
		return fail(c, http.StatusBadRequest, "evse_uid and status are required", nil)
	}
	if !evseStatuses[req.Status] {
		return fail(c, http.StatusBadRequest, "status must be one of AVAILABLE, CHARGING, BLOCKED, INOPERATIVE, OUTOFORDER", nil)
	}
	ctx := c.Request().Context()
	locationID, err := h.findLocationForEvse(ctx, req.EvseUID)
	if err != nil {
		return failFrom(c, err)
	}
	if locationID == "" {
		return fail(c, http.StatusBadRequest, "unknown evse_uid — not in own locations", nil)
	}
	result, err := h.mock.Post(ctx, "/admin/push/evse-status", map[string]string{
		"location_id": locationID, "evse_uid": req.EvseUID, "status": req.Status,
	})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) findLocationForEvse(ctx context.Context, evseUID string) (string, error) {
	raw, err := h.mock.Get(ctx, "/admin/own/locations")
	if err != nil {
		return "", err
	}
	var locations []struct {
		ID    string `json:"id"`
		Evses []struct {
			UID string `json:"uid"`
		} `json:"evses"`
	}
	if err := json.Unmarshal(raw, &locations); err != nil {
		return "", fmt.Errorf("parse own locations: %w", err)
	}
	for _, loc := range locations {
		for _, evse := range loc.Evses {
			if evse.UID == evseUID {
				return loc.ID, nil
			}
		}
	}
	return "", nil
}

func (h *Handlers) PushTariff(c echo.Context) error {
	var req struct {
		TariffID string `json:"tariff_id"`
	}
	if err := c.Bind(&req); err != nil || req.TariffID == "" {
		return fail(c, http.StatusBadRequest, "tariff_id is required", nil)
	}
	result, err := h.mock.Post(c.Request().Context(), "/admin/push/tariff", map[string]string{"tariff_id": req.TariffID})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) DeleteTariffPush(c echo.Context) error {
	tariffID := c.Param("tariff_id")
	if tariffID == "" {
		return fail(c, http.StatusBadRequest, "tariff_id is required", nil)
	}
	result, err := h.mock.Delete(c.Request().Context(), "/admin/push/tariff/"+tariffID)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func bindLimit(c echo.Context, def, max int) (int, error) {
	var req struct {
		Limit *int `json:"limit"`
	}
	if err := c.Bind(&req); err != nil {
		return 0, fmt.Errorf("invalid body")
	}
	if req.Limit == nil {
		return def, nil
	}
	if *req.Limit < 1 || *req.Limit > max {
		return 0, fmt.Errorf("limit must be 1..%d", max)
	}
	return *req.Limit, nil
}

// PartnerPull: the mock pulls a page of Evolt's CPO list with its own Token C.
func (h *Handlers) PartnerPull(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	limit, err := bindLimit(c, 5, 20)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error(), nil)
	}
	result, err := h.mock.Post(c.Request().Context(), "/admin/pull/"+kind, map[string]int{"limit": limit})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

// EvoltPull: Evolt's adapter pulls a page of the mock's CPO list — the demo
// supplies the token Evolt holds (the mock's inbound token), standing in for
// the roaming pull cron that owns this call in production.
func (h *Handlers) EvoltPull(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	limit, err := bindLimit(c, 5, 20)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error(), nil)
	}
	ctx := c.Request().Context()
	tokenInbound, _, status, err := h.mock.CurrentTokens(ctx)
	if err != nil {
		return failFrom(c, err)
	}
	if status != "REGISTERED" || tokenInbound == "" {
		return fail(c, http.StatusConflict, "no completed registration — run a handshake first", nil)
	}
	pullURL := h.cfg.MockBaseURL
	if h.cfg.PublicBaseURL != "" {
		pullURL = h.cfg.PublicBaseURL
	}
	result, err := h.evolt.AdapterPull(ctx, kind, pullURL+"/ocpi/cpo/2.2.1/"+kind, tokenInbound, limit)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) EvoltTariffPush(c echo.Context) error {
	var req struct {
		StationID string `json:"station_id"`
	}
	if err := c.Bind(&req); err != nil || req.StationID == "" {
		return fail(c, http.StatusBadRequest, "station_id is required", nil)
	}
	if !h.cfg.StationAllowed(req.StationID) {
		return fail(c, http.StatusForbidden, "station not allowed", nil)
	}
	ctx := c.Request().Context()
	result, err := h.evolt.RoamingTariffPush(ctx, req.StationID)
	if err != nil {
		return failFrom(c, err)
	}
	received, _ := h.mock.Get(ctx, "/admin/received/tariffs")
	return ok(c, map[string]any{
		"sync":             json.RawMessage(result),
		"received_tariffs": json.RawMessage(orNull(received)),
	})
}

// EvoltEvseStatusEvent produces a charger status event on the real dev topic;
// the roaming consumer turns it into a PATCH back into the mock through the
// tunnel. The mock replica needs a baseline location for that PATCH to land,
// so one is seeded first if missing.
func (h *Handlers) EvoltEvseStatusEvent(c echo.Context) error {
	if enabled, reason := h.kafka.Enabled(); !enabled {
		return fail(c, http.StatusNotFound, "kafka demo disabled", reason)
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.Bind(&req); err != nil || !evseStatuses[req.Status] {
		return fail(c, http.StatusBadRequest, "status must be one of AVAILABLE, CHARGING, BLOCKED, INOPERATIVE, OUTOFORDER", nil)
	}
	ctx := c.Request().Context()
	seeded, err := h.ensureKafkaBaseline(ctx)
	if err != nil {
		return failFrom(c, err)
	}

	// Flip the demo EVSE online first so Evolt derives the picked status instead of UNKNOWN. Without
	// the simulator the event still fires, but the pushed status is whatever the DB already holds.
	simulated := false
	if h.charger != nil {
		if _, err := h.charger.SimulateOnline(ctx, req.Status); err != nil {
			return fail(c, http.StatusBadGateway, "charger simulate failed: "+err.Error(), nil)
		}
		simulated = true
	}

	if err := h.kafka.ProduceEvseStatus(ctx, req.Status); err != nil {
		return fail(c, http.StatusBadGateway, err.Error(), nil)
	}
	note := "consumer should PATCH the mock within ~5s — watch the request log"
	if !simulated {
		note += " · status will show UNKNOWN (charger simulator off: set EVOLT_DB_DSN)"
	}
	return ok(c, map[string]any{
		"produced":         true,
		"baseline_seeded":  seeded,
		"simulated_status": req.Status,
		"charger_online":   simulated,
		"note":             note,
	})
}

// ensureKafkaBaseline injects a minimal received location for the configured
// station if the replica has none — Evolt only PATCHes EVSE status, never PUTs
// the full location, so without a baseline every PATCH answers 2003.
func (h *Handlers) ensureKafkaBaseline(ctx context.Context) (bool, error) {
	raw, err := h.mock.Get(ctx, "/admin/received/locations")
	if err != nil {
		return false, err
	}
	// ReceivedRow serializes the location id as "key" (store/received.go).
	var received struct {
		Locations []struct {
			Key string `json:"key"`
		} `json:"locations"`
	}
	if err := json.Unmarshal(raw, &received); err == nil {
		for _, loc := range received.Locations {
			if loc.Key == h.cfg.KafkaStationID {
				return false, nil
			}
		}
	}
	baseline := map[string]any{
		"id":           h.cfg.KafkaStationID,
		"country_code": h.cfg.KafkaPartyCC,
		"party_id":     h.cfg.KafkaPartyID,
		"name":         "demo baseline",
		"evses": []map[string]any{{
			"uid":          h.cfg.KafkaOCPIEvseUID,
			"status":       "UNKNOWN",
			"last_updated": "2020-01-01T00:00:00Z",
		}},
		"last_updated": "2020-01-01T00:00:00Z",
	}
	if _, err := h.mock.Post(ctx, "/admin/received/locations", map[string]any{
		"country_code": h.cfg.KafkaPartyCC,
		"party_id":     h.cfg.KafkaPartyID,
		"payload":      baseline,
	}); err != nil {
		return false, err
	}
	return true, nil
}

// Unregister clears the partner's registration so a repeat demo starts from a
// clean handshake. Evolt keeps its own record — it implements no DELETE
// /credentials — so that side is cleared separately.
func (h *Handlers) Unregister(c echo.Context) error {
	result, err := h.mock.Delete(c.Request().Context(), "/admin/registrations")
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) Own(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	result, err := h.mock.Get(c.Request().Context(), "/admin/own/"+kind)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) Received(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	result, err := h.mock.Get(c.Request().Context(), "/admin/received/"+kind)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) Requests(c echo.Context) error {
	limit := 50
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return fail(c, http.StatusBadRequest, "limit must be 1..200", nil)
		}
		limit = n
	}
	result, err := h.mock.Get(c.Request().Context(), "/admin/requests?limit="+strconv.Itoa(limit))
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

func (h *Handlers) Reset(c echo.Context) error {
	result, err := h.mock.Post(c.Request().Context(), "/admin/seed/reset", nil)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}
