package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	partner       *pgxpool.Pool            // PLG — the interactive partner, and the default
	partnerAlt    map[string]*pgxpool.Pool // fanout partners' own DBs, keyed "vct"/"chx"
	partnerKeys   []string                 // menu order: "plg" first, then alt keys as configured
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
	join    *crossDBJoin      // optional column filled from the other database after the query
}

// crossDBJoin fills a column from the other side's database. OCPI carries no station on a Tariff, so
// the only way to answer "which station published this price" on the partner's side is to resolve it
// against Evolt's own materialised table — one lookup per page, not a join the database can do.
type crossDBJoin struct {
	column string // the column in spec.columns that this fills; never selected from SQL
	key    string // the column in spec.columns whose value is looked up
	side   string // which pool answers the lookup
	sql    string // constant, from this file only: SELECT <key>, <value> ... WHERE <key> = ANY($1)
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
	"ocpi_partner_tariffs": {side: "evolt", columns: []string{"tariff_id", "name", "price", "price_type", "vat", "days", "hours", "elements", "currency", "type", "min_price_incl_vat", "max_price_incl_vat", "start_date_time", "end_date_time", "last_updated", "synced_at", "deleted_at"},
		expr: map[string]string{
			"name":       `tariff_alt_text->0->>'text'`,
			"price":      `elements->0->'price_components'->0->>'price'`,
			"price_type": `elements->0->'price_components'->0->>'type'`,
			"vat":        `elements->0->'price_components'->0->>'vat'`,
			"days":       `array_to_string(ARRAY(SELECT jsonb_array_elements_text(COALESCE(elements->0->'restrictions'->'day_of_week','[]'::jsonb))), ',')`,
			"hours":      `concat_ws('-', elements->0->'restrictions'->>'start_time', elements->0->'restrictions'->>'end_time')`,
			"elements":   `jsonb_array_length(elements)`,
		}, orderBy: "last_updated DESC"},
	// Partner (mock DB) — what PlugSiam holds. registrations.token_* / request_log.body_excerpt omitted on purpose.
	"registrations": {side: "partner", columns: []string{"id", "party_name", "country_code", "party_id", "status", "initiated_by", "versions_url", "created_at", "updated_at"}, orderBy: "id"},
	// partner_endpoints: what the mock stored of Evolt's OCPI endpoints (registrations.endpoints jsonb),
	// unnested to one row per endpoint — the partner-side mirror of Evolt's ocpi_endpoints.
	"partner_endpoints": {side: "partner", columns: []string{"party_name", "identifier", "role", "url", "updated_at"},
		from: `(SELECT r.party_name, e->>'identifier' AS identifier, e->>'role' AS role, e->>'url' AS url, r.updated_at
			FROM registrations r CROSS JOIN LATERAL jsonb_array_elements(COALESCE(r.endpoints, '[]'::jsonb)) AS e) AS pe`,
		orderBy: "identifier, role"},
	"own_locations": {side: "partner", columns: []string{"id", "source", "name", "city", "last_updated"},
		expr: map[string]string{"name": "payload->>'name'", "city": "payload->>'city'"}, orderBy: "source, id"},
	"own_tariffs": {side: "partner", columns: []string{"id", "source", "currency", "type", "last_updated"},
		expr: map[string]string{"currency": "payload->>'currency'", "type": "payload->>'type'"}, orderBy: "source, id"},
	"received_locations": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "name", "address", "city", "last_updated", "synced_at"},
		expr: map[string]string{"name": "payload->>'name'", "address": "payload->>'address'", "city": "payload->>'city'"}, orderBy: "synced_at DESC"},
	"received_evses": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "evse_uid", "evse_id", "status", "last_updated", "synced_at"},
		expr: map[string]string{"evse_id": "payload->>'evse_id'"}, orderBy: "synced_at DESC"},
	"received_connectors": {side: "partner", columns: []string{"country_code", "party_id", "location_id", "evse_uid", "connector_id", "standard", "format", "power_type", "max_electric_power", "last_updated", "synced_at"},
		expr: map[string]string{"standard": "payload->>'standard'", "format": "payload->>'format'", "power_type": "payload->>'power_type'", "max_electric_power": "payload->>'max_electric_power'"}, orderBy: "synced_at DESC"},
	"received_tariffs": {side: "partner", columns: []string{"tariff_id", "station_id", "name", "price", "price_type", "vat", "days", "hours", "elements", "currency", "type", "country_code", "party_id", "last_updated", "synced_at"},
		expr: map[string]string{
			"currency":   "payload->>'currency'",
			"type":       "payload->>'type'",
			"name":       `payload->'tariff_alt_text'->0->>'text'`,
			"price":      `payload->'elements'->0->'price_components'->0->>'price'`,
			"price_type": `payload->'elements'->0->'price_components'->0->>'type'`,
			"vat":        `payload->'elements'->0->'price_components'->0->>'vat'`,
			"days":       `array_to_string(ARRAY(SELECT jsonb_array_elements_text(COALESCE(payload->'elements'->0->'restrictions'->'day_of_week','[]'::jsonb))), ',')`,
			"hours":      `concat_ws('-', payload->'elements'->0->'restrictions'->>'start_time', payload->'elements'->0->'restrictions'->>'end_time')`,
			"elements":   `jsonb_array_length(payload->'elements')`,
		},
		orderBy: "synced_at DESC",
		join: &crossDBJoin{column: "station_id", key: "tariff_id", side: "evolt",
			sql: `SELECT id::text, station_id::text FROM evolt_ocpi_tariff WHERE id::text = ANY($1)`}},
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

