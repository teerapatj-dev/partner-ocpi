package seed

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// These checks mirror what Evolt actually validates on inbound partner data
// (orch handlers/models/partner_locations.go and partner_tariffs.go on
// origin/develop) — a seed object failing them would break Evolt's pull sync,
// so this test guards against seed rot.

type seedConnector struct {
	ID          *string  `json:"-"`
	RawID       any      `json:"id"`
	Standard    string   `json:"standard"`
	Format      string   `json:"format"`
	PowerType   string   `json:"power_type"`
	MaxVoltage  *int     `json:"max_voltage"`
	MaxAmperage *int     `json:"max_amperage"`
	TariffIDs   []string `json:"tariff_ids"`
	LastUpdated string   `json:"last_updated"`
}

type seedEvse struct {
	UID         string          `json:"uid"`
	Status      string          `json:"status"`
	Connectors  []seedConnector `json:"connectors"`
	LastUpdated string          `json:"last_updated"`
}

type seedLocation struct {
	CountryCode string         `json:"country_code"`
	PartyID     string         `json:"party_id"`
	ID          string         `json:"id"`
	Publish     *bool          `json:"publish"`
	Address     string         `json:"address"`
	City        string         `json:"city"`
	Country     string         `json:"country"`
	Coordinates map[string]any `json:"coordinates"`
	TimeZone    string         `json:"time_zone"`
	Evses       []seedEvse     `json:"evses"`
	LastUpdated string         `json:"last_updated"`
}

type seedElement struct {
	PriceComponents []map[string]any `json:"price_components"`
	Restrictions    map[string]any   `json:"restrictions"`
}

type seedTariff struct {
	CountryCode string        `json:"country_code"`
	PartyID     string        `json:"party_id"`
	ID          string        `json:"id"`
	Currency    string        `json:"currency"`
	Elements    []seedElement `json:"elements"`
	LastUpdated string        `json:"last_updated"`
}

func mustTime(t *testing.T, ctx, s string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, s); err != nil {
		t.Errorf("%s: bad last_updated %q", ctx, s)
	}
}

func validateLocation(t *testing.T, party string, raw json.RawMessage) map[string]bool {
	t.Helper()
	var loc seedLocation
	if err := json.Unmarshal(raw, &loc); err != nil {
		t.Fatalf("%s: location unmarshal: %v", party, err)
	}
	ctx := fmt.Sprintf("%s %s", party, loc.ID)
	if len(loc.CountryCode) != 2 || loc.PartyID != party {
		t.Errorf("%s: bad country_code/party_id", ctx)
	}
	if loc.Publish == nil {
		t.Errorf("%s: publish key missing — Evolt validates *bool required", ctx)
	}
	if len(loc.Country) != 3 {
		t.Errorf("%s: country must be ISO alpha-3, got %q", ctx, loc.Country)
	}
	if loc.TimeZone == "" || loc.Address == "" || loc.City == "" {
		t.Errorf("%s: address/city/time_zone required", ctx)
	}
	for _, axis := range []string{"latitude", "longitude"} {
		if _, ok := loc.Coordinates[axis].(string); !ok {
			t.Errorf("%s: coordinates.%s must be a STRING — Evolt's struct breaks on numbers", ctx, axis)
		}
	}
	mustTime(t, ctx, loc.LastUpdated)
	tariffRefs := map[string]bool{}
	for _, e := range loc.Evses {
		ectx := ctx + "/" + e.UID
		if e.UID == "" || e.Status == "" {
			t.Errorf("%s: evse uid/status required", ectx)
		}
		if len(e.Connectors) < 1 {
			t.Errorf("%s: evse must have >= 1 connector", ectx)
		}
		mustTime(t, ectx, e.LastUpdated)
		for _, cn := range e.Connectors {
			cctx := fmt.Sprintf("%s/conn %v", ectx, cn.RawID)
			if cn.RawID == nil || cn.Standard == "" || cn.Format == "" || cn.PowerType == "" {
				t.Errorf("%s: id/standard/format/power_type required", cctx)
			}
			if cn.MaxVoltage == nil || cn.MaxAmperage == nil {
				t.Errorf("%s: max_voltage/max_amperage required (*int on Evolt side)", cctx)
			}
			mustTime(t, cctx, cn.LastUpdated)
			for _, id := range cn.TariffIDs {
				tariffRefs[id] = true
			}
		}
	}
	return tariffRefs
}

func validateTariff(t *testing.T, party string, raw json.RawMessage) string {
	t.Helper()
	var tf seedTariff
	if err := json.Unmarshal(raw, &tf); err != nil {
		t.Fatalf("%s: tariff unmarshal: %v", party, err)
	}
	ctx := fmt.Sprintf("%s %s", party, tf.ID)
	if len(tf.Currency) != 3 {
		t.Errorf("%s: currency must be 3 chars", ctx)
	}
	if tf.PartyID != party {
		t.Errorf("%s: party_id mismatch", ctx)
	}
	if len(tf.Elements) < 1 {
		t.Errorf("%s: elements required (min 1)", ctx)
	}
	mustTime(t, ctx, tf.LastUpdated)
	for i, el := range tf.Elements {
		if len(el.PriceComponents) < 1 {
			t.Errorf("%s: elements[%d] needs >= 1 price component", ctx, i)
		}
		for _, pc := range el.PriceComponents {
			if _, ok := pc["price"].(float64); !ok {
				t.Errorf("%s: price must be a JSON number", ctx)
			}
			if _, ok := pc["step_size"].(float64); !ok {
				t.Errorf("%s: step_size required", ctx)
			}
		}
		// an element without restrictions is the fallback and must be last
		if el.Restrictions == nil && i != len(tf.Elements)-1 {
			t.Errorf("%s: unrestricted fallback element must be last (position %d)", ctx, i)
		}
	}
	return tf.ID
}

func TestSeedDatasetsMatchEvoltContract(t *testing.T) {
	for _, party := range []string{"PLG", "VCT", "CHX"} {
		t.Run(party, func(t *testing.T) {
			ds, err := load(party)
			if err != nil {
				t.Fatal(err)
			}
			if len(ds.Locations) < 3 || len(ds.Tariffs) < 3 {
				t.Fatalf("dataset too small: %d locations, %d tariffs", len(ds.Locations), len(ds.Tariffs))
			}
			wantTariffs := map[string]bool{}
			for _, l := range ds.Locations {
				for id := range validateLocation(t, party, l) {
					wantTariffs[id] = true
				}
			}
			haveTariffs := map[string]bool{}
			for _, tf := range ds.Tariffs {
				haveTariffs[validateTariff(t, party, tf)] = true
			}
			for id := range wantTariffs {
				if !haveTariffs[id] {
					t.Errorf("connector references tariff %q that is not in the dataset", id)
				}
			}
		})
	}
}
