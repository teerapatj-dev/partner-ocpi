package seed

import (
	"encoding/json"
	"strings"
	"testing"
)

// The generated batch goes to Evolt through the same pull path as the file dataset, so it has to
// clear the same contract — reusing the validators keeps a generator change from shipping objects
// the receiver would reject.
func TestCronBatchMatchesEvoltContract(t *testing.T) {
	for _, party := range []string{"PLG", "VCT", "CHX"} {
		t.Run(party, func(t *testing.T) {
			ds, err := load(party)
			if err != nil {
				t.Fatal(err)
			}
			brand, countryCode := identity(ds, party)
			batch, err := cronBatch(party, brand, countryCode)
			if err != nil {
				t.Fatal(err)
			}
			if len(batch.Locations) != CronBatchSize || len(batch.Tariffs) != CronBatchSize {
				t.Fatalf("batch = %d locations / %d tariffs, want %d each",
					len(batch.Locations), len(batch.Tariffs), CronBatchSize)
			}

			wantTariffs := map[string]bool{}
			for _, l := range batch.Locations {
				for id := range validateLocation(t, party, l) {
					wantTariffs[id] = true
				}
			}
			haveTariffs := map[string]bool{}
			for _, tf := range batch.Tariffs {
				haveTariffs[validateTariff(t, party, tf)] = true
			}
			for id := range wantTariffs {
				if !haveTariffs[id] {
					t.Errorf("connector references tariff %q that the batch does not publish", id)
				}
			}
		})
	}
}

// The batch only reads as a separate batch if its ids cannot collide with the hand-picked objects
// the push demo drives.
func TestCronBatchIDsDoNotCollideWithFileDataset(t *testing.T) {
	ds, err := load("PLG")
	if err != nil {
		t.Fatal(err)
	}
	brand, countryCode := identity(ds, "PLG")
	batch, err := cronBatch("PLG", brand, countryCode)
	if err != nil {
		t.Fatal(err)
	}

	base := map[string]bool{}
	for _, raw := range append(append([]json.RawMessage{}, ds.Locations...), ds.Tariffs...) {
		id, _, err := idAndLastUpdated(raw)
		if err != nil {
			t.Fatal(err)
		}
		base[id] = true
	}
	for _, raw := range append(append([]json.RawMessage{}, batch.Locations...), batch.Tariffs...) {
		id, _, err := idAndLastUpdated(raw)
		if err != nil {
			t.Fatal(err)
		}
		if base[id] {
			t.Errorf("cron object %q collides with the file dataset", id)
		}
		if !strings.Contains(id, "-CRON-") {
			t.Errorf("cron object %q is not recognisable as batch data", id)
		}
	}
}

// A partner that pulls has to see the batch as its own estate, not a second operator's.
func TestCronBatchInheritsPartnerIdentity(t *testing.T) {
	ds, err := load("VCT")
	if err != nil {
		t.Fatal(err)
	}
	brand, countryCode := identity(ds, "VCT")
	if brand == "VCT" || brand == "" {
		t.Fatalf("brand not taken from the dataset: %q", brand)
	}
	batch, err := cronBatch("VCT", brand, countryCode)
	if err != nil {
		t.Fatal(err)
	}
	var loc struct {
		CountryCode string `json:"country_code"`
		Name        string `json:"name"`
		Operator    struct {
			Name string `json:"name"`
		} `json:"operator"`
	}
	if err := json.Unmarshal(batch.Locations[0], &loc); err != nil {
		t.Fatal(err)
	}
	if loc.Operator.Name != brand || !strings.HasPrefix(loc.Name, brand) {
		t.Fatalf("operator = %q, name = %q, want brand %q", loc.Operator.Name, loc.Name, brand)
	}
	if loc.CountryCode != countryCode {
		t.Fatalf("country_code = %q, want %q", loc.CountryCode, countryCode)
	}
}
