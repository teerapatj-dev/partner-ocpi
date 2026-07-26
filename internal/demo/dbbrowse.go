package demo

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DBBrowser is the read-only DBeaver-style table viewer behind the demo's Tables menu, plus the one
// write it is allowed to make: seeding Evolt's own OCPI identity. Two pools — Evolt's aurora_dev and
// the mock's own Postgres — each queried only through the whitelist below.
//
// Safety model: a table can be read only if it is in b.tables (built from browsableTables), and only
// the columns listed there are ever selected or filtered on. Token/secret columns
// (ocpi_credentials.token_*, registrations.token_*) and large jsonb blobs are deliberately absent from
// every column list, so the browser has no code path that can surface them. Identifiers come from this
// file (never the request); request/scope values are bound as parameters.
type DBBrowser struct {
	evolt         *pgxpool.Pool
	partner       *pgxpool.Pool
	demoStationID string
	tables        map[string]tableSpec // browsableTables + any demo-scoped entries
	scopeArgs     map[string][]any     // leading bound args a scoped table's WHERE fragment consumes
}

const browsePageSize = 10

type tableSpec struct {
	side    string
	columns []string          // display + searchable columns, in order — never a token/secret/blob column
	expr    map[string]string // optional SQL expression per column (e.g. jsonb extract); default = the column itself
	from    string            // optional FROM clause (e.g. a jsonb unnest); default = the table name. Constant.
	orderBy string            // constant, from this file only
	scope   string            // constant WHERE fragment always applied (uses $1.. bound from scopeArgs); "" = none
}

// exprOf returns the SQL that produces a column's value — a jsonb extract for received_* payload
// fields, otherwise the column itself. All expressions are constants from this file.
func (s tableSpec) exprOf(col string) string {
	if e, ok := s.expr[col]; ok {
		return e
	}
	return quoteIdent(col)
}

// fromClause returns what the query selects FROM — a virtual unnest for the derived tables, otherwise
// the plain table. Constant SQL from this file only.
func (s tableSpec) fromClause(table string) string {
	if s.from != "" {
		return s.from
	}
	return quoteIdent(table)
}

