package handlers

import (
	"strconv"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
)

// Stub endpoints exist so the endpoint catalog is honest and future phases
// (sessions/cdrs/commands) get a well-formed envelope instead of a 404.

func (h *Handlers) StubEmptyList(c echo.Context) error {
	c.Response().Header().Set("X-Total-Count", "0")
	c.Response().Header().Set("X-Limit", strconv.Itoa(defaultLimit))
	return ocpi.OK(c, []any{})
}

func (h *Handlers) StubAccept(c echo.Context) error {
	return ocpi.OK(c, nil)
}

func (h *Handlers) StubCommand(c echo.Context) error {
	return ocpi.OK(c, map[string]any{"result": "ACCEPTED", "timeout": 30})
}
