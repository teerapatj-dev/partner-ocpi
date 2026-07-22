package middleware

import (
	"bytes"
	"io"

	"github.com/labstack/echo/v4"
	"github.com/rs/zerolog/log"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

const excerptCap = 2048

// RequestLog persists every inbound OCPI call for the admin/inspection API.
// The Authorization value is never stored — only whether one was present.
func RequestLog(st *store.Store) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			req := c.Request()
			var excerpt string
			if req.Body != nil {
				// cap matches the router's BodyLimit("2M") — a smaller cap here
				// would silently truncate the body handed to the handler
				body, err := io.ReadAll(io.LimitReader(req.Body, 2<<20))
				if err == nil {
					req.Body = io.NopCloser(bytes.NewReader(body))
					excerpt = ocpi.RedactTokens(string(body))
					if len(excerpt) > excerptCap {
						excerpt = excerpt[:excerptCap]
					}
				}
			}

			err := next(c)

			code, _ := c.Get(ocpi.CtxStatusCode).(int)
			counterparty := ""
			if reg, ok := RegistrationFrom(c); ok {
				counterparty = reg.CountryCode + "-" + reg.PartyID
				if counterparty == "-" {
					counterparty = reg.PartyName
				}
			}
			authPresent, _ := c.Get(CtxAuthPresent).(bool)
			entry := store.LogEntry{
				Direction:      "in",
				Method:         req.Method,
				Path:           req.URL.Path,
				HTTPStatus:     c.Response().Status,
				OCPIStatusCode: code,
				Counterparty:   counterparty,
				BodyExcerpt:    excerpt,
				AuthPresent:    authPresent,
			}
			if logErr := st.AppendLog(req.Context(), entry); logErr != nil {
				log.Warn().Err(logErr).Msg("append request log failed")
			}
			return err
		}
	}
}
