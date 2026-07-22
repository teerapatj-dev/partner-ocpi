package handlers

import (
	"testing"

	"mock-ocpi-partner/internal/store"
)

func TestLastUpdatedOf(t *testing.T) {
	cases := []struct {
		name     string
		obj      map[string]any
		required bool
		wantErr  bool
	}{
		{"present valid", map[string]any{"last_updated": "2026-07-01T00:00:00Z"}, true, false},
		{"absent but optional (PUT)", map[string]any{}, false, false},
		{"absent and required (PATCH)", map[string]any{}, true, true},
		{"wrong type", map[string]any{"last_updated": 123}, true, true},
		{"garbage string", map[string]any{"last_updated": "yesterday"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := lastUpdatedOf(tc.obj, tc.required)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckOwnership(t *testing.T) {
	k := store.ObjectKey{CountryCode: "TH", PartyID: "EVO", LocationID: "LOC-1"}
	cases := []struct {
		name    string
		obj     map[string]any
		wantErr bool
	}{
		{"matching", map[string]any{"country_code": "TH", "party_id": "EVO", "id": "LOC-1"}, false},
		{"case-insensitive match", map[string]any{"country_code": "th", "party_id": "evo", "id": "loc-1"}, false},
		{"fields absent (evse-level body)", map[string]any{"status": "CHARGING"}, false},
		{"country mismatch", map[string]any{"country_code": "SG"}, true},
		{"party mismatch", map[string]any{"party_id": "XXX"}, true},
		{"id mismatch", map[string]any{"id": "LOC-9"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOwnership(tc.obj, k, "id", k.LocationID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
