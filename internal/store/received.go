package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

type ObjectKey struct {
	CountryCode string
	PartyID     string
	LocationID  string
	EvseUID     string
	ConnectorID string
}

// Depth: 1 = location, 2 = evse, 3 = connector.
func (k ObjectKey) Depth() int {
	switch {
	case k.ConnectorID != "":
		return 3
	case k.EvseUID != "":
		return 2
	default:
		return 1
	}
}

func (s *Store) GetReceivedLocationObject(ctx context.Context, k ObjectKey) (json.RawMessage, error) {
	var (
		p   json.RawMessage
		err error
	)
	switch k.Depth() {
	case 1:
		err = s.pool.QueryRow(ctx, `SELECT payload FROM received_locations
			WHERE country_code=$1 AND party_id=$2 AND location_id=$3`,
			k.CountryCode, k.PartyID, k.LocationID).Scan(&p)
	case 2:
		err = s.pool.QueryRow(ctx, `SELECT payload FROM received_evses
			WHERE country_code=$1 AND party_id=$2 AND location_id=$3 AND evse_uid=$4`,
			k.CountryCode, k.PartyID, k.LocationID, k.EvseUID).Scan(&p)
	default:
		err = s.pool.QueryRow(ctx, `SELECT payload FROM received_connectors
			WHERE country_code=$1 AND party_id=$2 AND location_id=$3 AND evse_uid=$4 AND connector_id=$5`,
			k.CountryCode, k.PartyID, k.LocationID, k.EvseUID, k.ConnectorID).Scan(&p)
	}
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) PutReceivedLocationObject(ctx context.Context, k ObjectKey, payload json.RawMessage, lastUpdated time.Time) error {
	var err error
	switch k.Depth() {
	case 1:
		_, err = s.pool.Exec(ctx, `INSERT INTO received_locations (country_code, party_id, location_id, payload, last_updated)
			VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (country_code, party_id, location_id)
			DO UPDATE SET payload=EXCLUDED.payload, last_updated=EXCLUDED.last_updated`,
			k.CountryCode, k.PartyID, k.LocationID, payload, lastUpdated)
	case 2:
		status := extractString(payload, "status")
		_, err = s.pool.Exec(ctx, `INSERT INTO received_evses (country_code, party_id, location_id, evse_uid, status, payload, last_updated)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (country_code, party_id, location_id, evse_uid)
			DO UPDATE SET status=EXCLUDED.status, payload=EXCLUDED.payload, last_updated=EXCLUDED.last_updated`,
			k.CountryCode, k.PartyID, k.LocationID, k.EvseUID, status, payload, lastUpdated)
	default:
		_, err = s.pool.Exec(ctx, `INSERT INTO received_connectors (country_code, party_id, location_id, evse_uid, connector_id, payload, last_updated)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (country_code, party_id, location_id, evse_uid, connector_id)
			DO UPDATE SET payload=EXCLUDED.payload, last_updated=EXCLUDED.last_updated`,
			k.CountryCode, k.PartyID, k.LocationID, k.EvseUID, k.ConnectorID, payload, lastUpdated)
	}
	if err != nil {
		return fmt.Errorf("upsert received object depth %d: %w", k.Depth(), err)
	}
	if k.Depth() == 1 {
		if err := s.explodeLocation(ctx, k, payload, lastUpdated); err != nil {
			return err
		}
	}
	return s.bumpParents(ctx, k, lastUpdated)
}

// explodeLocation decomposes a full Location push into evse/connector rows —
// the same split Evolt's own receiver does. Without it a fresh replica would
// answer 2003 to every EVSE-status PATCH forever, since Evolt never sends
// depth-2 PUTs.
func (s *Store) explodeLocation(ctx context.Context, k ObjectKey, payload json.RawMessage, fallback time.Time) error {
	var loc struct {
		Evses []json.RawMessage `json:"evses"`
	}
	if err := json.Unmarshal(payload, &loc); err != nil {
		return nil
	}
	for _, evse := range loc.Evses {
		uid := extractID(evse, "uid")
		if uid == "" {
			continue
		}
		ek := ObjectKey{CountryCode: k.CountryCode, PartyID: k.PartyID, LocationID: k.LocationID, EvseUID: uid}
		if err := s.PutReceivedLocationObject(ctx, ek, evse, timestampOf(evse, fallback)); err != nil {
			return err
		}
		var ev struct {
			Connectors []json.RawMessage `json:"connectors"`
		}
		if json.Unmarshal(evse, &ev) != nil {
			continue
		}
		for _, conn := range ev.Connectors {
			id := extractID(conn, "id")
			if id == "" {
				continue
			}
			ck := ObjectKey{CountryCode: k.CountryCode, PartyID: k.PartyID, LocationID: k.LocationID, EvseUID: uid, ConnectorID: id}
			if err := s.PutReceivedLocationObject(ctx, ck, conn, timestampOf(conn, fallback)); err != nil {
				return err
			}
		}
	}
	return nil
}