// altPartnerDSNs maps a fanout partner key ("vct") to that mock's own DSN; each becomes a switchable
// database on the partner side of the browser. Order of keys decides the menu order after "plg".
func NewDBBrowser(ctx context.Context, evoltDSN, partnerDSN, demoStationID string, altPartnerDSNs [][2]string) (*DBBrowser, error) {
	b := &DBBrowser{demoStationID: demoStationID, tables: map[string]tableSpec{}, scopeArgs: map[string][]any{},
		partnerAlt: map[string]*pgxpool.Pool{}}
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
			b.Close()
			return nil, fmt.Errorf("partner db: %w", err)
		}
		b.partner = pool
		b.partnerKeys = append(b.partnerKeys, "plg")
	}
	for _, kv := range altPartnerDSNs {
		key, dsn := kv[0], kv[1]
		if dsn == "" || key == "" || key == "plg" {
			continue
		}
		pool, err := openPool(ctx, dsn)
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("partner db %s: %w", key, err)
		}
		b.partnerAlt[key] = pool
		b.partnerKeys = append(b.partnerKeys, key)
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
	for _, p := range b.partnerAlt {
		p.Close()
	}
}

// poolFor picks the pool a query runs on. db selects among partner databases ("" = PLG); the Evolt
// side has exactly one database, so db is ignored there.
func (b *DBBrowser) poolFor(side, db string) (*pgxpool.Pool, error) {
	if side == "evolt" {
		return b.evolt, nil
	}
	if db == "" || db == "plg" {
		return b.partner, nil
	}
	pool, ok := b.partnerAlt[db]
	if !ok {
		return nil, fmt.Errorf("unknown partner db %q", db)
	}
	return pool, nil
}

// TableMenu is the whitelist the UI renders as a menu — table name + its searchable columns.
// PartnerDBs lists the partner databases the browser can switch between ("plg" first); a single-DB
// setup gets ["plg"] and the UI hides the switch.
type TableMenu struct {
	Evolt      []TableInfo `json:"evolt"`
	Partner    []TableInfo `json:"partner"`
	PartnerDBs []string    `json:"partner_dbs"`
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
	return TableMenu{Evolt: build(evoltTableOrder), Partner: build(partnerTableOrder), PartnerDBs: b.partnerKeys}
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
	DB       string      `json:"db,omitempty"` // which partner database answered ("plg"/"vct"/"chx"); empty on the Evolt side
}

// Query returns one page of a whitelisted table. filterCol must be one of the table's own columns;
// filterVal is bound as a parameter, as is any scope arg. Every column is cast to text so the result
// is a uniform grid and no pgtype decoding surprises leak through. db picks which partner's database
// answers a partner-side table ("" = PLG); it never selects the SQL, only the pool.
func (b *DBBrowser) Query(ctx context.Context, table, db, filterCol, filterVal string, page int) (*BrowseResult, error) {
	spec, ok := b.tables[table]
	if !ok {
		return nil, fmt.Errorf("unknown table")
	}
	if filterCol != "" && !contains(spec.columns, filterCol) {
		return nil, fmt.Errorf("unknown column")
	}
	if spec.join != nil && filterCol == spec.join.column {
		return nil, fmt.Errorf("ค้นหาคอลัมน์นี้ไม่ได้ — ค่ามาจาก DB อีกฝั่ง ให้ค้นด้วย %s แทน", spec.join.key)
	}
	pool, err := b.poolFor(spec.side, db)
	if err != nil {
		return nil, err
	}
	if pool == nil {
		return nil, fmt.Errorf("%s database not configured", spec.side)
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
		if spec.join != nil && c == spec.join.column {
			sel[i] = "NULL::text" // filled after the query, from the other database
			continue
		}
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
	// A failed lookup leaves the column blank: the page still shows the rows the table does hold.
	if spec.join != nil {
		_ = b.fillCrossDB(ctx, spec, out)
	}

	res := &BrowseResult{
		Table: table, Side: spec.side, Columns: spec.columns,
		Rows: out, Total: total, Page: page, Pages: pages, PageSize: browsePageSize,
	}
	if spec.side == "partner" {
		res.DB = db
		if res.DB == "" {
			res.DB = "plg"
		}
	}
	return res, nil
}

func (b *DBBrowser) fillCrossDB(ctx context.Context, spec tableSpec, rows [][]*string) error {
	pool, err := b.poolFor(spec.join.side, "")
	if err != nil || pool == nil {
		return fmt.Errorf("%s database not configured", spec.join.side)
	}
	keyIdx, valIdx := indexOf(spec.columns, spec.join.key), indexOf(spec.columns, spec.join.column)
	if keyIdx < 0 || valIdx < 0 {
		return fmt.Errorf("join columns not in the table")
	}

	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		if r[keyIdx] != nil {
			keys = append(keys, *r[keyIdx])
		}
	}
	if len(keys) == 0 {
		return nil
	}

	found, err := pool.Query(ctx, spec.join.sql, keys)
	if err != nil {
		return err
	}
	defer found.Close()
	lookup := map[string]string{}
	for found.Next() {
		var k, v *string
		if err := found.Scan(&k, &v); err != nil {
			return err
		}
		if k != nil && v != nil {
			lookup[*k] = *v
		}
	}
	if err := found.Err(); err != nil {
		return err
	}

	for _, r := range rows {
		if r[keyIdx] == nil {
			continue
		}
		if v, hit := lookup[*r[keyIdx]]; hit {
			val := v
			r[valIdx] = &val
		}
	}
	return nil
}

func indexOf(list []string, want string) int {
	for i, v := range list {
		if v == want {
			return i
		}
	}
	return -1
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
		// The self row is our own identity, not a handshake waiting on a counterparty, so it lands
		// REGISTERED. Conflicts repair the status rather than skip: a row seeded PENDING earlier
		// otherwise stays that way forever, since the id never changes.
		{"ocpi_credentials", `
			INSERT INTO ocpi_credentials (id, partner_name, is_self, status, initiated_by, version, registered_at)
			VALUES ('ddd81667-4652-4768-ab9c-cbe94c65540e', 'Evolt', true, 'REGISTERED', 'SELF', '2.2.1', now())
			ON CONFLICT (id) DO UPDATE SET status = 'REGISTERED',
				registered_at = COALESCE(ocpi_credentials.registered_at, now())`},
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
	// What the partner holds for this EVSE right now. Empty means the partner has no row for it,
	// so a PATCH would answer 2003 until a baseline lands.
	PartnerStatus string `json:"partner_status"`
	InPartner     bool   `json:"in_partner"`
}

