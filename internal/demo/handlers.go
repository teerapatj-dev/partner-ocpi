package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
)

var evseStatuses = map[string]bool{
	"AVAILABLE": true, "CHARGING": true, "BLOCKED": true,
	"INOPERATIVE": true, "OUTOFORDER": true,
	"FINISHING": true, "RESERVED": true, "PLANNED": true,
	"REMOVED": true, "UNKNOWN": true,
}

type Handlers struct {
	cfg     Config
	mock    *MockAdmin
	evolt   *Evolt
	kafka   *Kafka
	charger *ChargerDB   // nil when the charger simulator is not configured
	db      *DBBrowser   // nil when no DB is configured (table browser + evolt seed off)
	batch   *BatchRunner // nil when the batch binaries are not mounted (cron buttons off)
}

func NewHandlers(cfg Config, mock *MockAdmin, evolt *Evolt, kafka *Kafka, charger *ChargerDB, db *DBBrowser, batch *BatchRunner) *Handlers {
	return &Handlers{cfg: cfg, mock: mock, evolt: evolt, kafka: kafka, charger: charger, db: db, batch: batch}
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
		"batch_jobs":       h.batch.Jobs(),
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

// EvoltBatchRun starts one Roaming Out cron job — the real batch-ocpi-process binary a k8s CronJob
// runs on dev. A job that exits non-zero still answers 200 with its exit code and log: that outcome
// is what the demo exists to show. Only a job that could not be started at all is an error here.
func (h *Handlers) EvoltBatchRun(c echo.Context) error {
	var req struct {
		DryRun bool `json:"dry_run"`
		// Refresh re-publishes the partner's cron batch first. A cron only collects what changed
		// since its watermark, so without it every run after the first returns nothing — which reads
		// as a broken job rather than as the incremental sync it is.
		Refresh bool `json:"refresh"`
	}
	// The body is optional — an absent one means a real run.
	_ = c.Bind(&req)

	ctx := c.Request().Context()
	if req.Refresh {
		if _, err := h.mock.Post(ctx, "/admin/seed/new-batch", nil); err != nil {
			return failFrom(c, err)
		}
	}

	result, err := h.batch.Run(ctx, c.Param("job"), req.DryRun)
	if err != nil {
		if errors.Is(err, errBatchUnavailable) {
			return fail(c, http.StatusConflict, err.Error(), nil)
		}
		return fail(c, http.StatusBadRequest, err.Error(), nil)
	}
	return ok(c, result)
}

// EvoltTariffBackfill seeds evolt_ocpi_tariff + location_map_tariff_ocpi for every exposable station
// (core-ocpi-roaming /internal/tariffs/backfill). Roaming In tariff pull/push read from these tables,
// so this is the one-time seed to run before them.
func (h *Handlers) EvoltTariffBackfill(c echo.Context) error {
	result, err := h.evolt.RoamingTariffBackfill(c.Request().Context())
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
		EvseID string `json:"evse_id"` // optional: evses.id (uuid) of a demo-station EVSE; empty = the configured default
	}
	if err := c.Bind(&req); err != nil || !evseStatuses[req.Status] {
		return fail(c, http.StatusBadRequest, "status must be a valid EVSE status", nil)
	}
	ctx := c.Request().Context()
	seeded, err := h.ensureKafkaBaseline(ctx)
	if err != nil {
		return failFrom(c, err)
	}

	// Targeted path: flip and fire for a chosen demo-station EVSE, deriving its identifiers from the DB.
	if req.EvseID != "" {
		if h.db == nil {
			return fail(c, http.StatusBadRequest, "cannot target an EVSE without EVOLT_DB_DSN", nil)
		}
		target, err := h.resolveDemoEvse(ctx, req.EvseID)
		if err != nil {
			return fail(c, http.StatusBadRequest, err.Error(), nil)
		}
		if _, err := h.db.SimulateEvseOnline(ctx, target.ID, req.Status); err != nil {
			return fail(c, http.StatusBadGateway, "charger simulate failed: "+err.Error(), nil)
		}
		if err := h.kafka.ProduceEvseStatusFor(ctx, req.Status, h.cfg.KafkaStationID, target.UID, target.ID); err != nil {
			return fail(c, http.StatusBadGateway, err.Error(), nil)
		}
		return ok(c, map[string]any{
			"produced": true, "baseline_seeded": seeded, "simulated_status": req.Status,
			"charger_online": true, "evse": target.UID,
			"note": "consumer should PATCH the mock within ~5s — watch the request log",
		})
	}

	// Default path: the configured demo EVSE via ChargerDB. Flip online first so Evolt derives the
	// picked status instead of UNKNOWN; without the simulator the event still fires with the DB's value.
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

// resolveDemoEvse looks up a chosen EVSE id among the demo station's EVSEs, so the id can never point
// outside that station.
func (h *Handlers) resolveDemoEvse(ctx context.Context, id string) (DemoEvse, error) {
	evses, err := h.db.DemoStationEvses(ctx)
	if err != nil {
		return DemoEvse{}, err
	}
	for _, e := range evses {
		if e.ID == id {
			return e, nil
		}
	}
	return DemoEvse{}, fmt.Errorf("evse %s is not in the demo station", id)
}

// StationEvses lists the demo station's EVSEs so the UI can offer them as status-push targets.
func (h *Handlers) StationEvses(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "EVOLT_DB_DSN not configured — no station EVSEs", nil)
	}
	ctx := c.Request().Context()
	evses, err := h.db.DemoStationEvses(ctx)
	if err != nil {
		return fail(c, http.StatusBadGateway, err.Error(), nil)
	}
	// The picker offers what the partner actually holds, so what you see listed is what a PATCH can
	// land on. A partner that is unreachable just leaves the statuses blank rather than failing.
	if held, err := h.partnerEvseStatuses(ctx); err == nil {
		for i, e := range evses {
			if status, found := held[e.OCPIUID]; found {
				evses[i].PartnerStatus, evses[i].InPartner = status, true
			}
		}
	}
	return ok(c, evses)
}

