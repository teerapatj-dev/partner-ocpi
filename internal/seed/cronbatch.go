package seed

import (
	"encoding/json"
	"fmt"
	"time"
)

// SourceBase marks the handful of objects the push demo drives by hand; SourceCron marks the batch
// the pull cron is meant to collect. Keeping them apart is the whole point: a cron run that returns
// the same objects the operator just pushed shows nothing about what the cron does.
const (
	SourceBase = "base"
	SourceCron = "cron"
)

// CronBatchSize is how many locations and how many tariffs the cron batch holds.
const CronBatchSize = 10

// cronBatchEpoch dates the generated objects. It sits before any demo run so the first pull, which
// carries no watermark, collects the whole batch; "new batch" then re-dates them to now.
var cronBatchEpoch = time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC)

type cronSite struct {
	name    string
	address string
	city    string
	postal  string
	lat     string
	lon     string
	parking string
}

// Real districts, so a demo audience reads the batch as a partner's estate rather than filler.
var cronSites = [CronBatchSize]cronSite{
	{"Ari", "88 Phahonyothin Soi 7, Phaya Thai", "Bangkok", "10400", "13.77970", "100.54430", "ON_STREET"},
	{"Ekkamai", "142 Sukhumvit 63, Watthana", "Bangkok", "10110", "13.71930", "100.58540", "PARKING_GARAGE"},
	{"Ladprao", "1693 Phahonyothin Rd, Chatuchak", "Bangkok", "10900", "13.81640", "100.56010", "PARKING_LOT"},
	{"Rangsit", "94 Phahonyothin Rd, Khlong Luang", "Pathum Thani", "12120", "14.03390", "100.61760", "PARKING_LOT"},
	{"Nonthaburi Civic", "550 Rattanathibet Rd, Bang Kraso", "Nonthaburi", "11000", "13.85960", "100.51470", "PARKING_GARAGE"},
	{"Pinklao", "7/129 Borommaratchachonnani Rd, Bang Bamru", "Bangkok", "10700", "13.77750", "100.47590", "PARKING_GARAGE"},
	{"Ramkhamhaeng", "3891 Ramkhamhaeng Rd, Suan Luang", "Bangkok", "10250", "13.75110", "100.64380", "PARKING_LOT"},
	{"Silom", "191 Silom Rd, Bang Rak", "Bangkok", "10500", "13.72760", "100.52380", "UNDERGROUND_GARAGE"},
	{"Bang Kapi", "3522 Lat Phrao Rd, Bang Kapi", "Bangkok", "10240", "13.76500", "100.64300", "PARKING_LOT"},
	{"Bang Na Km.7", "1091 Bangna-Trad Rd, Bang Na", "Bangkok", "10260", "13.66830", "100.64990", "PARKING_LOT"},
}

func cronLocationID(partyID string, i int) string {
	return fmt.Sprintf("%s-CRON-LOC-%02d", partyID, i+1)
}
func cronTariffID(partyID string, i int) string { return fmt.Sprintf("%s-CRON-TAR-%02d", partyID, i+1) }

// cronBatch builds the batch for a party. Objects are generated rather than checked in as JSON:
// twenty hand-written OCPI objects that differ only by name and price are noise to maintain, and the
// generator keeps every one of them valid against the same shape the file dataset uses.
func cronBatch(partyID, brand, countryCode string) (dataset, error) {
	var ds dataset
	for i := 0; i < CronBatchSize; i++ {
		site := cronSites[i]
		stamp := ocpiTime(cronBatchEpoch.Add(time.Duration(i) * 7 * time.Minute))
		tariffID := cronTariffID(partyID, i)

		// Every other site runs DC, so the batch exercises both connector shapes the receiver maps.
		dc := i%2 == 1
		connector := map[string]any{
			"id": "1", "standard": "IEC_62196_T2", "format": "SOCKET", "power_type": "AC_3_PHASE",
			"max_voltage": 400, "max_amperage": 32, "max_electric_power": 22000,
			"tariff_ids": []string{tariffID}, "last_updated": stamp,
		}
		if dc {
			connector = map[string]any{
				"id": "1", "standard": "IEC_62196_T2_COMBO", "format": "CABLE", "power_type": "DC",
				"max_voltage": 500, "max_amperage": 250, "max_electric_power": 120000,
				"tariff_ids": []string{tariffID}, "last_updated": stamp,
			}
		}

		loc, err := json.Marshal(map[string]any{
			"country_code": countryCode,
			"party_id":     partyID,
			"id":           cronLocationID(partyID, i),
			"publish":      true,
			"name":         fmt.Sprintf("%s %s", brand, site.name),
			"address":      site.address,
			"city":         site.city,
			"postal_code":  site.postal,
			"country":      "THA",
			"coordinates":  map[string]any{"latitude": site.lat, "longitude": site.lon},
			"parking_type": site.parking,
			"evses": []any{map[string]any{
				"uid":          fmt.Sprintf("%s-CRON-EVSE-%02d01", partyID, i+1),
				"status":       "AVAILABLE",
				"capabilities": []string{"REMOTE_START_STOP_CAPABLE", "RFID_READER"},
				"connectors":   []any{connector},
				"last_updated": stamp,
			}},
			"operator":      map[string]any{"name": brand},
			"time_zone":     "Asia/Bangkok",
			"opening_times": map[string]any{"twentyfourseven": true},
			"last_updated":  stamp,
		})
		if err != nil {
			return ds, err
		}
		ds.Locations = append(ds.Locations, loc)

		price := 6.0 + float64(i)*0.25
		kind, unit := "AC", "AC"
		if dc {
			price += 2.0
			kind, unit = "DC", "DC"
		}
		tar, err := json.Marshal(map[string]any{
			"country_code": countryCode,
			"party_id":     partyID,
			"id":           tariffID,
			"currency":     "THB",
			"type":         "REGULAR",
			"tariff_alt_text": []any{
				map[string]any{"language": "en", "text": fmt.Sprintf("%s charging %.2f THB/kWh at %s (excl. VAT)", unit, price, site.name)},
				map[string]any{"language": "th", "text": fmt.Sprintf("ชาร์จ %s %.2f บาท/หน่วย ที่ %s (ไม่รวม VAT)", kind, price, site.name)},
			},
			"elements": []any{map[string]any{
				"price_components": []any{
					map[string]any{"type": "ENERGY", "price": price, "vat": 7, "step_size": 1},
				},
			}},
			"last_updated": stamp,
		})
		if err != nil {
			return ds, err
		}
		ds.Tariffs = append(ds.Tariffs, tar)
	}
	return ds, nil
}

func ocpiTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }
