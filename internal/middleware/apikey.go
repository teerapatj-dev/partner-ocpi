package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/labstack/echo/v4"
)

// APIKeyAuth is constant-time and fail-closed: no configured key means every
// request is rejected (same rule as the workspace's internal endpoints).
func APIKeyAuth(key string) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			presented := c.Request().Header.Get("X-API-Key")
			if key == "" || presented == "" ||
				subtle.ConstantTimeCompare([]byte(key), []byte(presented)) != 1 {
				return echo.NewHTTPError(http.StatusUnauthorized, "invalid api key")
			}
			return next(c)
		}
	}
}
