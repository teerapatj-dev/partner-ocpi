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

// Apply upserts both batches for partyID and returns how many objects landed.
func Apply(ctx context.Context, st *store.Store, partyID string) (int, error) {
	ds, err := load(partyID)
	if err != nil {
		return 0, err
	}
	n, err := applyDataset(ctx, st, ds, SourceBase)
	if err != nil {
		return n, err
	}

	brand, countryCode := identity(ds, partyID)
	cron, err := cronBatch(partyID, brand, countryCode)
	if err != nil {
		return n, fmt.Errorf("build cron batch: %w", err)
	}
	cronN, err := applyDataset(ctx, st, cron, SourceCron)
	return n + cronN, err
}

func applyDataset(ctx context.Context, st *store.Store, ds dataset, source string) (int, error) {
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
			if err := st.UpsertOwn(ctx, table, id, payload, lastUpdated, source); err != nil {
				return n, fmt.Errorf("upsert %s %s: %w", table, id, err)
			}
			n++
		}
	}
	return n, nil
}

// identity reads the brand and country the file dataset already publishes, so the generated batch
// belongs to the same partner instead of inventing a second one.
func identity(ds dataset, partyID string) (brand, countryCode string) {
	brand, countryCode = partyID, "TH"
	if len(ds.Locations) == 0 {
		return brand, countryCode
	}
	var first struct {
		CountryCode string `json:"country_code"`
		Operator    struct {
			Name string `json:"name"`
		} `json:"operator"`
	}
	if json.Unmarshal(ds.Locations[0], &first) != nil {
		return brand, countryCode
	}
	if first.Operator.Name != "" {
		brand = first.Operator.Name
	}
	if first.CountryCode != "" {
		countryCode = first.CountryCode
	}
	return brand, countryCode
}

// EnsureSeeded tops up whichever batch is missing and leaves the rest alone, so a restart never
// clobbers data mutated during a demo — and a database seeded before a batch existed still gets it.
func EnsureSeeded(ctx context.Context, st *store.Store, partyID string) (int, error) {
	ds, err := load(partyID)
	if err != nil {
		return 0, err
	}

	n := 0
	if missing, err := batchMissing(ctx, st, SourceBase); err != nil {
		return 0, err
	} else if missing {
		if n, err = applyDataset(ctx, st, ds, SourceBase); err != nil {
			return n, err
		}
	}

	missing, err := batchMissing(ctx, st, SourceCron)
	if err != nil || !missing {
		return n, err
	}
	brand, countryCode := identity(ds, partyID)
	cron, err := cronBatch(partyID, brand, countryCode)
	if err != nil {
		return n, fmt.Errorf("build cron batch: %w", err)
	}
	cronN, err := applyDataset(ctx, st, cron, SourceCron)
	return n + cronN, err
}

func batchMissing(ctx context.Context, st *store.Store, source string) (bool, error) {
	for _, table := range []string{"own_locations", "own_tariffs"} {
		n, err := st.CountOwn(ctx, table, source)
		if err != nil {
			return false, err
		}
		if n == 0 {
			return true, nil
		}
	}
	return false, nil
}