var browsableTables = map[string]tableSpec{
	// Evolt (aurora_dev) — is_self identity, the OCPI tariff seed, and the partner cache it holds
	"ocpi_credentials":         {side: "evolt", columns: []string{"id", "partner_name", "status", "initiated_by", "version", "is_self", "registered_at", "created_at"}, orderBy: "is_self DESC, created_at"},
	"ocpi_credentials_roles":   {side: "evolt", columns: []string{"id", "credentials_id", "role", "party_id", "country_code", "business_name", "created_at"}, orderBy: "created_at"},
	"ocpi_endpoints":           {side: "evolt", columns: []string{"id", "credentials_id", "identifier", "role", "url", "created_at"}, orderBy: "identifier, role"},
	"evolt_ocpi_tariff":        {side: "evolt", columns: []string{"id", "station_id", "scope", "last_updated", "updated_at"}, orderBy: "last_updated DESC"},
	"location_map_tariff_ocpi": {side: "evolt", columns: []string{"id", "station_id", "connector_id", "tariff_ocpi_id", "connector_type", "updated_at"}, orderBy: "station_id"},
	"ocpi_partner_locations":   {side: "evolt", columns: []string{"id", "credentials_id", "country_code", "party_id", "location_id", "name", "address", "city", "publish", "last_updated", "synced_at"}, orderBy: "last_updated DESC"},
	"ocpi_partner_evses":       {side: "evolt", columns: []string{"id", "location_id", "evse_uid", "evse_id", "status", "last_updated", "synced_at"}, orderBy: "last_updated DESC"},
	"ocpi_partner_connectors":  {side: "evolt", columns: []string{"id", "evse_id", "connector_id", "standard", "format", "power_type", "max_electric_power", "last_updated", "synced_at"}, orderBy: "connector_id"},
	"ocpi_partner_tariffs":     {side: "evolt", columns: []string{"id", "credentials_id", "country_code", "party_id", "tariff_id", "currency", "type", "min_price_incl_vat", "max_price_incl_vat", "last_updated", "synced_at", "deleted_at"}, orderBy: "last_updated DESC"},
	// Partner (mock DB) — what PlugSiam holds. registrations.token_* / request_log.body_excerpt omitted on purpose.
	"registrations": {side: "partner", columns: []string{"id", "party_name", "country_code", "party_id", "status", "initiated_by", "versions_url", "created_at", "updated_at"}, orderBy: "id"},
	// partner_endpoints: what the mock stored of Evolt's OCPI endpoints (registrations.endpoints jsonb),
	// unnested to one row per endpoint — the partner-side mirror of Evolt's ocpi_endpoints.
	"partner_endpoints": {side: "partner", columns: []string{"party_name", "identifier", "role", "url", "updated_at"},
		from: `(SELECT r.party_name, e->>'identifier' AS identifier, e->>'role' AS role, e->>'url' AS url, r.updated_at
			FROM registrations r CROSS JOIN LATERAL jsonb_array_elements(COALESCE(r.endpoints, '[]'::jsonb)) AS e) AS pe`,
		orderBy: "identifier, role"},
	"own_locations": {side: "partner", columns: []string{"id", "last_updated"}, orderBy: "id"},
	"own_tariffs":   {side: "partner", columns: []string{"id", "last_updated"}, orderBy: "id"},
	"received_locations": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "name", "address", "city", "last_updated", "synced_at"},
		expr: map[string]string{"name": "payload->>'name'", "address": "payload->>'address'", "city": "payload->>'city'"}, orderBy: "synced_at DESC"},
	"received_evses": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "evse_uid", "evse_id", "status", "last_updated", "synced_at"},
		expr: map[string]string{"evse_id": "payload->>'evse_id'"}, orderBy: "synced_at DESC"},
	"received_connectors": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "evse_uid", "connector_id", "standard", "format", "power_type", "max_electric_power", "last_updated", "synced_at"},
		expr: map[string]string{"standard": "payload->>'standard'", "format": "payload->>'format'", "power_type": "payload->>'power_type'", "max_electric_power": "payload->>'max_electric_power'"}, orderBy: "synced_at DESC"},
	"received_tariffs": {side: "partner", columns: []string{"country_code", "party_id", "tariff_id", "currency", "type", "last_updated", "synced_at"},
		expr: map[string]string{"currency": "payload->>'currency'", "type": "payload->>'type'"}, orderBy: "synced_at DESC"},
	"request_log": {side: "partner", columns: []string{"id", "ts", "direction", "method", "path", "http_status", "ocpi_status_code", "counterparty", "auth_present"}, orderBy: "id DESC"},
}

// demoEvsesColumns is the safe column set for the demo-station-scoped evses table (added per-instance,
// so its secret-freedom is asserted separately in the test).
var demoEvsesColumns = []string{"id", "evse_id", "uid", "ocpp_status", "status", "charger_id", "last_status_at", "last_heartbeat_at", "updated_at"}

// Menu order per side — a map has none. "evses" appears only when a demo station is configured.
var (
	evoltTableOrder   = []string{"ocpi_credentials", "ocpi_credentials_roles", "ocpi_endpoints", "evolt_ocpi_tariff", "location_map_tariff_ocpi", "evses", "ocpi_partner_locations", "ocpi_partner_evses", "ocpi_partner_connectors", "ocpi_partner_tariffs"}
	partnerTableOrder = []string{"registrations", "partner_endpoints", "own_locations", "own_tariffs", "received_locations", "received_evses", "received_connectors", "received_tariffs", "request_log"}
)

func NewDBBrowser(ctx context.Context, evoltDSN, partnerDSN, demoStationID string) (*DBBrowser, error) {
	b := &DBBrowser{demoStationID: demoStationID, tables: map[string]tableSpec{}, scopeArgs: map[string][]any{}}
	for k, v := range browsableTables {
		b.tables[k] = v
	}
	// evses is the whole charger fleet; scope it to the demo station so the browser stays a one-row
	// window on the EVSE the status flow flips, not a fleet dump.
	if demoStationID != "" {
		b.tables["evses"] = tableSpec{
			side: "evolt", columns: demoEvsesColumns, orderBy: "uid",
			scope: "charger_id IN (SELECT id FROM chargers WHERE station_id = $1)",
		}
		b.scopeArgs["evses"] = []any{demoStationID}
	}
	if evoltDSN != "" {
		pool, err := openPool(ctx, evoltDSN)
		if err != nil {
			return nil, fmt.Errorf("evolt db: %w", err)
		}
		b.evolt = pool
	}
	if partnerDSN != "" {
		pool, err := openPool(ctx, partnerDSN)
		if err != nil {
			if b.evolt != nil {
				b.evolt.Close()
			}
			return nil, fmt.Errorf("partner db: %w", err)
		}
		b.partner = pool
	}
	return b, nil
}

func openPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	// Render timestamptz in Bangkok time so the grid matches the wall clock the
	// team reads; without this ::text falls back to UTC and every synced_at looks
	// 7h stale.
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Bangkok"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

func (b *DBBrowser) Close() {
	if b == nil {
		return
	}
	if b.evolt != nil {
		b.evolt.Close()
	}
	if b.partner != nil {
		b.partner.Close()
	}
}

func (b *DBBrowser) poolFor(side string) *pgxpool.Pool {
	if side == "evolt" {
		return b.evolt
	}
	return b.partner
}

// TableMenu is the whitelist the UI renders as a menu — table name + its searchable columns.
type TableMenu struct {
	Evolt   []TableInfo `json:"evolt"`
	Partner []TableInfo `json:"partner"`
}

type TableInfo struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
}

func (b *DBBrowser) Menu() TableMenu {
	build := func(order []string) []TableInfo {
		out := make([]TableInfo, 0, len(order))
		for _, t := range order {
			spec, ok := b.tables[t]
			if !ok {
				continue // e.g. evses when no demo station is configured
			}
			out = append(out, TableInfo{Table: t, Columns: spec.columns})
		}
		return out
	}
	return TableMenu{Evolt: build(evoltTableOrder), Partner: build(partnerTableOrder)}
}

type BrowseResult struct {
	Table    string      `json:"table"`
	Side     string      `json:"side"`
	Columns  []string    `json:"columns"`
	Rows     [][]*string `json:"rows"`
	Total    int         `json:"total"`
	Page     int         `json:"page"`
	Pages    int         `json:"pages"`
	PageSize int         `json:"page_size"`
}

