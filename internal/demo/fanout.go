package demo

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
)

// Every partner on the board is equal: PLG is simply the first row, with the same handshake,
// partner-init and unregister actions as the fanout ones. allPartners keeps that order stable.

func (h *Handlers) allPartners() []fanoutPartner {
	out := []fanoutPartner{{FanoutMock: FanoutMock{Key: "plg", PathPrefix: ""}, admin: h.mock}}
	return append(out, h.fanout...)
}

func (h *Handlers) fanoutByKey(key string) (fanoutPartner, bool) {
	for _, p := range h.allPartners() {
		if p.Key == key {
			return p, true
		}
	}
	return fanoutPartner{}, false
}

// FanoutState answers one row per partner: who it is, what its mock thinks, and whether Evolt
// actually holds a registration for it — the two can disagree, and that disagreement is exactly
// what the badge should show.
func (h *Handlers) FanoutState(c echo.Context) error {
	ctx := c.Request().Context()
	out := []map[string]any{}
	for _, p := range h.allPartners() {
		row := map[string]any{"key": p.Key}
		raw, err := p.admin.Get(ctx, "/admin/state")
		if err != nil {
			row["reachable"] = false
			out = append(out, row)
			continue
		}
		var state struct {
			Partner struct {
				Name        string `json:"name"`
				CountryCode string `json:"country_code"`
				PartyID     string `json:"party_id"`
			} `json:"partner"`
			RegistrationStatus string `json:"registration_status"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			row["reachable"] = false
			out = append(out, row)
			continue
		}
		row["reachable"] = true
		row["name"] = state.Partner.Name
		row["party"] = state.Partner.CountryCode + "/" + state.Partner.PartyID
		row["mock_status"] = state.RegistrationStatus
		if _, err := h.evolt.PartnerCredentialsID(ctx, state.Partner.CountryCode, state.Partner.PartyID); err == nil {
			row["evolt_registered"] = true
		} else {
			row["evolt_registered"] = false
			if !errors.Is(err, errNotRegistered) {
				row["evolt_note"] = describeErr(err)
			}
		}
		out = append(out, row)
	}
	return ok(c, out)
}

// FanoutHandshake registers one partner Evolt-initiated: that mock issues Token A, Evolt walks the
// handshake against the partner's public path prefix.
func (h *Handlers) FanoutHandshake(c echo.Context) error {
	p, found := h.fanoutByKey(c.Param("partner"))
	if !found {
		return fail(c, http.StatusBadRequest, "unknown fanout partner", nil)
	}
	ctx := c.Request().Context()
	if h.cfg.PublicBaseURL == "" {
		return fail(c, http.StatusBadGateway, "PUBLIC_BASE_URL not configured — Evolt cannot reach the mock without the tunnel", nil)
	}
	partnerName, err := partnerNameOf(ctx, p.admin)
	if err != nil {
		return failFrom(c, err)
	}
	tokenRes, err := p.admin.Post(ctx, "/admin/tokens?force=true", nil)
	if err != nil {
		return failFrom(c, err)
	}
	var token struct {
		TokenA string `json:"token_a"`
	}
	if err := json.Unmarshal(tokenRes, &token); err != nil || token.TokenA == "" {
		return fail(c, http.StatusBadGateway, "mock returned no token_a", nil)
	}
	result, err := h.evolt.PartnerOutbound(ctx, partnerName, token.TokenA,
		h.cfg.PublicBaseURL+p.PathPrefix+"/ocpi/versions")
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

// FanoutPartnerInit is the opposite direction for any partner: orch hands out Token A for it, then
// that mock walks versions → details → credentials against Evolt itself.
func (h *Handlers) FanoutPartnerInit(c echo.Context) error {
	p, found := h.fanoutByKey(c.Param("partner"))
	if !found {
		return fail(c, http.StatusBadRequest, "unknown fanout partner", nil)
	}
	ctx := c.Request().Context()
	if h.cfg.EvoltVersionsURL == "" {
		return fail(c, http.StatusBadGateway, errNotConfigured.Error(), nil)
	}
	partnerName, err := partnerNameOf(ctx, p.admin)
	if err != nil {
		return failFrom(c, err)
	}
	tokenA, err := h.evolt.PartnerInitial(ctx, partnerName)
	if err != nil {
		return failFrom(c, err)
	}
	result, err := p.admin.Post(ctx, "/admin/handshake", map[string]string{
		"evolt_versions_url": h.cfg.EvoltVersionsURL,
		"token_a":            tokenA,
	})
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, json.RawMessage(result))
}

// FanoutUnregister mirrors the main Unregister for a single partner: wipe the mock side, then clear
// Evolt's row for that party (a leftover REGISTERED row blocks re-handshake with 9999).
func (h *Handlers) FanoutUnregister(c echo.Context) error {
	p, found := h.fanoutByKey(c.Param("partner"))
	if !found {
		return fail(c, http.StatusBadRequest, "unknown fanout partner", nil)
	}
	out, err := h.unregisterPartner(c.Request().Context(), p.admin)
	if err != nil {
		return failFrom(c, err)
	}
	return ok(c, out)
}
