// Package seed loads the per-partner demo dataset (locations + tariffs).
// Files are keyed by party_id; shapes follow what Evolt's receiver validates
// (coordinates as strings, publish always present, last_updated everywhere).
package seed

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"time"

	"mock-ocpi-partner/internal/store"
)

//go:embed data/*.json
var dataFS embed.FS

type dataset struct {
	Locations []json.RawMessage `json:"locations"`
	Tariffs   []json.RawMessage `json:"tariffs"`
}

func load(partyID string) (dataset, error) {
	var ds dataset
	raw, err := dataFS.ReadFile("data/" + partyID + ".json")
	if err != nil {
		return ds, fmt.Errorf("no seed dataset for party %q: %w", partyID, err)
	}
	if err := json.Unmarshal(raw, &ds); err != nil {
		return ds, fmt.Errorf("parse seed dataset %q: %w", partyID, err)
	}
	return ds, nil
}

func idAndLastUpdated(payload json.RawMessage) (string, time.Time, error) {
	var obj struct {
		ID          string `json:"id"`
		LastUpdated string `json:"last_updated"`
	}
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", time.Time{}, err
	}
	if obj.ID == "" {
		return "", time.Time{}, fmt.Errorf("seed object without id")
	}
	t, err := time.Parse(time.RFC3339, obj.LastUpdated)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("seed object %s: bad last_updated: %w", obj.ID, err)
	}
	return obj.ID, t, nil
}

// Apply upserts the dataset for partyID and returns how many objects landed.
func Apply(ctx context.Context, st *store.Store, partyID string) (int, error) {
	ds, err := load(partyID)
	if err != nil {
		return 0, err
	}
	n := 0
	for table, items := range map[string][]json.RawMessage{
		"own_locations": ds.Locations,
		"own_tariffs":   ds.Tariffs,
	} {
		for _, payload := range items {
			id, lastUpdated, err := idAndLastUpdated(payload)
			if err != nil {
				return n, fmt.Errorf("%s: %w", table, err)
			}
			if err := st.UpsertOwn(ctx, table, id, payload, lastUpdated); err != nil {
				return n, fmt.Errorf("upsert %s %s: %w", table, id, err)
			}
			n++
		}
	}
	return n, nil
}

// EnsureSeeded applies the dataset only when the database is still empty so a
// restart never clobbers data mutated during a demo.
func EnsureSeeded(ctx context.Context, st *store.Store, partyID string) (int, error) {
	n, err := st.CountOwn(ctx, "own_locations")
	if err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, nil
	}
	return Apply(ctx, st, partyID)
}
