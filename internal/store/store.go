package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS registrations (
		id BIGSERIAL PRIMARY KEY,
		party_name TEXT NOT NULL DEFAULT '',
		country_code TEXT NOT NULL DEFAULT '',
		party_id TEXT NOT NULL DEFAULT '',
		token_inbound TEXT NOT NULL,
		token_outbound TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'PENDING',
		versions_url TEXT NOT NULL DEFAULT '',
		endpoints JSONB,
		initiated_by TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE IF NOT EXISTS own_locations (
		id TEXT PRIMARY KEY,
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS own_tariffs (
		id TEXT PRIMARY KEY,
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS received_locations (
		country_code TEXT NOT NULL,
		party_id TEXT NOT NULL,
		location_id TEXT NOT NULL,
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (country_code, party_id, location_id)
	)`,
	`CREATE TABLE IF NOT EXISTS received_evses (
		country_code TEXT NOT NULL,
		party_id TEXT NOT NULL,
		location_id TEXT NOT NULL,
		evse_uid TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT '',
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (country_code, party_id, location_id, evse_uid)
	)`,
	`CREATE TABLE IF NOT EXISTS received_connectors (
		country_code TEXT NOT NULL,
		party_id TEXT NOT NULL,
		location_id TEXT NOT NULL,
		evse_uid TEXT NOT NULL,
		connector_id TEXT NOT NULL,
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (country_code, party_id, location_id, evse_uid, connector_id)
	)`,
	`CREATE TABLE IF NOT EXISTS received_tariffs (
		country_code TEXT NOT NULL,
		party_id TEXT NOT NULL,
		tariff_id TEXT NOT NULL,
		payload JSONB NOT NULL,
		last_updated TIMESTAMPTZ NOT NULL,
		PRIMARY KEY (country_code, party_id, tariff_id)
	)`,
	`CREATE TABLE IF NOT EXISTS request_log (
		id BIGSERIAL PRIMARY KEY,
		ts TIMESTAMPTZ NOT NULL DEFAULT now(),
		direction TEXT NOT NULL,
		method TEXT NOT NULL,
		path TEXT NOT NULL,
		http_status INT NOT NULL DEFAULT 0,
		ocpi_status_code INT NOT NULL DEFAULT 0,
		counterparty TEXT NOT NULL DEFAULT '',
		body_excerpt TEXT NOT NULL DEFAULT '',
		auth_present BOOL NOT NULL DEFAULT false
	)`,
}

// Open connects with retries so the app survives compose/Neon cold starts.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	var pool *pgxpool.Pool
	var err error
	deadline := time.Now().Add(60 * time.Second)
	for {
		pool, err = pgxpool.New(ctx, databaseURL)
		if err == nil {
			err = pool.Ping(ctx)
			if err == nil {
				break
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect database: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	s := &Store{pool: pool}
	for _, stmt := range schema {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			return nil, fmt.Errorf("bootstrap schema: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }
