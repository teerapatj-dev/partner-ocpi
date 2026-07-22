// Package ocpi holds the OCPI 2.2.1 wire types and response envelope.
//
// Contract locked against the real Evolt client (orch/adapter on origin/develop):
// status_code MUST be a JSON number and timestamp MUST be RFC3339 UTC ending in
// "Z" — Evolt decodes with `StatusCode int` and a string value fails the whole
// response.
package ocpi

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
)

const (
	StatusSuccess          = 1000
	StatusClientError      = 2000
	StatusInvalidParams    = 2001
	StatusUnknownResource  = 2003
	StatusServerError      = 3000
	StatusUnreachableParty = 3001
)

const TimeLayout = "2006-01-02T15:04:05Z"

func Now() string { return time.Now().UTC().Format(TimeLayout) }

type Envelope struct {
	Data          any    `json:"data,omitempty"`
	StatusCode    int    `json:"status_code"`
	StatusMessage string `json:"status_message,omitempty"`
	Timestamp     string `json:"timestamp"`
}

// ctx key used by the request-log middleware to record the OCPI code we answered.
const CtxStatusCode = "ocpi_status_code"

func respond(c echo.Context, httpStatus, code int, msg string, data any) error {
	c.Set(CtxStatusCode, code)
	return c.JSON(httpStatus, Envelope{Data: data, StatusCode: code, StatusMessage: msg, Timestamp: Now()})
}

func OK(c echo.Context, data any) error {
	return respond(c, http.StatusOK, StatusSuccess, "Success", data)
}

// Err answers HTTP 200 with the OCPI code in the body — the Evolt fanout treats
// "HTTP 2xx AND status_code" as the verdict (e.g. tariff DELETE needs 2xx+2003
// to converge), so functional errors must not surface as HTTP errors.
func Err(c echo.Context, code int, msg string) error {
	return respond(c, http.StatusOK, code, msg, nil)
}

// ErrHTTP is for transport-level failures where a real HTTP status is expected:
// 401 bad token, 400 unparseable JSON, 405 method/state not allowed.
func ErrHTTP(c echo.Context, httpStatus, code int, msg string) error {
	return respond(c, httpStatus, code, msg, nil)
}

type Version struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

type Endpoint struct {
	Identifier string `json:"identifier"`
	Role       string `json:"role"`
	URL        string `json:"url"`
}

type VersionDetails struct {
	Version   string     `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
}

type BusinessDetails struct {
	Name    string `json:"name"`
	Website string `json:"website,omitempty"`
	LogoURL string `json:"logo_url,omitempty"`
}

type Role struct {
	Role            string          `json:"role"`
	PartyID         string          `json:"party_id"`
	CountryCode     string          `json:"country_code"`
	BusinessDetails BusinessDetails `json:"business_details"`
}

type Credentials struct {
	Token string `json:"token"`
	URL   string `json:"url"`
	Roles []Role `json:"roles"`
}

// SelfRoles is the credentials roles array this partner presents — it acts as
// both CPO (sender of its own locations/tariffs) and eMSP (receiver of
// Evolt's pushes).
func SelfRoles(countryCode, partyID, name string) []Role {
	bd := BusinessDetails{Name: name}
	return []Role{
		{Role: "CPO", PartyID: partyID, CountryCode: countryCode, BusinessDetails: bd},
		{Role: "EMSP", PartyID: partyID, CountryCode: countryCode, BusinessDetails: bd},
	}
}