// Query returns one page of a whitelisted table. filterCol must be one of the table's own columns;
// filterVal is bound as a parameter, as is any scope arg. Every column is cast to text so the result
// is a uniform grid and no pgtype decoding surprises leak through.
func (b *DBBrowser) Query(ctx context.Context, table, filterCol, filterVal string, page int) (*BrowseResult, error) {
	spec, ok := b.tables[table]
	if !ok {
		return nil, fmt.Errorf("unknown table")
	}
	pool := b.poolFor(spec.side)
	if pool == nil {
		return nil, fmt.Errorf("%s database not configured", spec.side)
	}
	if filterCol != "" && !contains(spec.columns, filterCol) {
		return nil, fmt.Errorf("unknown column")
	}
	if page < 1 {
		page = 1
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var conds []string
	var args []any
	if spec.scope != "" {
		conds = append(conds, spec.scope)
		args = append(args, b.scopeArgs[table]...)
	}
	if filterCol != "" && filterVal != "" {
		conds = append(conds, "("+spec.exprOf(filterCol)+")::text ILIKE $"+strconv.Itoa(len(args)+1))
		args = append(args, "%"+filterVal+"%")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+spec.fromClause(table)+where, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count: %w", err)
	}

	pages := (total + browsePageSize - 1) / browsePageSize
	if pages == 0 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	offset := (page - 1) * browsePageSize

	sel := make([]string, len(spec.columns))
	for i, c := range spec.columns {
		sel[i] = "(" + spec.exprOf(c) + ")::text"
	}
	q := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s LIMIT %d OFFSET %d",
		strings.Join(sel, ", "), spec.fromClause(table), where, spec.orderBy, browsePageSize, offset)
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := [][]*string{}
	for rows.Next() {
		vals := make([]*string, len(spec.columns))
		dest := make([]any, len(spec.columns))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return &BrowseResult{
		Table: table, Side: spec.side, Columns: spec.columns,
		Rows: out, Total: total, Page: page, Pages: pages, PageSize: browsePageSize,
	}, nil
}

// SeedEvoltSelf inserts Evolt's own OCPI identity (is_self=true, party TH/EVT) into the three
// credential tables if absent. Idempotent via ON CONFLICT — the same rows the aurora-liquibase seed
// carries, so the demo can restore them after a wipe without hand-run SQL. Returns rows inserted per
// table (0 when already present).
func (b *DBBrowser) SeedEvoltSelf(ctx context.Context) (map[string]int, error) {
	if b.evolt == nil {
		return nil, fmt.Errorf("evolt database not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := b.evolt.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	res := map[string]int{}
	steps := []struct {
		table string
		sql   string
	}{
		{"ocpi_credentials", `
			INSERT INTO ocpi_credentials (id, partner_name, is_self, status, initiated_by, version)
			VALUES ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'Evolt', true, 'PENDING', 'SELF', '2.2.1')
			ON CONFLICT (id) DO NOTHING`},
		{"ocpi_credentials_roles", `
			INSERT INTO ocpi_credentials_roles (credentials_id, role, party_id, country_code, business_name)
			VALUES
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'CPO',  'EVT', 'TH', 'Evolt Co., Ltd.'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'EMSP', 'EVT', 'TH', 'Evolt Co., Ltd.')
			ON CONFLICT (country_code, party_id, role) DO NOTHING`},
		{"ocpi_endpoints", `
			INSERT INTO ocpi_endpoints (credentials_id, identifier, role, url)
			VALUES
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'versions',    'SENDER',   'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/versions'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', '2.2.1',       'SENDER',   'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/2.2.1'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'credentials', 'RECEIVER', 'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/2.2.1/credentials'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'locations',   'SENDER',   'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/cpo/2.2.1/locations'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'locations',   'RECEIVER', 'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/emsp/2.2.1/locations'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'tariffs',     'SENDER',   'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/cpo/2.2.1/tariffs'),
			  ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'tariffs',     'RECEIVER', 'http://bff-external-service-internal.dev.evtech.dev/api/ocpi/emsp/2.2.1/tariffs')
			ON CONFLICT (credentials_id, identifier, role) DO NOTHING`},
	}
	for _, s := range steps {
		tag, err := tx.Exec(ctx, s.sql)
		if err != nil {
			return nil, fmt.Errorf("seed %s: %w", s.table, err)
		}
		res[s.table] = int(tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

// DemoEvse is one EVSE of the demo station, carrying all three identifiers the status flow needs:
// the row id (flip target + Kafka evse_id), uid (Kafka evse_uid), and the numeric OCPI uid
// (evses.evse_id — the uid the OCPI feed/baseline addresses it by).
type DemoEvse struct {
	ID         string `json:"id"`
	UID        string `json:"uid"`
	OCPIUID    string `json:"ocpi_uid"`
	OcppStatus string `json:"ocpp_status"`
}

// DemoStationEvses lists the demo station's EVSEs so the UI can offer them as push targets.
func (b *DBBrowser) DemoStationEvses(ctx context.Context) ([]DemoEvse, error) {
	if b.evolt == nil || b.demoStationID == "" {
		return nil, fmt.Errorf("demo station not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := b.evolt.Query(ctx, `
		SELECT e.id, e.uid, e.evse_id, e.ocpp_status
		FROM evses e JOIN chargers c ON c.id = e.charger_id
		WHERE c.station_id = $1 ORDER BY e.evse_id`, b.demoStationID)
	if err != nil {
		return nil, fmt.Errorf("list demo evses: %w", err)
	}
	defer rows.Close()
	var out []DemoEvse
	for rows.Next() {
		var e DemoEvse
		if err := rows.Scan(&e.ID, &e.UID, &e.OCPIUID, &e.OcppStatus); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SimulateEvseOnline flips one chosen EVSE the same way ChargerDB does for the default one, but scoped
// to the demo station — the UPDATE can only touch a row whose charger belongs to that station, so a
// caller-supplied id can never walk the fleet. Returns the charger id it touched.
func (b *DBBrowser) SimulateEvseOnline(ctx context.Context, evseID, ocppStatus string) (string, error) {
	if b.evolt == nil || b.demoStationID == "" {
		return "", fmt.Errorf("demo station not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := b.evolt.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var chargerID string
	err = tx.QueryRow(ctx, `
		UPDATE evses SET ocpp_status=$1, status='ready', updated_at=now()
		WHERE id=$2 AND charger_id IN (SELECT id FROM chargers WHERE station_id=$3)
		RETURNING charger_id`, ocppStatus, evseID, b.demoStationID).Scan(&chargerID)
	if err != nil {
		return "", fmt.Errorf("evse %s not in demo station or update failed: %w", evseID, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE chargers SET connectivity_status='online', operative_status='ready', updated_at=now() WHERE id=$1`, chargerID); err != nil {
		return "", fmt.Errorf("update charger %s: %w", chargerID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return chargerID, nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
