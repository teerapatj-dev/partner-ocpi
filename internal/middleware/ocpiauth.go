package middleware

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

const (
	CtxRegistration = "registration"
	CtxAuthPresent  = "auth_present"
)

// ScopeHandshake allows PENDING tokens (Token A) — only versions, version
// details and credentials may be called before registration completes.
type Scope int

const (
	ScopeHandshake Scope = iota
	ScopeRegistered
)

// OCPIAuth mirrors Evolt's inbound middleware: "Token <base64>" with a
// RawStdEncoding fallback, then the decoded value is compared (constant-time)
// against every token this partner has issued.
func OCPIAuth(st *store.Store, scope Scope) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			header := c.Request().Header.Get(echo.HeaderAuthorization)
			c.Set(CtxAuthPresent, header != "")
			raw, ok := decodeTokenHeader(header)
			if !ok {
				return ocpi.ErrHTTP(c, http.StatusUnauthorized, ocpi.StatusInvalidParams, "missing or malformed Authorization header")
			}
			reg, err := st.FindByInboundToken(c.Request().Context(), raw)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return ocpi.ErrHTTP(c, http.StatusUnauthorized, ocpi.StatusInvalidParams, "invalid token")
				}
				return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "token lookup failed")
			}
			if scope == ScopeRegistered && reg.Status != store.StatusRegistered {
				return ocpi.ErrHTTP(c, http.StatusUnauthorized, ocpi.StatusInvalidParams, "registration not completed")
			}
			c.Set(CtxRegistration, reg)
			return next(c)
		}
	}
}

func decodeTokenHeader(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Token") {
		return "", false
	}
	enc := strings.TrimSpace(parts[1])
	if enc == "" {
		return "", false
	}
	if b, err := base64.StdEncoding.DecodeString(enc); err == nil {
		return string(b), true
	}
	if b, err := base64.RawStdEncoding.DecodeString(enc); err == nil {
		return string(b), true
	}
	return "", false
}

func RegistrationFrom(c echo.Context) (store.Registration, bool) {
	reg, ok := c.Get(CtxRegistration).(store.Registration)
	return reg, ok
}