// PartnerCache answers what Evolt currently holds for this partner — locations with their EVSE
// statuses, the eMSP-side counterpart of the panel the mock fills for Roaming In.
func (h *Handlers) PartnerCache(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "EVOLT_DB_DSN not configured", nil)
	}
	ctx := c.Request().Context()
	countryCode, partyID, err := h.partnerParty(ctx)
	if err != nil {
		return failFrom(c, err)
	}
	locations, err := h.db.PartnerCacheLocations(ctx, countryCode, partyID, 25)
	if err != nil {
		return fail(c, http.StatusBadGateway, err.Error(), nil)
	}
	return ok(c, map[string]any{"country_code": countryCode, "party_id": partyID, "locations": locations})
}

// partnerEvseStatuses maps the partner's received EVSE uid to the status it currently holds.
func (h *Handlers) partnerEvseStatuses(ctx context.Context) (map[string]string, error) {
	raw, err := h.mock.Get(ctx, "/admin/received/locations")
	if err != nil {
		return nil, err
	}
	// ReceivedRow serializes "<location>/<evse_uid>" as "key" (store/received.go).
	var received struct {
		Evses []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"evses"`
	}
	if err := json.Unmarshal(raw, &received); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(received.Evses))
	for _, e := range received.Evses {
		if i := strings.LastIndex(e.Key, "/"); i >= 0 {
			out[e.Key[i+1:]] = e.Status
		}
	}
	return out, nil
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
	// Seed every demo-station EVSE into the baseline so a PATCH to any chosen head lands (no 2003).
	// Falls back to the single configured EVSE when the DB is unavailable.
	var evses []map[string]any
	if h.db != nil {
		if list, err := h.db.DemoStationEvses(ctx); err == nil {
			for _, e := range list {
				evses = append(evses, map[string]any{"uid": e.OCPIUID, "status": "UNKNOWN", "last_updated": "2020-01-01T00:00:00Z"})
			}
		}
	}
	if len(evses) == 0 {
		evses = []map[string]any{{"uid": h.cfg.KafkaOCPIEvseUID, "status": "UNKNOWN", "last_updated": "2020-01-01T00:00:00Z"}}
	}
	baseline := map[string]any{
		"id":           h.cfg.KafkaStationID,
		"country_code": h.cfg.KafkaPartyCC,
		"party_id":     h.cfg.KafkaPartyID,
		"name":         "demo baseline",
		"evses":        evses,
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
	ctx := c.Request().Context()
	deleted, err := h.mock.Delete(ctx, "/admin/registrations")
	if err != nil {
		return failFrom(c, err)
	}
	// Also wipe the partner's roaming data (received_* + log) and re-seed its own catalog, so the next
	// demo run starts from a clean slate rather than the previous run's leftovers.
	reset, err := h.mock.Post(ctx, "/admin/seed/reset", nil)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, map[string]any{
		"registration": json.RawMessage(deleted),
		"data_reset":   json.RawMessage(reset),
	})
}

// ClearRequests empties just the partner's request log (own/received data untouched).
func (h *Handlers) ClearRequests(c echo.Context) error {
	result, err := h.mock.Delete(c.Request().Context(), "/admin/requests")
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
	// The push demo drives the hand-picked objects only; the cron batch would just crowd its pickers.
	path := "/admin/own/" + kind
	if source := c.QueryParam("source"); source == "base" || source == "cron" {
		path += "?source=" + source
	}
	result, err := h.mock.Get(c.Request().Context(), path)
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

// InitEvolt seeds Evolt's own OCPI identity (is_self row + roles + endpoints) into aurora_dev. It is
// the UI's one-click restore for the self-registration a partner-clear can wipe; idempotent, so a
// second press just reports zeros.
func (h *Handlers) InitEvolt(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "EVOLT_DB_DSN not configured — cannot seed Evolt identity", nil)
	}
	inserted, err := h.db.SeedEvoltSelf(c.Request().Context())
	if err != nil {
		return fail(c, http.StatusBadGateway, err.Error(), nil)
	}
	return ok(c, map[string]any{"inserted": inserted})
}

// Tables lists the browsable tables per side so the UI can build its Tables menu.
func (h *Handlers) Tables(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "no database configured — table browser unavailable", nil)
	}
	return ok(c, h.db.Menu())
}

// BrowseTable returns one page of a whitelisted table with an optional per-column filter.
func (h *Handlers) BrowseTable(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "no database configured — table browser unavailable", nil)
	}
	page := 1
	if v := c.QueryParam("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fail(c, http.StatusBadRequest, "page must be >= 1", nil)
		}
		page = n
	}
	res, err := h.db.Query(c.Request().Context(), c.Param("table"), c.QueryParam("col"), c.QueryParam("q"), page)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error(), nil)
	}
	return ok(c, res)
}
