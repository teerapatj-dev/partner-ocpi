package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	StatusPending    = "PENDING"
	StatusRegistered = "REGISTERED"
)

var ErrNotFound = errors.New("not found")

type Registration struct {
	ID            int64           `json:"id"`
	PartyName     string          `json:"party_name"`
	CountryCode   string          `json:"country_code"`
	PartyID       string          `json:"party_id"`
	TokenInbound  string          `json:"-"`
	TokenOutbound string          `json:"-"`
	Status        string          `json:"status"`
	VersionsURL   string          `json:"versions_url"`
	Endpoints     json.RawMessage `json:"endpoints,omitempty"`
	InitiatedBy   string          `json:"initiated_by"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

const regCols = `id, party_name, country_code, party_id, token_inbound, token_outbound,
	status, versions_url, COALESCE(endpoints, 'null'::jsonb), initiated_by, created_at, updated_at`

func scanReg(row interface{ Scan(...any) error }) (Registration, error) {
	var r Registration
	err := row.Scan(&r.ID, &r.PartyName, &r.CountryCode, &r.PartyID, &r.TokenInbound, &r.TokenOutbound,
		&r.Status, &r.VersionsURL, &r.Endpoints, &r.InitiatedBy, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s *Store) CreateRegistration(ctx context.Context, r Registration) (Registration, error) {
	row := s.pool.QueryRow(ctx, `INSERT INTO registrations
		(party_name, country_code, party_id, token_inbound, token_outbound, status, versions_url, endpoints, initiated_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+regCols,
		r.PartyName, r.CountryCode, r.PartyID, r.TokenInbound, r.TokenOutbound, r.Status, r.VersionsURL, r.Endpoints, r.InitiatedBy)
	return scanReg(row)
}

func (s *Store) UpdateRegistration(ctx context.Context, r Registration) error {
	_, err := s.pool.Exec(ctx, `UPDATE registrations SET
		party_name=$2, country_code=$3, party_id=$4, token_inbound=$5, token_outbound=$6,
		status=$7, versions_url=$8, endpoints=$9, updated_at=now() WHERE id=$1`,
		r.ID, r.PartyName, r.CountryCode, r.PartyID, r.TokenInbound, r.TokenOutbound,
		r.Status, r.VersionsURL, r.Endpoints)
	return err
}

func (s *Store) DeleteRegistration(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM registrations WHERE id=$1`, id)
	return err
}

func (s *Store) ListRegistrations(ctx context.Context) ([]Registration, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+regCols+` FROM registrations ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		r, err := scanReg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FindByInboundToken matches the presented raw token against every stored
// inbound token in constant time per candidate.
func (s *Store) FindByInboundToken(ctx context.Context, raw string) (Registration, error) {
	regs, err := s.ListRegistrations(ctx)
	if err != nil {
		return Registration{}, fmt.Errorf("list registrations: %w", err)
	}
	for _, r := range regs {
		if len(r.TokenInbound) > 0 &&
			subtle.ConstantTimeCompare([]byte(r.TokenInbound), []byte(raw)) == 1 {
			return r, nil
		}
	}
	return Registration{}, ErrNotFound
}

// FirstRegistered returns the counterparty used for outbound pushes; the mock
// realistically only ever registers with one Evolt environment at a time.
func (s *Store) FirstRegistered(ctx context.Context) (Registration, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+regCols+` FROM registrations WHERE status=$1 ORDER BY id LIMIT 1`, StatusRegistered)
	r, err := scanReg(row)
	if err != nil {
		if isNoRows(err) {
			return Registration{}, ErrNotFound
		}
		return Registration{}, err
	}
	return r, nil
}
