package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/seed"
	"mock-ocpi-partner/internal/store"
)

func adminErr(c echo.Context, status int, msg string) error {
	return c.JSON(status, map[string]string{"error": msg})
}

// AdminCreateToken issues a fresh Token A for a counterparty-initiated
// handshake (the mirror of Evolt's POST /ocpi/partner/initial). The raw token
// is returned exactly once.
func (h *Handlers) AdminCreateToken(c echo.Context) error {
	ctx := c.Request().Context()
	tokenA := uuid.NewString()
	regs, err := h.st.ListRegistrations(ctx)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list registrations failed")
	}
	if len(regs) > 0 {
		reg := regs[0]
		// Re-issuing Token A wipes an active registration (inbound + outbound
		// tokens both die) — require an explicit opt-in so a stray call can't
		// break a working push setup.
		if reg.Status == store.StatusRegistered && c.QueryParam("force") != "true" {
			return adminErr(c, http.StatusConflict,
				"already REGISTERED — issuing a new token A resets the registration; retry with ?force=true if intended")
		}
		reg.TokenInbound = tokenA
		reg.TokenOutbound = ""
		reg.Status = store.StatusPending
		reg.InitiatedBy = "REMOTE"
		if err := h.st.UpdateRegistration(ctx, reg); err != nil {
			return adminErr(c, http.StatusInternalServerError, "persist token failed")
		}
	} else if _, err := h.st.CreateRegistration(ctx, store.Registration{
		TokenInbound: tokenA,
		Status:       store.StatusPending,
		InitiatedBy:  "REMOTE",
	}); err != nil {
		return adminErr(c, http.StatusInternalServerError, "persist token failed")
	}
	return c.JSON(http.StatusOK, map[string]string{
		"token_a": tokenA,
		"note":    "hand this to Evolt (/ocpi/partner/outbound) — shown only once",
	})
}

type handshakeRequest struct {
	EvoltVersionsURL string `json:"evolt_versions_url"`
	TokenA           string `json:"token_a"`
}

func (h *Handlers) AdminHandshake(c echo.Context) error {
	var req handshakeRequest
	if err := c.Bind(&req); err != nil || req.EvoltVersionsURL == "" || req.TokenA == "" {
		return adminErr(c, http.StatusBadRequest, "evolt_versions_url and token_a are required")
	}
	result, err := h.cl.Handshake(c.Request().Context(), req.EvoltVersionsURL, req.TokenA)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "steps": result.Steps})
	}
	return c.JSON(http.StatusOK, result)
}

// AdminCredentialsUpdate drives the outbound PUT /credentials at the counterparty (token rotation).
func (h *Handlers) AdminCredentialsUpdate(c echo.Context) error {
	result, err := h.cl.UpdateCredentials(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "steps": result.Steps})
	}
	return c.JSON(http.StatusOK, result)
}

// AdminCredentialsDelete drives the outbound DELETE /credentials — the spec-true unregister.
func (h *Handlers) AdminCredentialsDelete(c echo.Context) error {
	result, err := h.cl.DeleteCredentials(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "steps": result.Steps})
	}
	return c.JSON(http.StatusOK, result)
}

type pushEvseStatusRequest struct {
	LocationID string `json:"location_id"`
	EvseUID    string `json:"evse_uid"`
	Status     string `json:"status"`
}

func (h *Handlers) AdminPushEvseStatus(c echo.Context) error {
	var req pushEvseStatusRequest
	if err := c.Bind(&req); err != nil || req.LocationID == "" || req.EvseUID == "" || req.Status == "" {
		return adminErr(c, http.StatusBadRequest, "location_id, evse_uid and status are required")
	}
	result, err := h.cl.PushEvseStatus(c.Request().Context(), req.LocationID, req.EvseUID, req.Status)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
	}
	return c.JSON(http.StatusOK, result)
}

type pushTariffRequest struct {
	TariffID string `json:"tariff_id"`
}

