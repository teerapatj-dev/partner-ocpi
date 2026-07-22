package handlers

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/client"
	"mock-ocpi-partner/internal/config"
	"mock-ocpi-partner/internal/store"
)

func contextWithTimeout(c echo.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Request().Context(), d)
}

type Handlers struct {
	cfg config.Config
	st  *store.Store
	cl  *client.Client
}

func New(cfg config.Config, st *store.Store, cl *client.Client) *Handlers {
	return &Handlers{cfg: cfg, st: st, cl: cl}
}
