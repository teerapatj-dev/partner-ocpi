package demo

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The browser must never be able to select a token/secret column. This guards the whitelist itself:
// add a sensitive column to any table's list and this fails.
func TestBrowsableTablesExcludeSecrets(t *testing.T) {
	forbidden := []string{"token", "secret", "password", "hash", "body_excerpt", "endpoints", "_enc"}
	cols := map[string][]string{"evses(scoped)": demoEvsesColumns}
	for table, spec := range browsableTables {
		cols[table] = spec.columns
	}
	for table, list := range cols {
		for _, col := range list {
			lc := strings.ToLower(col)
			for _, bad := range forbidden {
				if strings.Contains(lc, bad) {
					t.Errorf("%s.%s looks sensitive (matched %q) — must not be browsable", table, col, bad)
				}
			}
		}
	}
	// Belt-and-braces on the two tables that actually hold secrets.
	for _, col := range browsableTables["registrations"].columns {
		if col == "token_inbound" || col == "token_outbound" {
			t.Fatalf("registrations exposes %s", col)
		}
	}
	for _, col := range browsableTables["ocpi_credentials"].columns {
		if strings.HasPrefix(col, "token_") {
			t.Fatalf("ocpi_credentials exposes %s", col)
		}
	}
}

// The menu order lists must exactly cover the registry, with each entry on the right side.
func TestTableMenuOrderMatchesRegistry(t *testing.T) {
	seen := map[string]bool{}
	check := func(order []string, side string) {
		for _, tbl := range order {
			if tbl == "evses" { // scoped, added per-instance from config — not in the base map
				seen[tbl] = true
				continue
			}
			spec, ok := browsableTables[tbl]
			if !ok {
				t.Errorf("menu lists %q which is not in browsableTables", tbl)
				continue
			}
			if spec.side != side {
				t.Errorf("%q is on side %q but listed under %q", tbl, spec.side, side)
			}
			seen[tbl] = true
		}
	}
	check(evoltTableOrder, "evolt")
	check(partnerTableOrder, "partner")
	for tbl := range browsableTables {
		if !seen[tbl] {
			t.Errorf("%q is browsable but missing from the menu order", tbl)
		}
	}
}

// Without a configured DB the three DB-backed endpoints fail closed, not panic.
func TestDBEndpointsRequireDB(t *testing.T) {
	h := newHandlers(t, Config{}, nil, nil) // h.db == nil

	rec := doReq(t, h.Tables, http.MethodGet, "/api/demo/tables", "")
	if got := decode(t, rec); got.ok {
		t.Errorf("Tables should fail without a DB: %s", rec.Body.String())
	}
	rec = doReq(t, h.BrowseTable, http.MethodGet, "/api/demo/table/ocpi_credentials", "", "table", "ocpi_credentials")
	if got := decode(t, rec); got.ok {
		t.Errorf("BrowseTable should fail without a DB: %s", rec.Body.String())
	}
	rec = doReq(t, h.InitEvolt, http.MethodPost, "/api/demo/evolt/seed", "")
	if got := decode(t, rec); got.ok {
		t.Errorf("InitEvolt should fail without a DB: %s", rec.Body.String())
	}
}

// A cross-DB column has no SQL behind it on the side being queried, so filtering on it would either
// error at the database or silently search the wrong thing.
func TestCrossDBColumnIsNotSearchable(t *testing.T) {
	for name, spec := range browsableTables {
		if spec.join == nil {
			continue
		}
		if !contains(spec.columns, spec.join.column) || !contains(spec.columns, spec.join.key) {
			t.Errorf("%s: join column %q / key %q must both be listed columns", name, spec.join.column, spec.join.key)
		}
		if spec.join.side == spec.side {
			t.Errorf("%s: a cross-DB join to its own side (%s) should be a plain SQL join", name, spec.side)
		}
		if _, isExpr := spec.expr[spec.join.column]; isExpr {
			t.Errorf("%s: %q cannot be both a SQL expression and cross-DB filled", name, spec.join.column)
		}
	}
	b := &DBBrowser{tables: browsableTables}
	_, err := b.Query(context.Background(), "received_tariffs", "", "station_id", "x", 1)
	if err == nil || !strings.Contains(err.Error(), "tariff_id") {
		t.Fatalf("filtering a cross-DB column must be refused with a hint, got %v", err)
	}
}

// db selects a pool, never SQL — an unknown key must be refused before any query is built.
func TestUnknownPartnerDBIsRefused(t *testing.T) {
	b := &DBBrowser{tables: browsableTables}
	_, err := b.Query(context.Background(), "own_locations", "zzz", "", "", 1)
	if err == nil || !strings.Contains(err.Error(), "unknown partner db") {
		t.Fatalf("expected unknown partner db error, got %v", err)
	}
	if _, err := b.Query(context.Background(), "ocpi_credentials", "zzz", "", "", 1); err == nil ||
		strings.Contains(err.Error(), "unknown partner db") {
		t.Fatalf("evolt side must ignore db, got %v", err)
	}
}

func TestQuoteIdent(t *testing.T) {
	if got := quoteIdent("is_self"); got != `"is_self"` {
		t.Fatalf("quoteIdent: %s", got)
	}
	if got := quoteIdent(`a"b`); got != `"a""b"` {
		t.Fatalf("quoteIdent should double embedded quotes: %s", got)
	}
}