func (h *Handlers) AdminPushTariff(c echo.Context) error {
	var req pushTariffRequest
	if err := c.Bind(&req); err != nil || req.TariffID == "" {
		return adminErr(c, http.StatusBadRequest, "tariff_id is required")
	}
	ctx := c.Request().Context()
	payload, err := h.st.GetOwn(ctx, "own_tariffs", req.TariffID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return adminErr(c, http.StatusNotFound, "unknown tariff_id")
		}
		return adminErr(c, http.StatusInternalServerError, "tariff lookup failed")
	}
	result, err := h.cl.PushTariff(ctx, req.TariffID, payload)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
	}
	return c.JSON(http.StatusOK, result)
}

type pushLocationRequest struct {
	LocationID string `json:"location_id"`
}

func (h *Handlers) AdminPushLocation(c echo.Context) error {
	var req pushLocationRequest
	if err := c.Bind(&req); err != nil || req.LocationID == "" {
		return adminErr(c, http.StatusBadRequest, "location_id is required")
	}
	ctx := c.Request().Context()
	payload, err := h.st.GetOwn(ctx, "own_locations", req.LocationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return adminErr(c, http.StatusNotFound, "unknown location_id")
		}
		return adminErr(c, http.StatusInternalServerError, "location lookup failed")
	}
	result, err := h.cl.PushLocation(ctx, req.LocationID, payload)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
	}
	return c.JSON(http.StatusOK, result)
}

type pullRequest struct {
	Limit int `json:"limit"`
	// All walks every page (initial-sync style) instead of the first page only.
	All bool `json:"all"`
}

func (h *Handlers) AdminPullCounterparty(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return adminErr(c, http.StatusBadRequest, "kind must be locations or tariffs")
	}
	var req pullRequest
	if err := c.Bind(&req); err != nil {
		return adminErr(c, http.StatusBadRequest, "invalid body")
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if req.All {
		result, err := h.cl.PullAllFromCounterparty(c.Request().Context(), kind, limit, 25)
		if err != nil {
			return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
		}
		return c.JSON(http.StatusOK, result)
	}
	result, err := h.cl.PullFromCounterparty(c.Request().Context(), kind, limit)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
	}
	return c.JSON(http.StatusOK, result)
}

// AdminCurrentTokens returns the raw token pair. The demo backend needs the
// inbound token to stand in for Evolt's not-yet-built pull cron when calling
// the adapter; it stays inside the docker network, never in a browser.
func (h *Handlers) AdminCurrentTokens(c echo.Context) error {
	regs, err := h.st.ListRegistrations(c.Request().Context())
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list registrations failed")
	}
	if len(regs) == 0 {
		return adminErr(c, http.StatusNotFound, "no registration")
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status":         regs[0].Status,
		"token_inbound":  regs[0].TokenInbound,
		"token_outbound": regs[0].TokenOutbound,
	})
}

func (h *Handlers) AdminDeleteTariffPush(c echo.Context) error {
	result, err := h.cl.DeleteTariff(c.Request().Context(), c.Param("tariff_id"))
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{"error": err.Error(), "result": result})
	}
	return c.JSON(http.StatusOK, result)
}

func maskToken(t string) string {
	if t == "" {
		return ""
	}
	if len(t) <= 8 {
		return "********"
	}
	return t[:8] + "…"
}

func (h *Handlers) AdminRegistrations(c echo.Context) error {
	regs, err := h.st.ListRegistrations(c.Request().Context())
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list registrations failed")
	}
	type maskedReg struct {
		store.Registration
		TokenInboundMasked  string `json:"token_inbound_masked"`
		TokenOutboundMasked string `json:"token_outbound_masked"`
	}
	out := make([]maskedReg, 0, len(regs))
	for _, r := range regs {
		out = append(out, maskedReg{
			Registration:        r,
			TokenInboundMasked:  maskToken(r.TokenInbound),
			TokenOutboundMasked: maskToken(r.TokenOutbound),
		})
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handlers) AdminUnregister(c echo.Context) error {
	deleted, err := h.st.DeleteAllRegistrations(c.Request().Context())
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "delete registrations failed")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"deleted": deleted,
		"note":    "partner side only — the counterparty still holds its record",
	})
}

