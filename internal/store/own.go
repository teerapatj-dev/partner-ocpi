package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

type ListFilter struct {
	DateFrom *time.Time
	DateTo   *time.Time
	Offset   int
	Limit    int
}

// ListOwn pages over own_locations/own_tariffs ordered oldest-first, matching
// the pagination contract Evolt's adapter parses (X-Total-Count + Link next).
func (s *Store) ListOwn(ctx context.Context, table string, f ListFilter) (items []json.RawMessage, total int, err error) {
	if table != "own_locations" && table != "own_tariffs" {
		return nil, 0, fmt.Errorf("invalid table %q", table)
	}
	where := ` WHERE ($1::timestamptz IS NULL OR last_updated >= $1)
		AND ($2::timestamptz IS NULL OR last_updated < $2)`
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table+where, f.DateFrom, f.DateTo).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("count %s: %w", table, err)
	}
	rows, err := s.pool.Query(ctx, `SELECT payload FROM `+table+where+
		` ORDER BY last_updated ASC, id ASC OFFSET $3 LIMIT $4`, f.DateFrom, f.DateTo, f.Offset, f.Limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list %s: %w", table, err)
	}
	defer rows.Close()
	items = []json.RawMessage{}
	for rows.Next() {
		var p json.RawMessage
		if err := rows.Scan(&p); err != nil {
			return nil, 0, err
		}
		items = append(items, p)
	}
	return items, total, rows.Err()
}

func (s *Store) GetOwn(ctx context.Context, table, id string) (json.RawMessage, error) {
	if table != "own_locations" && table != "own_tariffs" {
		return nil, fmt.Errorf("invalid table %q", table)
	}
	var p json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT payload FROM `+table+` WHERE id=$1`, id).Scan(&p)
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	return p, err
}

func (s *Store) UpsertOwn(ctx context.Context, table, id string, payload json.RawMessage, lastUpdated time.Time) error {
	if table != "own_locations" && table != "own_tariffs" {
		return fmt.Errorf("invalid table %q", table)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO `+table+` (id, payload, last_updated) VALUES ($1,$2,$3)
		ON CONFLICT (id) DO UPDATE SET payload=EXCLUDED.payload, last_updated=EXCLUDED.last_updated`,
		id, payload, lastUpdated)
	return err
}

func (s *Store) CountOwn(ctx context.Context, table string) (int, error) {
	if table != "own_locations" && table != "own_tariffs" {
		return 0, fmt.Errorf("invalid table %q", table)
	}
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	return n, err
}

// ResetData wipes seeded and received data (keeps registrations so a demo
// reset does not force a new handshake).
func (s *Store) ResetData(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE own_locations, own_tariffs, received_locations,
		received_evses, received_connectors, received_tariffs, request_log`)
	return err
}