// PartnerCacheLocation is one partner location as Evolt cached it, with the EVSE statuses under it —
// the eMSP-side mirror of what the mock shows for the other direction.
type PartnerCacheLocation struct {
	LocationID string `json:"location_id"`
	Name       string `json:"name"`
	City       string `json:"city"`
	// LastUpdated is the partner's own timestamp; SyncedAt is when Evolt received it. A PUT that
	// replays an unchanged object moves only SyncedAt, which is why the panel sorts on it — the
	// object you just pushed is the one you want to look at, changed or not.
	LastUpdated string             `json:"last_updated"`
	SyncedAt    string             `json:"synced_at"`
	Evses       []PartnerCacheEvse `json:"evses"`
}

type PartnerCacheEvse struct {
	UID         string                  `json:"uid"`
	Status      string                  `json:"status"`
	LastUpdated string                  `json:"last_updated"`
	Connectors  []PartnerCacheConnector `json:"connectors,omitempty"`
}

// PartnerCacheConnector is what the panel needs to answer "how fast is this head" without
// sending the reader to the Tables tab.
type PartnerCacheConnector struct {
	ID       string `json:"id"`
	Standard string `json:"standard"`
	PowerW   int    `json:"power_w"`
}

// DeletePartnerCredentials removes this partner's registration from Evolt; roles, endpoints and the
// ocpi_partner_* cache follow by FK cascade. Evolt has no DELETE /credentials endpoint (OCPI §7 gap),
// so without this a re-handshake for a party whose row is still REGISTERED answers 9999 forever.
// The is_self=false guard is non-negotiable — the self row died to its absence once already.
func (b *DBBrowser) DeletePartnerCredentials(ctx context.Context, countryCode, partyID string) (int, error) {
	if b.evolt == nil {
		return 0, fmt.Errorf("evolt database not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tag, err := b.evolt.Exec(ctx, `
		DELETE FROM ocpi_credentials
		WHERE is_self = false AND id IN (
			SELECT credentials_id FROM ocpi_credentials_roles
			WHERE country_code = $1 AND party_id = $2)`, countryCode, partyID)
	if err != nil {
		return 0, fmt.Errorf("delete partner credentials: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// TariffOrigin names the station behind a materialized tariff. The OCPI Tariff object carries no
// station at all — resolving against evolt_ocpi_tariff (+ stations, read-only) is the only way a
// panel can say "this price belongs to Megabangna, DC side".
type TariffOrigin struct {
	StationID   string `json:"station_id"`
	StationName string `json:"station_name"`
	Scope       string `json:"scope"`
}

func (b *DBBrowser) TariffOrigins(ctx context.Context, ids []string) (map[string]TariffOrigin, error) {
	out := map[string]TariffOrigin{}
	if b.evolt == nil || len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := b.evolt.Query(ctx, `
		SELECT t.id::text, t.station_id::text, COALESCE(s.name,''), COALESCE(t.scope,'')
		FROM evolt_ocpi_tariff t
		LEFT JOIN stations s ON s.id = t.station_id
		WHERE t.id::text = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve tariff origins: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var o TariffOrigin
		if err := rows.Scan(&id, &o.StationID, &o.StationName, &o.Scope); err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}

// PartnerRegistration reads Evolt's own status column for one party. The HTTP partner list cannot
// answer this: it returns REGISTERED rows only, so a handshake that is mid-flight (PENDING) or a
// partner that left (REVOKED) both look exactly like "never registered" from outside. Empty status
// means no row at all.
func (b *DBBrowser) PartnerRegistration(ctx context.Context, countryCode, partyID string) (string, time.Time, error) {
	if b.evolt == nil {
		return "", time.Time{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var status string
	var at time.Time
	err := b.evolt.QueryRow(ctx, `
		SELECT c.status, COALESCE(c.updated_at, c.created_at)
		FROM ocpi_credentials c
		JOIN ocpi_credentials_roles r ON r.credentials_id = c.id
		WHERE c.is_self = false AND r.country_code = $1 AND r.party_id = $2
		ORDER BY COALESCE(c.updated_at, c.created_at) DESC
		LIMIT 1`, strings.ToUpper(countryCode), strings.ToUpper(partyID)).Scan(&status, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, nil
	}
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read partner registration: %w", err)
	}
	return status, at, nil
}

// PartnerCredential is Evolt's whole view of one partner registration, taken before and after a
// credentials PUT so the UI can show what the call actually changed. TokenIssuedAt is the proof the
// token rotated — the token itself is never read out of the database.
type PartnerCredential struct {
	Status        string            `json:"status"`
	VersionsURL   string            `json:"versions_url"`
	TokenIssuedAt *time.Time        `json:"token_issued_at,omitempty"`
	Endpoints     int               `json:"endpoints"`
	Roles         []PartnerCredRole `json:"roles"`
}

type PartnerCredRole struct {
	Role         string `json:"role"`
	Party        string `json:"party"`
	BusinessName string `json:"business_name"`
	Website      string `json:"website"`
	LogoURL      string `json:"logo_url"`
}

// PartnerCredential reads that view for one party. A missing row is not an error: the caller wants
// "before" even when the partner is not registered yet, and an empty Status says so.
func (b *DBBrowser) PartnerCredential(ctx context.Context, countryCode, partyID string) (PartnerCredential, error) {
	out := PartnerCredential{Roles: []PartnerCredRole{}}
	if b.evolt == nil {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cc, pid := strings.ToUpper(countryCode), strings.ToUpper(partyID)

	var id string
	var issued *time.Time
	err := b.evolt.QueryRow(ctx, `
		SELECT c.id::text, c.status, COALESCE(c.partner_versions_url,''), c.token_inbound_issued_at
		FROM ocpi_credentials c
		JOIN ocpi_credentials_roles r ON r.credentials_id = c.id
		WHERE c.is_self = false AND r.country_code = $1 AND r.party_id = $2
		ORDER BY COALESCE(c.updated_at, c.created_at) DESC
		LIMIT 1`, cc, pid).Scan(&id, &out.Status, &out.VersionsURL, &issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read partner credential: %w", err)
	}
	out.TokenIssuedAt = issued

	if err := b.evolt.QueryRow(ctx,
		`SELECT count(*) FROM ocpi_endpoints WHERE credentials_id = $1`, id).Scan(&out.Endpoints); err != nil {
		return out, fmt.Errorf("count partner endpoints: %w", err)
	}

	rows, err := b.evolt.Query(ctx, `
		SELECT role::text, country_code || '/' || party_id, business_name,
		       COALESCE(business_website,''), COALESCE(logo_url,'')
		FROM ocpi_credentials_roles WHERE credentials_id = $1 ORDER BY role`, id)
	if err != nil {
		return out, fmt.Errorf("read partner roles: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r PartnerCredRole
		if err := rows.Scan(&r.Role, &r.Party, &r.BusinessName, &r.Website, &r.LogoURL); err != nil {
			return out, err
		}
		out.Roles = append(out.Roles, r)
	}
	return out, rows.Err()
}

// StationNames resolves the demo's allowed station ids to their names, so a picker reads
// "TAPPY Station" rather than a truncated uuid.
func (b *DBBrowser) StationNames(ctx context.Context, ids []string) (map[string]string, error) {
	out := map[string]string{}
	if b.evolt == nil || len(ids) == 0 {
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := b.evolt.Query(ctx, `SELECT id::text, COALESCE(name,'') FROM stations WHERE id::text = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve station names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// PartnerCacheTariff is one partner tariff as Evolt cached it; Raw is the stored OCPI object so the
// panel can price it the same way the mock's side does.
type PartnerCacheTariff struct {
	TariffID    string          `json:"tariff_id"`
	Raw         json.RawMessage `json:"raw"`
	LastUpdated string          `json:"last_updated"`
	SyncedAt    string          `json:"synced_at"`
	DeletedAt   string          `json:"deleted_at,omitempty"`
}

func (b *DBBrowser) PartnerCacheTariffs(ctx context.Context, countryCode, partyID string, limit int) ([]PartnerCacheTariff, error) {
	if b.evolt == nil {
		return nil, fmt.Errorf("evolt database not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := b.evolt.Query(ctx, `
		SELECT tariff_id, COALESCE(raw, '{}'::jsonb),
		       to_char(last_updated AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'),
		       to_char(synced_at AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'),
		       COALESCE(to_char(deleted_at AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'), '')
		FROM ocpi_partner_tariffs
		WHERE country_code = $1 AND party_id = $2
		ORDER BY synced_at DESC
		LIMIT $3`, countryCode, partyID, limit)
	if err != nil {
		return nil, fmt.Errorf("read partner tariffs: %w", err)
	}
	defer rows.Close()
	out := []PartnerCacheTariff{}
	for rows.Next() {
		var t PartnerCacheTariff
		if err := rows.Scan(&t.TariffID, &t.Raw, &t.LastUpdated, &t.SyncedAt, &t.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PartnerCacheLocations reads what Evolt holds for one partner. Neither table carries a soft-delete
// column (only ocpi_partner_tariffs does), so every cached row counts.
func (b *DBBrowser) PartnerCacheLocations(ctx context.Context, countryCode, partyID string, limit int) ([]PartnerCacheLocation, error) {
	if b.evolt == nil {
		return nil, fmt.Errorf("evolt database not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := b.evolt.Query(ctx, `
		SELECT l.location_id, COALESCE(l.name,''), COALESCE(l.city,''),
		       to_char(l.last_updated AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'),
		       COALESCE(to_char(l.synced_at AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'),''),
		       COALESCE(e.evse_uid,''), COALESCE(e.status,''),
		       COALESCE(to_char(e.last_updated AT TIME ZONE 'Asia/Bangkok', 'YYYY-MM-DD"T"HH24:MI:SS'),''),
		       COALESCE((SELECT string_agg(c.connector_id || '|' || COALESCE(c.standard,'') || '|' ||
		                        COALESCE(c.max_electric_power::text,''), ';' ORDER BY c.connector_id)
		                 FROM ocpi_partner_connectors c WHERE c.evse_id = e.id), '')
		FROM ocpi_partner_locations l
		LEFT JOIN ocpi_partner_evses e ON e.location_id = l.id
		WHERE l.country_code = $1 AND l.party_id = $2
		ORDER BY l.synced_at DESC NULLS LAST, l.location_id, e.evse_uid
		LIMIT $3`, countryCode, partyID, limit*8)
	if err != nil {
		return nil, fmt.Errorf("read partner cache: %w", err)
	}
	defer rows.Close()

	out := []PartnerCacheLocation{}
	byID := map[string]int{}
	for rows.Next() {
		var locID, name, city, locUpdated, locSynced, evseUID, status, evseUpdated, conns string
		if err := rows.Scan(&locID, &name, &city, &locUpdated, &locSynced, &evseUID, &status, &evseUpdated, &conns); err != nil {
			return nil, err
		}
		idx, seen := byID[locID]
		if !seen {
			if len(out) >= limit {
				continue
			}
			out = append(out, PartnerCacheLocation{LocationID: locID, Name: name, City: city, LastUpdated: locUpdated, SyncedAt: locSynced})
			idx = len(out) - 1
			byID[locID] = idx
		}
		if evseUID != "" {
			out[idx].Evses = append(out[idx].Evses, PartnerCacheEvse{
				UID: evseUID, Status: status, LastUpdated: evseUpdated, Connectors: parseConnectorAgg(conns)})
		}
	}
	return out, rows.Err()
}

// parseConnectorAgg unpacks the "id|standard|watts;…" string the query aggregates per EVSE.
func parseConnectorAgg(agg string) []PartnerCacheConnector {
	if agg == "" {
		return nil
	}
	var out []PartnerCacheConnector
	for _, part := range strings.Split(agg, ";") {
		f := strings.SplitN(part, "|", 3)
		if len(f) != 3 {
			continue
		}
		w, _ := strconv.Atoi(f[2])
		out = append(out, PartnerCacheConnector{ID: f[0], Standard: f[1], PowerW: w})
	}
	return out
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