func (h *Handlers) AdminState(c echo.Context) error {
	ctx := c.Request().Context()
	counts := map[string]int{}
	for name, table := range map[string]string{
		"own_locations": "own_locations", "own_tariffs": "own_tariffs",
	} {
		n, err := h.st.CountOwn(ctx, table, "")
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "count failed")
		}
		counts[name] = n
	}
	for name, table := range map[string]string{
		"received_locations": "received_locations", "received_evses": "received_evses",
		"received_connectors": "received_connectors", "received_tariffs": "received_tariffs",
	} {
		n, err := h.st.CountReceived(ctx, table)
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "count failed")
		}
		counts[name] = n
	}
	if n, err := h.st.CountLog(ctx); err == nil {
		counts["request_log"] = n
	}
	regs, err := h.st.ListRegistrations(ctx)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list registrations failed")
	}
	regStatus := ""
	if len(regs) > 0 {
		regStatus = regs[0].Status
	}
	return c.JSON(http.StatusOK, map[string]any{
		"partner": map[string]string{
			"name":         h.cfg.PartnerName,
			"party_id":     h.cfg.PartyID,
			"country_code": h.cfg.CountryCode,
			"currency":     h.cfg.Currency,
			"base_url":     h.cfg.BaseURL,
		},
		"registration_status": regStatus,
		"counts":              counts,
		"time":                ocpi.Now(),
	})
}

func (h *Handlers) AdminOwn(c echo.Context) error {
	kind := c.Param("kind")
	if kind != "locations" && kind != "tariffs" {
		return adminErr(c, http.StatusBadRequest, "kind must be locations or tariffs")
	}
	source := c.QueryParam("source")
	if source != "" && source != seed.SourceBase && source != seed.SourceCron {
		return adminErr(c, http.StatusBadRequest, "source must be base or cron")
	}
	items, _, err := h.st.ListOwn(c.Request().Context(), "own_"+kind, store.ListFilter{Limit: 1000, Source: source})
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list failed")
	}
	return c.JSON(http.StatusOK, items)
}

// AdminNewBatch re-dates the cron batch to now. A pull cron only collects what changed since its
// watermark, so without this a second run has nothing to find and the demo looks broken.
func (h *Handlers) AdminNewBatch(c echo.Context) error {
	ctx := c.Request().Context()
	now := time.Now().UTC().Truncate(time.Second)
	locations, err := h.st.RestampSource(ctx, "own_locations", seed.SourceCron, now)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "restamp locations failed")
	}
	tariffs, err := h.st.RestampSource(ctx, "own_tariffs", seed.SourceCron, now)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "restamp tariffs failed")
	}
	return c.JSON(http.StatusOK, map[string]any{
		"locations":    locations,
		"tariffs":      tariffs,
		"last_updated": now.Format("2006-01-02T15:04:05Z"),
	})
}

