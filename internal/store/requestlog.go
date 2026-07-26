package store

import (
	"context"
	"time"
)

type LogEntry struct {
	ID             int64     `json:"id"`
	TS             time.Time `json:"ts"`
	Direction      string    `json:"direction"`
	Method         string    `json:"method"`
	Path           string    `json:"path"`
	HTTPStatus     int       `json:"http_status"`
	OCPIStatusCode int       `json:"ocpi_status_code"`
	Counterparty   string    `json:"counterparty"`
	BodyExcerpt    string    `json:"body_excerpt"`
	AuthPresent    bool      `json:"auth_present"`
}

// ClearLog empties the request log without touching own/received data or registrations.
func (s *Store) ClearLog(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE request_log`)
	return err
}

func (s *Store) AppendLog(ctx context.Context, e LogEntry) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO request_log
		(direction, method, path, http_status, ocpi_status_code, counterparty, body_excerpt, auth_present)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.Direction, e.Method, e.Path, e.HTTPStatus, e.OCPIStatusCode, e.Counterparty, e.BodyExcerpt, e.AuthPresent)
	return err
}

func (s *Store) ListLog(ctx context.Context, limit int) ([]LogEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT id, ts, direction, method, path, http_status,
		ocpi_status_code, counterparty, body_excerpt, auth_present
		FROM request_log ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LogEntry{}
	for rows.Next() {
		var e LogEntry
		if err := rows.Scan(&e.ID, &e.TS, &e.Direction, &e.Method, &e.Path, &e.HTTPStatus,
			&e.OCPIStatusCode, &e.Counterparty, &e.BodyExcerpt, &e.AuthPresent); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) CountLog(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM request_log`).Scan(&n)
	return n, err
}
