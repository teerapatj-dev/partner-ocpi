package handlers

import (
	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
)

func (h *Handlers) Versions(c echo.Context) error {
	return ocpi.OK(c, []ocpi.Version{{Version: "2.2.1", URL: h.cfg.BaseURL + "/ocpi/2.2.1"}})
}

// endpointCatalog is what this partner advertises during handshake. Evolt's
// Roaming-Out flow requires at least one endpoint including "credentials".
func (h *Handlers) endpointCatalog() []ocpi.Endpoint {
	base := h.cfg.BaseURL
	return []ocpi.Endpoint{
		{Identifier: "credentials", Role: "SENDER", URL: base + "/ocpi/2.2.1/credentials"},
		{Identifier: "locations", Role: "SENDER", URL: base + "/ocpi/cpo/2.2.1/locations"},
		{Identifier: "locations", Role: "RECEIVER", URL: base + "/ocpi/emsp/2.2.1/locations"},
		{Identifier: "tariffs", Role: "SENDER", URL: base + "/ocpi/cpo/2.2.1/tariffs"},
		{Identifier: "tariffs", Role: "RECEIVER", URL: base + "/ocpi/emsp/2.2.1/tariffs"},
		{Identifier: "sessions", Role: "SENDER", URL: base + "/ocpi/cpo/2.2.1/sessions"},
		{Identifier: "sessions", Role: "RECEIVER", URL: base + "/ocpi/emsp/2.2.1/sessions"},
		{Identifier: "cdrs", Role: "SENDER", URL: base + "/ocpi/cpo/2.2.1/cdrs"},
		{Identifier: "cdrs", Role: "RECEIVER", URL: base + "/ocpi/emsp/2.2.1/cdrs"},
		{Identifier: "commands", Role: "RECEIVER", URL: base + "/ocpi/cpo/2.2.1/commands"},
	}
}

func (h *Handlers) VersionDetails(c echo.Context) error {
	return ocpi.OK(c, ocpi.VersionDetails{Version: "2.2.1", Endpoints: h.endpointCatalog()})
}