func timestampOf(payload json.RawMessage, fallback time.Time) time.Time {
	if s := extractString(payload, "last_updated"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	return fallback
}

// bumpParents cascades last_updated up the tree (connector→evse→location) as
// the module guide requires, touching only rows that already exist.
func (s *Store) bumpParents(ctx context.Context, k ObjectKey, lastUpdated time.Time) error {
	if k.Depth() >= 3 {
		if _, err := s.pool.Exec(ctx, `UPDATE received_evses
			SET last_updated=$5, payload = jsonb_set(payload, '{last_updated}', to_jsonb($6::text))
			WHERE country_code=$1 AND party_id=$2 AND location_id=$3 AND evse_uid=$4 AND last_updated < $5`,
			k.CountryCode, k.PartyID, k.LocationID, k.EvseUID, lastUpdated, lastUpdated.UTC().Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
	}
	if k.Depth() >= 2 {
		if _, err := s.pool.Exec(ctx, `UPDATE received_locations
			SET last_updated=$4, payload = jsonb_set(payload, '{last_updated}', to_jsonb($5::text))
			WHERE country_code=$1 AND party_id=$2 AND location_id=$3 AND last_updated < $4`,
			k.CountryCode, k.PartyID, k.LocationID, lastUpdated, lastUpdated.UTC().Format("2006-01-02T15:04:05Z")); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetReceivedTariff(ctx context.Context, cc, pid, id string) (json.RawMessage, error) {
	var p json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT payload FROM received_tariffs
		WHERE country_code=$1 AND party_id=$2 AND tariff_id=$3`, cc, pid, id).Scan(&p)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) PutReceivedTariff(ctx context.Context, cc, pid, id string, payload json.RawMessage, lastUpdated time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO received_tariffs (country_code, party_id, tariff_id, payload, last_updated)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (country_code, party_id, tariff_id)
		DO UPDATE SET payload=EXCLUDED.payload, last_updated=EXCLUDED.last_updated`,
		cc, pid, id, payload, lastUpdated)
	return err
}

// DeleteReceivedTariff reports whether a row was actually removed — the
// handler answers 2003 (still HTTP 200) when nothing existed.
func (s *Store) DeleteReceivedTariff(ctx context.Context, cc, pid, id string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM received_tariffs
		WHERE country_code=$1 AND party_id=$2 AND tariff_id=$3`, cc, pid, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

type ReceivedRow struct {
	CountryCode string          `json:"country_code"`
	PartyID     string          `json:"party_id"`
	Key         string          `json:"key"`
	Status      string          `json:"status,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	LastUpdated time.Time       `json:"last_updated"`
}

func (s *Store) ListReceived(ctx context.Context, kind string) ([]ReceivedRow, error) {
	var q string
	switch kind {
	case "locations":
		q = `SELECT country_code, party_id, location_id, '' AS status, payload, last_updated FROM received_locations ORDER BY last_updated DESC`
	case "evses":
		q = `SELECT country_code, party_id, location_id || '/' || evse_uid, status, payload, last_updated FROM received_evses ORDER BY last_updated DESC`
	case "connectors":
		q = `SELECT country_code, party_id, location_id || '/' || evse_uid || '/' || connector_id, '' AS status, payload, last_updated FROM received_connectors ORDER BY last_updated DESC`
	case "tariffs":
		q = `SELECT country_code, party_id, tariff_id, '' AS status, payload, last_updated FROM received_tariffs ORDER BY last_updated DESC`
	default:
		return nil, fmt.Errorf("invalid kind %q", kind)
	}
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReceivedRow{}
	for rows.Next() {
		var r ReceivedRow
		if err := rows.Scan(&r.CountryCode, &r.PartyID, &r.Key, &r.Status, &r.Payload, &r.LastUpdated); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) CountReceived(ctx context.Context, table string) (int, error) {
	switch table {
	case "received_locations", "received_evses", "received_connectors", "received_tariffs":
	default:
		return 0, fmt.Errorf("invalid table %q", table)
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	return n, err
}

// extractID stringifies an identifier that may be a JSON string or number
// (connector ids appear both ways in the wild).
func extractID(payload json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func extractString(payload json.RawMessage, key string) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return ""
	}
	var v string
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}
