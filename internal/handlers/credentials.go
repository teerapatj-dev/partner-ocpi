package handlers

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"

	"mock-ocpi-partner/internal/middleware"
	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

func (h *Handlers) selfCredentials(token string) ocpi.Credentials {
	return ocpi.Credentials{
		Token: token,
		URL:   h.cfg.BaseURL + "/ocpi/versions",
		Roles: ocpi.SelfRoles(h.cfg.CountryCode, h.cfg.PartyID, h.cfg.PartnerName),
	}
}

func validateCredentials(req ocpi.Credentials) error {
	if req.Token == "" {
		return fmt.Errorf("token is required")
	}
	if req.URL == "" {
		return fmt.Errorf("url is required")
	}
	if u, err := url.Parse(req.URL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("url is not a valid absolute URL")
	}
	if len(req.Roles) == 0 {
		return fmt.Errorf("roles must contain at least one role")
	}
	for i, r := range req.Roles {
		if len(r.PartyID) != 3 {
			return fmt.Errorf("roles[%d].party_id must be exactly 3 chars", i)
		}
		if len(r.CountryCode) != 2 {
			return fmt.Errorf("roles[%d].country_code must be exactly 2 chars", i)
		}
		if r.BusinessDetails.Name == "" {
			return fmt.Errorf("roles[%d].business_details.name is required", i)
		}
	}
	return nil
}

// PostCredentials handles the counterparty-initiated registration (Evolt →
// mock, authenticated with Token A). The callback to the initiator's versions
// URL is best-effort unless STRICT_CALLBACK is set — on dev that URL is
// cluster-internal and a cloud-hosted mock cannot reach it.
func (h *Handlers) PostCredentials(c echo.Context) error {
	reg, ok := middleware.RegistrationFrom(c)
	if !ok {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "registration missing from context")
	}
	if reg.Status == store.StatusRegistered {
		return ocpi.ErrHTTP(c, http.StatusMethodNotAllowed, ocpi.StatusClientError, "already registered — use PUT to update")
	}
	var req ocpi.Credentials
	if err := c.Bind(&req); err != nil {
		return ocpi.ErrHTTP(c, http.StatusBadRequest, ocpi.StatusInvalidParams, "invalid JSON body")
	}
	if err := validateCredentials(req); err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}

	ctx := c.Request().Context()
	endpoints, err := h.cl.FetchCounterparty(ctx, req.URL, req.Token)
	if err != nil {
		if h.cfg.StrictCallback {
			return ocpi.Err(c, ocpi.StatusUnreachableParty, "unable to reach initiator versions endpoint")
		}
		log.Warn().Err(err).Msg("credentials callback failed, continuing (STRICT_CALLBACK=false)")
	}

	tokenC := uuid.NewString()
	reg.TokenInbound = tokenC
	reg.TokenOutbound = req.Token
	reg.Status = store.StatusRegistered
	reg.VersionsURL = req.URL
	reg.Endpoints = endpoints
	reg.CountryCode = strings.ToUpper(req.Roles[0].CountryCode)
	reg.PartyID = strings.ToUpper(req.Roles[0].PartyID)
	reg.PartyName = req.Roles[0].BusinessDetails.Name
	reg.InitiatedBy = "REMOTE"
	if err := h.st.UpdateRegistration(ctx, reg); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "persist registration failed")
	}
	return ocpi.OK(c, h.selfCredentials(tokenC))
}

// registeredOnly guards GET/PUT/DELETE which are meaningless before the
// handshake completes — the spec answer is 405, not 401.
func registeredOnly(c echo.Context) (store.Registration, error) {
	reg, ok := middleware.RegistrationFrom(c)
	if !ok {
		return reg, ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "registration missing from context")
	}
	if reg.Status != store.StatusRegistered {
		return reg, ocpi.ErrHTTP(c, http.StatusMethodNotAllowed, ocpi.StatusClientError, "not registered yet")
	}
	return reg, nil
}

func (h *Handlers) GetCredentials(c echo.Context) error {
	reg, err := registeredOnly(c)
	if err != nil {
		return err
	}
	return ocpi.OK(c, h.selfCredentials(reg.TokenInbound))
}

// PutCredentials updates the registration and rotates our token, as the spec
// requires for credentials updates.
func (h *Handlers) PutCredentials(c echo.Context) error {
	reg, err := registeredOnly(c)
	if err != nil {
		return err
	}
	var req ocpi.Credentials
	if err := c.Bind(&req); err != nil {
		return ocpi.ErrHTTP(c, http.StatusBadRequest, ocpi.StatusInvalidParams, "invalid JSON body")
	}
	if err := validateCredentials(req); err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	tokenC := uuid.NewString()
	reg.TokenInbound = tokenC
	reg.TokenOutbound = req.Token
	reg.VersionsURL = req.URL
	reg.CountryCode = strings.ToUpper(req.Roles[0].CountryCode)
	reg.PartyID = strings.ToUpper(req.Roles[0].PartyID)
	reg.PartyName = req.Roles[0].BusinessDetails.Name
	if err := h.st.UpdateRegistration(c.Request().Context(), reg); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "persist registration failed")
	}
	return ocpi.OK(c, h.selfCredentials(tokenC))
}

func (h *Handlers) DeleteCredentials(c echo.Context) error {
	reg, err := registeredOnly(c)
	if err != nil {
		return err
	}
	if err := h.st.DeleteRegistration(c.Request().Context(), reg.ID); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "delete registration failed")
	}
	return ocpi.OK(c, nil)
}