func (h *Handlers) AdminReceived(c echo.Context) error {
	ctx := c.Request().Context()
	// A synced catalog holds the counterparty's whole estate; full payloads for every row can
	// exceed what callers will read. lean=1 strips payloads, limit=N keeps the newest N per list
	// (rows come back last_updated DESC) — 0 or absent returns everything, as before.
	lean := c.QueryParam("lean") == "1"
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	trim := func(rows []store.ReceivedRow) []store.ReceivedRow {
		if limit > 0 && len(rows) > limit {
			rows = rows[:limit]
		}
		if lean {
			out := make([]store.ReceivedRow, len(rows))
			for i, r := range rows {
				r.Payload = nil
				out[i] = r
			}
			return out
		}
		return rows
	}
	switch c.Param("kind") {
	case "locations":
		locations, err := h.st.ListReceived(ctx, "locations")
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "list failed")
		}
		evses, err := h.st.ListReceived(ctx, "evses")
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "list failed")
		}
		connectors, err := h.st.ListReceived(ctx, "connectors")
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "list failed")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"locations": trim(locations), "evses": trim(evses), "connectors": trim(connectors),
		})
	case "tariffs":
		tariffs, err := h.st.ListReceived(ctx, "tariffs")
		if err != nil {
			return adminErr(c, http.StatusInternalServerError, "list failed")
		}
		return c.JSON(http.StatusOK, map[string]any{"tariffs": trim(tariffs)})
	default:
		return adminErr(c, http.StatusBadRequest, "kind must be locations or tariffs")
	}
}

type injectLocationRequest struct {
	CountryCode string          `json:"country_code"`
	PartyID     string          `json:"party_id"`
	Payload     json.RawMessage `json:"payload"`
}

// AdminInjectLocation seeds the received replica manually — useful because
// Evolt today only PATCHes EVSE status and never PUTs the full Location, so
// without a baseline every PATCH would 2003 forever.
func (h *Handlers) AdminInjectLocation(c echo.Context) error {
	var req injectLocationRequest
	if err := c.Bind(&req); err != nil || len(req.Payload) == 0 || req.CountryCode == "" || req.PartyID == "" {
		return adminErr(c, http.StatusBadRequest, "country_code, party_id and payload are required")
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Payload, &obj); err != nil {
		return adminErr(c, http.StatusBadRequest, "payload must be a JSON object")
	}
	id, _ := obj["id"].(string)
	if id == "" {
		return adminErr(c, http.StatusBadRequest, "payload.id is required")
	}
	lastUpdated, err := lastUpdatedOf(obj, false)
	if err != nil {
		return adminErr(c, http.StatusBadRequest, err.Error())
	}
	k := store.ObjectKey{CountryCode: strings.ToUpper(req.CountryCode), PartyID: strings.ToUpper(req.PartyID), LocationID: id}
	if err := h.st.PutReceivedLocationObject(c.Request().Context(), k, req.Payload, lastUpdated); err != nil {
		return adminErr(c, http.StatusInternalServerError, "store failed")
	}
	return c.JSON(http.StatusOK, map[string]string{"stored": id})
}

func (h *Handlers) AdminRequests(c echo.Context) error {
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	entries, err := h.st.ListLog(c.Request().Context(), limit)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "list log failed")
	}
	return c.JSON(http.StatusOK, entries)
}

func (h *Handlers) AdminClearRequests(c echo.Context) error {
	ctx := c.Request().Context()
	if err := h.st.ClearLog(ctx); err != nil {
		return adminErr(c, http.StatusInternalServerError, "clear failed")
	}
	return c.JSON(http.StatusOK, map[string]any{"cleared": true})
}

func (h *Handlers) AdminSeedReset(c echo.Context) error {
	ctx := c.Request().Context()
	if err := h.st.ResetData(ctx); err != nil {
		return adminErr(c, http.StatusInternalServerError, "reset failed")
	}
	n, err := seed.Apply(ctx, h.st, h.cfg.PartyID)
	if err != nil {
		return adminErr(c, http.StatusInternalServerError, "reseed failed: "+err.Error())
	}
	return c.JSON(http.StatusOK, map[string]any{"reset": true, "seeded_objects": n})
}

func (h *Handlers) Health(c echo.Context) error {
	ctx, cancel := contextWithTimeout(c, 5*time.Second)
	defer cancel()
	if err := h.st.Ping(ctx); err != nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{"status": "db unreachable"})
	}
	return c.JSON(http.StatusOK, map[string]string{
		"status": "ok", "partner": h.cfg.PartyID, "time": ocpi.Now(),
	})
}
