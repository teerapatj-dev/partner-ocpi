package demo

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"
)

// The fanout partners (VoltCity/ChargeX) exist to be pushed at: once registered they stay in the
// background and every Evolt push shows up N times in the log. Only registration is driven from
// here — the interactive flows all stay on the main partner.

func (h *Handlers) fanoutByKey(key string) (fanoutPartner, bool) {
	for _, p := range h.fanout {
		if p.Key == key {
			return p, true
		}
	}
	return fanoutPartner{}, false
}

// FanoutState answers one row per fanout partner: who it is, what its mock thinks, and whether
// Evolt actually holds a registration for it — the two can disagree, and that disagreement is
// exactly what the badge should show.
func (h *Handlers) FanoutState(c echo.Context) error {
	ctx := c.Request().Context()
	out := []map[string]any{}
	for _, p := range h.fanout {
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

// FanoutHandshake registers one fanout partner the same way the main evolt-init flow does: that
// mock issues Token A, Evolt walks the handshake against its public path prefix.
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
	// Same contract as the main partner: the baseline location lands at registration time, so the
	// EVSE-status fanout answers 1000 here instead of 2003.
	if enabled, _ := h.kafka.Enabled(); enabled {
		if _, err := h.ensureKafkaBaselineFor(ctx, p.admin); err != nil {
			log.Warn().Err(err).Str("partner", p.Key).Msg("fanout baseline seed failed")
		}
	}
	return ok(c, json.RawMessage(result))
}

// FanoutUnregister mirrors the main Unregister: wipe the mock side, then clear Evolt's row for that
// party (no DELETE /credentials exists, and a leftover REGISTERED row blocks re-handshake with 9999).
func (h *Handlers) FanoutUnregister(c echo.Context) error {
	p, found := h.fanoutByKey(c.Param("partner"))
	if !found {
		return fail(c, http.StatusBadRequest, "unknown fanout partner", nil)
	}
	ctx := c.Request().Context()
	countryCode, partyID, partyErr := partnerPartyOf(ctx, p.admin)
	deleted, err := p.admin.Delete(ctx, "/admin/registrations")
	if err != nil {
		return failFrom(c, err)
	}
	reset, err := p.admin.Post(ctx, "/admin/seed/reset", nil)
	if err != nil {
		return failFrom(c, err)
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
	return ok(c, out)
}
