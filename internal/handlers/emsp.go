package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"mock-ocpi-partner/internal/ocpi"
	"mock-ocpi-partner/internal/store"
)

func objectKey(c echo.Context) store.ObjectKey {
	return store.ObjectKey{
		CountryCode: strings.ToUpper(c.Param("country_code")),
		PartyID:     strings.ToUpper(c.Param("party_id")),
		LocationID:  c.Param("location_id"),
		EvseUID:     c.Param("evse_uid"),
		ConnectorID: c.Param("connector_id"),
	}
}

func readBodyObject(c echo.Context) (map[string]any, json.RawMessage, error) {
	raw, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, nil, fmt.Errorf("read body: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON object")
	}
	return obj, raw, nil
}

// lastUpdatedOf parses the object's last_updated; required tells apart PUT
// (default to now when absent) from PATCH (2001 when absent, per the module
// guide).
func lastUpdatedOf(obj map[string]any, required bool) (time.Time, error) {
	v, ok := obj["last_updated"]
	if !ok {
		if required {
			return time.Time{}, fmt.Errorf("last_updated is required")
		}
		return time.Now().UTC(), nil
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, fmt.Errorf("last_updated must be a string timestamp")
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("last_updated is not a valid RFC3339 timestamp")
	}
	return t, nil
}

// checkOwnership rejects a body whose identity fields contradict the URL —
// country_code/party_id in the push URL identify the object owner (the CPO).
func checkOwnership(obj map[string]any, k store.ObjectKey, bodyIDKey, urlID string) error {
	stringField := func(key string) (string, bool) {
		v, ok := obj[key].(string)
		return v, ok
	}
	if cc, ok := stringField("country_code"); ok && !strings.EqualFold(cc, k.CountryCode) {
		return fmt.Errorf("country_code in body does not match URL")
	}
	if pid, ok := stringField("party_id"); ok && !strings.EqualFold(pid, k.PartyID) {
		return fmt.Errorf("party_id in body does not match URL")
	}
	if id, ok := stringField(bodyIDKey); ok && !strings.EqualFold(id, urlID) {
		return fmt.Errorf("%s in body does not match URL", bodyIDKey)
	}
	return nil
}

func (h *Handlers) PutLocationObject(c echo.Context) error {
	k := objectKey(c)
	obj, raw, err := readBodyObject(c)
	if err != nil {
		return ocpi.ErrHTTP(c, http.StatusBadRequest, ocpi.StatusInvalidParams, err.Error())
	}
	idKey := map[int]string{1: "id", 2: "uid", 3: "id"}[k.Depth()]
	urlID := map[int]string{1: k.LocationID, 2: k.EvseUID, 3: k.ConnectorID}[k.Depth()]
	if err := checkOwnership(obj, k, idKey, urlID); err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	lastUpdated, err := lastUpdatedOf(obj, false)
	if err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	if err := h.st.PutReceivedLocationObject(c.Request().Context(), k, raw, lastUpdated); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "store object failed")
	}
	return ocpi.OK(c, nil)
}

// PatchLocationObject is the partial update — this is the call Evolt actually
// makes today (EVSE status: {"status","last_updated"}). Unknown object → 2003
// so the periodic pull can heal it, never a 500.
func (h *Handlers) PatchLocationObject(c echo.Context) error {
	k := objectKey(c)
	obj, _, err := readBodyObject(c)
	if err != nil {
		return ocpi.ErrHTTP(c, http.StatusBadRequest, ocpi.StatusInvalidParams, err.Error())
	}
	lastUpdated, err := lastUpdatedOf(obj, true)
	if err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	ctx := c.Request().Context()
	existing, err := h.st.GetReceivedLocationObject(ctx, k)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown object — full PUT or pull sync required first")
		}
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "lookup failed")
	}
	var merged map[string]any
	if err := json.Unmarshal(existing, &merged); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "stored object unreadable")
	}
	for key, v := range obj {
		merged[key] = v
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "merge failed")
	}
	if err := h.st.PutReceivedLocationObject(ctx, k, raw, lastUpdated); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "store object failed")
	}
	return ocpi.OK(c, nil)
}

func (h *Handlers) GetReceivedLocationObject(c echo.Context) error {
	payload, err := h.st.GetReceivedLocationObject(c.Request().Context(), objectKey(c))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown object")
		}
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "lookup failed")
	}
	return ocpi.OK(c, payload)
}

func (h *Handlers) PutReceivedTariff(c echo.Context) error {
	k := objectKey(c)
	obj, raw, err := readBodyObject(c)
	if err != nil {
		return ocpi.ErrHTTP(c, http.StatusBadRequest, ocpi.StatusInvalidParams, err.Error())
	}
	tariffID := c.Param("tariff_id")
	if err := checkOwnership(obj, k, "id", tariffID); err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	lastUpdated, err := lastUpdatedOf(obj, false)
	if err != nil {
		return ocpi.Err(c, ocpi.StatusInvalidParams, err.Error())
	}
	if err := h.st.PutReceivedTariff(c.Request().Context(), k.CountryCode, k.PartyID, tariffID, raw, lastUpdated); err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "store tariff failed")
	}
	return ocpi.OK(c, nil)
}

// DeleteReceivedTariff really deletes (tariffs differ from locations here);
// deleting the unknown answers 2003 with HTTP 200 — Evolt's fanout counts
// that as convergence.
func (h *Handlers) DeleteReceivedTariff(c echo.Context) error {
	k := objectKey(c)
	deleted, err := h.st.DeleteReceivedTariff(c.Request().Context(), k.CountryCode, k.PartyID, c.Param("tariff_id"))
	if err != nil {
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "delete tariff failed")
	}
	if !deleted {
		return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown tariff")
	}
	return ocpi.OK(c, nil)
}

// PatchTariffNotAllowed answers the spec-correct 405 — the tariffs receiver
// interface has no PATCH (unlike locations); without this route echo would 404.
func (h *Handlers) PatchTariffNotAllowed(c echo.Context) error {
	return ocpi.ErrHTTP(c, http.StatusMethodNotAllowed, ocpi.StatusClientError, "tariffs receiver has no PATCH — use PUT")
}

func (h *Handlers) GetReceivedTariff(c echo.Context) error {
	k := objectKey(c)
	payload, err := h.st.GetReceivedTariff(c.Request().Context(), k.CountryCode, k.PartyID, c.Param("tariff_id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ocpi.Err(c, ocpi.StatusUnknownResource, "unknown tariff")
		}
		return ocpi.ErrHTTP(c, http.StatusInternalServerError, ocpi.StatusServerError, "lookup failed")
	}
	return ocpi.OK(c, payload)
}
