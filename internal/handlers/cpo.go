package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 100
)

func parseListQuery(c echo.Context) (store.ListFilter, error) {
	f := store.ListFilter{Limit: defaultLimit}
	if v := c.QueryParam("date_from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("date_from is not a valid RFC3339 timestamp")
		}
		f.DateFrom = &t
	}
	if v := c.QueryParam("date_to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, fmt.Errorf("date_to is not a valid RFC3339 timestamp")
		}
		f.DateTo = &t
	}
	if v := c.QueryParam("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return f, fmt.Errorf("offset must be a non-negative integer")
		}
		f.Offset = n
	}
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return f, fmt.Errorf("limit must be an integer")
		}
		switch {
		case n < 1:
			f.Limit = defaultLimit
		case n > maxLimit:
			f.Limit = maxLimit
		default:
			f.Limit = n
		}
	}
	return f, nil
}

// setPaginationHeaders emits what Evolt's adapter actually parses:
// X-Total-Count, X-Limit and Link rel="next" only while more pages remain.
func (h *Handlers) setPaginationHeaders(c echo.Context, f store.ListFilter, total int) {
	header := c.Response().Header()
	header.Set("X-Total-Count", strconv.Itoa(total))
	header.Set("X-Limit", strconv.Itoa(f.Limit))
	next := f.Offset + f.Limit
	if next >= total {
		return
	}
	q := url.Values{}
	if f.DateFrom != nil {
		q.Set("date_from", f.DateFrom.UTC().Format(ocpi.TimeLayout))
	}
	if f.DateTo != nil {
		q.Set("date_to", f.DateTo.UTC().Format(ocpi.TimeLayout))
	}
	q.Set("offset", strconv.Itoa(next))
	q.Set("limit", strconv.Itoa(f.Limit))
	header.Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, h.cfg.BaseURL, c.Request().URL.Path, q.Encode()))
}

func (h *Handlers) listOwn(c echo.Context, table string) error {
	f, err := parseListQuery(c)
	if err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	items, total, listErr := h.st.ListOwn(c.Request().Context(), table, f)
	if listErr != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "list failed")
	}
	h.setPaginationHeaders(c, f, total)
	return ocpi.OK(c, items)
}

func (h *Handlers) ListLocations(c echo.Context) error { return h.listOwn(c, "own_locations") }
func (h *Handlers) ListTariffs(c echo.Context) error   { return h.listOwn(c, "own_tariffs") }

// GetLocationObject serves the sender GET-object at any depth:
// /{location_id}[/{evse_uid}[/{connector_id}]].
func (h *Handlers) GetLocationObject(c echo.Context) error {
	payload, err := h.st.GetOwn(c.Request().Context(), "own_locations", c.Param("location_id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown location")
		}
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "lookup failed")
	}
	evseUID := c.Param("evse_uid")
	if evseUID == "" {
		return ocpi.OK(c, payload)
	}
	evse, ok := findInArray(payload, "evses", "uid", evseUID)
	if !ok {
		return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown evse")
	}
	connectorID := c.Param("connector_id")
	if connectorID == "" {
		return ocpi.OK(c, evse)
	}
	connector, ok := findInArray(evse, "connectors", "id", connectorID)
	if !ok {
		return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown connector")
	}
	return ocpi.OK(c, connector)
}

// findInArray digs payload[arrayKey] for the element whose idKey stringifies
// to want — connector ids may legitimately be numbers or strings in JSON.
func findInArray(payload json.RawMessage, arrayKey, idKey, want string) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return nil, false
	}
	var items []json.RawMessage
	if raw, ok := obj[arrayKey]; !ok || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	for _, item := range items {
		var fields map[string]any
		if json.Unmarshal(item, &fields) != nil {
			continue
		}
		switch v := fields[idKey].(type) {
		case string:
			if v == want {
				return item, true
			}
		case float64:
			if strconv.FormatFloat(v, 'f', -1, 64) == want {
				return item, true
			}
		}
	}
	return nil, false
}
