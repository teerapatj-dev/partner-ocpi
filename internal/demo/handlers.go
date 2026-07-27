package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
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

	// fanout holds the extra partner instances (VCT/CHX) in config order; empty = single-partner demo.
	fanout []fanoutPartner
}

type fanoutPartner struct {
	FanoutMock
	admin *MockAdmin
}

func NewHandlers(cfg Config, mock *MockAdmin, evolt *Evolt, kafka *Kafka, charger *ChargerDB, db *DBBrowser, batch *BatchRunner) *Handlers {
	h := &Handlers{cfg: cfg, mock: mock, evolt: evolt, kafka: kafka, charger: charger, db: db, batch: batch}
	for _, m := range cfg.FanoutMocks {
		h.fanout = append(h.fanout, fanoutPartner{FanoutMock: m, admin: NewMockAdminAt(m.URL, m.AdminKey, cfg)})
	}
	return h
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
	return partnerNameOf(ctx, h.mock)
}

func partnerNameOf(ctx context.Context, admin *MockAdmin) (string, error) {
	raw, err := admin.Get(ctx, "/admin/state")
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
	return partnerPartyOf(ctx, h.mock)
}

func partnerPartyOf(ctx context.Context, admin *MockAdmin) (string, string, error) {
	raw, err := admin.Get(ctx, "/admin/state")
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

// PartnerPull: the mock pulls a page of Evolt's CPO list with its own Token C.
func (h *Handlers) PartnerPull(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	var req struct {
		Limit *int `json:"limit"`
		// All = the roaming initial sync: walk the whole feed page by page instead of one page.
		All bool `json:"all"`
	}
	if err := c.Bind(&req); err != nil {
		return fail(c, http.StatusBadRequest, "invalid body", nil)
	}
	limit := 5
	if req.All {
		limit = 20 // page size for the full walk
	}
	if req.Limit != nil {
		if *req.Limit < 1 || *req.Limit > 20 {
			return fail(c, http.StatusBadRequest, "limit must be 1..20", nil)
		}
		limit = *req.Limit
	}

	// Every partner pulls, not just PLG — the point is one CPO feed serving several eMSPs at
	// once. Concurrent because the tariffs feed costs ~19s a page; three sequential pulls would
	// outlive the request timeout.
	ctx := c.Request().Context()
	type pullTarget struct {
		key   string
		admin *MockAdmin
	}
	targets := []pullTarget{{key: "PLG", admin: h.mock}}
	for _, p := range h.fanout {
		targets = append(targets, pullTarget{key: strings.ToUpper(p.Key), admin: p.admin})
	}
	type pullResult struct {
		Partner string          `json:"partner"`
		OK      bool            `json:"ok"`
		Error   string          `json:"error,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
	}
	results := make([]pullResult, len(targets))
	var wg sync.WaitGroup
	for i, tg := range targets {
		wg.Add(1)
		go func(i int, tg pullTarget) {
			defer wg.Done()
			raw, err := tg.admin.Post(ctx, "/admin/pull/"+kind, map[string]any{"limit": limit, "all": req.All})
			if err != nil {
				results[i] = pullResult{Partner: tg.key, Error: describeErr(err)}
				return
			}
			results[i] = pullResult{Partner: tg.key, OK: true, Result: json.RawMessage(raw)}
		}(i, tg)
	}
	wg.Wait()
	return ok(c, map[string]any{"results": results})
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
		// Fanout partners republish too, so the cron collects a fresh batch from every registered
		// sender — one being down only costs its own rows.
		for _, p := range h.fanout {
			if _, err := p.admin.Post(ctx, "/admin/seed/new-batch", nil); err != nil {
				log.Warn().Err(err).Str("partner", p.Key).Msg("fanout new-batch failed")
			}
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
	// A status event is an update to something the partner already holds — OCPI has no "PATCH
	// creates". The baseline location is PUT at handshake time; pushing to an EVSE the partner does
	// not hold is refused rather than silently seeded, so what the demo shows is the real contract.
	held, err := h.partnerEvseStatuses(ctx)
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
		if _, inPartner := held[target.OCPIUID]; !inPartner {
			return fail(c, http.StatusConflict,
				"ยังไม่มี partner ไหนถือตู้ "+target.OCPIUID+" — ทำ Roaming Location ก่อน (ปุ่ม ดึงทั้งหมด ในแท็บ Location)", nil)
		}
		if _, err := h.db.SimulateEvseOnline(ctx, target.ID, req.Status); err != nil {
			return fail(c, http.StatusBadGateway, "charger simulate failed: "+err.Error(), nil)
		}
		if err := h.kafka.ProduceEvseStatusFor(ctx, req.Status, h.cfg.KafkaStationID, target.UID, target.ID); err != nil {
			return fail(c, http.StatusBadGateway, err.Error(), nil)
		}
		return ok(c, map[string]any{
			"produced": true, "simulated_status": req.Status,
			"charger_online": true, "evse": target.UID,
			"note": "consumer should PATCH the mock within ~5s — watch the request log",
		})
	}

	// Default path: the configured demo EVSE via ChargerDB. Flip online first so Evolt derives the
	// picked status instead of UNKNOWN; without the simulator the event still fires with the DB's value.
	if _, inPartner := held[h.cfg.KafkaOCPIEvseUID]; !inPartner {
		return fail(c, http.StatusConflict,
			"ยังไม่มี partner ไหนถือตู้ "+h.cfg.KafkaOCPIEvseUID+" — ทำ Roaming Location ก่อน (ปุ่ม ดึงทั้งหมด ในแท็บ Location)", nil)
	}
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
// PartnerCache answers what Evolt's Roaming-Out cache holds, one section per partner party — the
// board is multi-partner, so "what Evolt stored" is only honest when every party shows.
func (h *Handlers) PartnerCache(c echo.Context) error {
	if h.db == nil {
		return fail(c, http.StatusBadRequest, "EVOLT_DB_DSN not configured", nil)
	}
	ctx := c.Request().Context()
	type target struct {
		key   string
		admin *MockAdmin
	}
	targets := []target{{key: "PLG", admin: h.mock}}
	for _, p := range h.fanout {
		targets = append(targets, target{key: strings.ToUpper(p.Key), admin: p.admin})
	}
	partners := make([]map[string]any, 0, len(targets))
	for _, tg := range targets {
		entry := map[string]any{"key": tg.key}
		countryCode, partyID, err := partnerPartyOf(ctx, tg.admin)
		if err != nil {
			entry["error"] = describeErr(err)
			partners = append(partners, entry)
			continue
		}
		if name, err := partnerNameOf(ctx, tg.admin); err == nil {
			entry["name"] = name
		}
		entry["country_code"], entry["party_id"] = countryCode, partyID
		locations, err := h.db.PartnerCacheLocations(ctx, countryCode, partyID, 25)
		if err != nil {
			entry["error"] = err.Error()
			partners = append(partners, entry)
			continue
		}
		tariffs, err := h.db.PartnerCacheTariffs(ctx, countryCode, partyID, 25)
		if err != nil {
			entry["error"] = err.Error()
			partners = append(partners, entry)
			continue
		}
		entry["locations"], entry["tariffs"] = locations, tariffs
		partners = append(partners, entry)
	}
	return ok(c, map[string]any{"partners": partners})
}

// partnerEvseStatuses maps EVSE uid → the status held across every partner (union — a status push
// lands wherever the EVSE is held, so "someone holds it" is the question; PLG's view wins when
// several do). lean=1: after a full sync the payloads of the whole estate blow past the admin
// client's 1 MiB cap, and only key+status matter here.
func (h *Handlers) partnerEvseStatuses(ctx context.Context) (map[string]string, error) {
	admins := []*MockAdmin{h.mock}
	for _, p := range h.fanout {
		admins = append(admins, p.admin)
	}
	out := map[string]string{}
	var lastErr error
	reached := 0
	for _, admin := range admins {
		raw, err := admin.Get(ctx, "/admin/received/locations?lean=1")
		if err != nil {
			lastErr = err
			continue
		}
		reached++
		// ReceivedRow serializes "<location>/<evse_uid>" as "key" (store/received.go).
		var received struct {
			Evses []struct {
				Key    string `json:"key"`
				Status string `json:"status"`
			} `json:"evses"`
		}
		if err := json.Unmarshal(raw, &received); err != nil {
			lastErr = err
			continue
		}
		for _, e := range received.Evses {
			if i := strings.LastIndex(e.Key, "/"); i >= 0 {
				if _, held := out[e.Key[i+1:]]; !held {
					out[e.Key[i+1:]] = e.Status
				}
			}
		}
	}
	if reached == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// Unregister clears the partner's registration so a repeat demo starts from a
// clean handshake. Evolt keeps its own record — it implements no DELETE
// /credentials — so that side is cleared separately.
func (h *Handlers) Unregister(c echo.Context) error {
	ctx := c.Request().Context()
	out, err := h.unregisterPartner(ctx, h.mock)
	if err != nil {
		return failFrom(c, err)
	}
	// The clean-slate button means the whole board: the fanout partners wipe too, each reported on
	// its own (one unreachable mock must not abort the rest of the sweep).
	if len(h.fanout) > 0 {
		fanout := map[string]any{}
		for _, p := range h.fanout {
			if res, err := h.unregisterPartner(ctx, p.admin); err != nil {
				fanout[p.Key] = map[string]any{"error": describeErr(err)}
			} else {
				fanout[p.Key] = res
			}
		}
		out["fanout"] = fanout
	}
	return ok(c, out)
}

// unregisterPartner wipes one partner's mock side (registration + roaming data, own catalog
// re-seeded) and clears Evolt's row for that party. Evolt offers no DELETE /credentials, so a
// leftover REGISTERED row would block every re-handshake with 9999 (is_self=false guard; cascade
// takes roles/endpoints/cache).
func (h *Handlers) unregisterPartner(ctx context.Context, admin *MockAdmin) (map[string]any, error) {
	countryCode, partyID, partyErr := partnerPartyOf(ctx, admin)
	deleted, err := admin.Delete(ctx, "/admin/registrations")
	if err != nil {
		return nil, err
	}
	reset, err := admin.Post(ctx, "/admin/seed/reset", nil)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"registration": json.RawMessage(deleted),
		"data_reset":   json.RawMessage(reset),
	}
	if h.db != nil && partyErr == nil {
		if n, err := h.db.DeletePartnerCredentials(ctx, countryCode, partyID); err != nil {
			out["evolt_side"] = "ลบทะเบียนฝั่ง Evolt ไม่สำเร็จ: " + err.Error()
		} else {
			out["evolt_credentials_deleted"] = n
		}
	}
	return out, nil
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

// Received aggregates what every partner holds from Evolt, one section per partner, so the panel
// shows the whole roaming estate — not just PLG's copy.
func (h *Handlers) Received(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return fail(c, http.StatusBadRequest, "kind must be locations or tariffs", nil)
	}
	ctx := c.Request().Context()
	type target struct {
		key   string
		admin *MockAdmin
	}
	targets := []target{{key: "PLG", admin: h.mock}}
	for _, p := range h.fanout {
		targets = append(targets, target{key: strings.ToUpper(p.Key), admin: p.admin})
	}
	partners := make([]map[string]any, 0, len(targets))
	for _, tg := range targets {
		entry := map[string]any{"key": tg.key}
		if name, err := partnerNameOf(ctx, tg.admin); err == nil {
			entry["name"] = name
		}
		// limit: the panel shows the newest slice; a fully synced catalog with payloads would
		// blow past the admin client's 1 MiB cap.
		result, err := tg.admin.Get(ctx, "/admin/received/"+kind+"?limit=30")
		if err != nil {
			entry["error"] = describeErr(err)
			partners = append(partners, entry)
			continue
		}
		// A received tariff is just an OCPI object — which station published it only Evolt
		// knows. Attach that origin so the panel can label each price instead of bare uuids.
		if kind == "tariffs" && h.db != nil {
			if enriched, ok2 := h.attachTariffOrigins(ctx, result); ok2 {
				entry["data"] = enriched
				partners = append(partners, entry)
				continue
			}
		}
		entry["data"] = json.RawMessage(result)
		partners = append(partners, entry)
	}
	return ok(c, map[string]any{"partners": partners})
}

func (h *Handlers) attachTariffOrigins(ctx context.Context, result json.RawMessage) (json.RawMessage, bool) {
	var keys struct {
		Tariffs []struct {
			Key string `json:"key"`
		} `json:"tariffs"`
	}
	var full map[string]json.RawMessage
	if json.Unmarshal(result, &keys) != nil || json.Unmarshal(result, &full) != nil || len(keys.Tariffs) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(keys.Tariffs))
	for _, t := range keys.Tariffs {
		ids = append(ids, t.Key)
	}
	origins, err := h.db.TariffOrigins(ctx, ids)
	if err != nil {
		return nil, false
	}
	enc, err := json.Marshal(origins)
	if err != nil {
		return nil, false
	}
	full["origins"] = enc
	merged, err := json.Marshal(full)
	if err != nil {
		return nil, false
	}
	return merged, true
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
	ctx := c.Request().Context()
	result, err := h.mock.Get(ctx, "/admin/requests?limit="+strconv.Itoa(limit))
	if err != nil {
		return failFrom(c, err)
	}
	rows := tagPartnerRows(result, "PLG")
	// Fanout partners contribute their logs too — that is where "one push, three receivers" becomes
	// visible. One of them being down must not empty the whole log.
	for _, p := range h.fanout {
		raw, err := p.admin.Get(ctx, "/admin/requests?limit="+strconv.Itoa(limit))
		if err != nil {
			continue
		}
		rows = append(rows, tagPartnerRows(raw, strings.ToUpper(p.Key))...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ts > rows[j].ts })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	merged := make([]json.RawMessage, len(rows))
	for i, r := range rows {
		merged[i] = r.raw
	}
	return ok(c, merged)
}

type taggedRow struct {
	ts  string
	raw json.RawMessage
}

func tagPartnerRows(result json.RawMessage, partner string) []taggedRow {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(result, &rows) != nil {
		return nil
	}
	out := make([]taggedRow, 0, len(rows))
	for _, r := range rows {
		enc, _ := json.Marshal(partner)
		r["partner"] = enc
		var ts string
		_ = json.Unmarshal(r["ts"], &ts)
		raw, err := json.Marshal(r)
		if err != nil {
			continue
		}
		out = append(out, taggedRow{ts: ts, raw: raw})
	}
	return out
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
	res, err := h.db.Query(c.Request().Context(), c.Param("table"), c.QueryParam("db"), c.QueryParam("col"), c.QueryParam("q"), page)
	if err != nil {
		return fail(c, http.StatusBadRequest, err.Error(), nil)
	}
	return ok(c, res)
}
