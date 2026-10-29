package source

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/idle-builders/idle-infra/infra-console/internal/api"
)

// PostgresClient reads catalog metadata and, for the row browser, table contents.
//
// It connects as `platform_reader`: SELECT and nothing else, granted per tenant
// schema in terraform/main. The console is outside the schema-per-app boundary
// by design, and this role is the declared exception rather than an assumption.
type PostgresClient struct {
	db        *sql.DB
	maxRows   int
	queryWait time.Duration
}

// NewPostgres wraps an open pool. The caller owns the pool's lifetime.
func NewPostgresClient(db *sql.DB, maxRows int, queryWait time.Duration) *PostgresClient {
	if maxRows <= 0 {
		maxRows = 100
	}
	if queryWait <= 0 {
		queryWait = 5 * time.Second
	}
	return &PostgresClient{db: db, maxRows: maxRows, queryWait: queryWait}
}

func (p *PostgresClient) Configured() bool { return p != nil && p.db != nil }

// identifier is the only shape a schema or table name may take. SQL
// identifiers cannot be parameterized, so they are re-checked here rather than
// trusted from the caller.
//
// Hyphens are allowed: a tenant's schema is its Kubernetes namespace, and
// DNS-1123 permits them — rejecting them would leave a tenant called "my-app"
// unbrowsable. They are safe because every identifier is double-quoted; what
// must stay excluded is the quote, semicolons, dots and whitespace.
var identifier = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_-]*$`)

func quoteIdent(name string) (string, error) {
	if !identifier.MatchString(name) || len(name) > 63 {
		return "", fmt.Errorf("invalid identifier %q", name)
	}
	// Double any embedded quote for completeness; the pattern already excludes
	// them, so this is belt and braces rather than the primary defence.
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

// masksValue reports whether a column's contents must be withheld.
//
// The owner decides, in SQL, and PostgreSQL enforces it. A column is masked
// exactly when platform_reader has no SELECT privilege on it — which is what
// `GRANT SELECT (col, ...)` on a table expresses. The console guesses nothing:
// it asks has_column_privilege and honours the answer.
//
// Two earlier designs were wrong. A blocklist of sensitive-looking NAMES
// caught 4 of 35 realistic ones and missed every container column (metadata,
// payload, notes). A type allowlist masked all free-form text, which withheld
// data nobody asked to hide. Both had the console deciding what is sensitive
// in a schema it does not own.
//
// The primary key is never masked: it is the pagination cursor, and masking it
// returns a truncated page with no nextCursor. A table whose key is itself
// sensitive should not be granted to platform_reader at all.
func masksValue(readable bool, name, pk string) bool {
	if name == pk {
		return false
	}
	return !readable
}

// Summary describes a tenant's schema.
func (p *PostgresClient) Summary(ctx context.Context, schema string) (api.DatabaseSummary, error) {
	if !p.Configured() {
		return api.DatabaseSummary{}, fmt.Errorf("postgres not configured")
	}
	if _, err := quoteIdent(schema); err != nil {
		return api.DatabaseSummary{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.queryWait)
	defer cancel()

	out := api.DatabaseSummary{Schema: schema, Role: &schema}

	// pg_total_relation_size includes indexes and TOAST, which is what an
	// operator means by "how big is this schema".
	const sizeQuery = `
		SELECT COALESCE(SUM(pg_total_relation_size(c.oid)), 0)::bigint, COUNT(*)::int
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'`
	if err := p.db.QueryRowContext(ctx, sizeQuery, schema).Scan(&out.SizeBytes, &out.TableCount); err != nil {
		return api.DatabaseSummary{}, fmt.Errorf("schema size: %w", err)
	}

	// Connections are per database, not per schema; report the tenant role's
	// own connections, which is the number that matters for a quota.
	const connQuery = `SELECT COUNT(*)::int FROM pg_stat_activity WHERE usename = $1`
	var conns int
	if err := p.db.QueryRowContext(ctx, connQuery, schema).Scan(&conns); err == nil {
		out.Connections = &conns
	}

	return out, nil
}

// Tables lists a schema's tables with estimated row counts.
func (p *PostgresClient) Tables(ctx context.Context, schema string) ([]api.Table, error) {
	if !p.Configured() {
		return nil, fmt.Errorf("postgres not configured")
	}
	if _, err := quoteIdent(schema); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, p.queryWait)
	defer cancel()

	// reltuples is an estimate maintained by ANALYZE. COUNT(*) on a large table
	// scans it and can stall the shared instance, which is why the API field is
	// named rowsEstimate.
	const query = `
		SELECT
			c.relname,
			GREATEST(c.reltuples, 0)::bigint AS rows_estimate,
			pg_total_relation_size(c.oid)::bigint AS size_bytes,
			(SELECT COUNT(*)::int FROM pg_index i WHERE i.indrelid = c.oid) AS index_count,
			EXISTS (
				SELECT 1 FROM pg_index i
				WHERE i.indrelid = c.oid AND i.indisprimary
				  AND array_length(i.indkey::int[], 1) = 1
			) AS single_col_pk
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind = 'r'
		ORDER BY c.relname`

	rows, err := p.db.QueryContext(ctx, query, schema)
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []api.Table{}
	for rows.Next() {
		var t api.Table
		var indexCount int
		var singleColPK bool
		if err := rows.Scan(&t.Name, &t.RowsEstimate, &t.SizeBytes, &indexCount, &singleColPK); err != nil {
			return nil, fmt.Errorf("scan table: %w", err)
		}
		t.IndexCount = &indexCount
		t.HasSingleColumnPrimaryKey = &singleColPK
		out = append(out, t)
	}
	return out, rows.Err()
}

// Activity reports live connection and lock state.
func (p *PostgresClient) Activity(ctx context.Context, schema string) (api.DatabaseActivity, error) {
	if !p.Configured() {
		return api.DatabaseActivity{}, fmt.Errorf("postgres not configured")
	}

	ctx, cancel := context.WithTimeout(ctx, p.queryWait)
	defer cancel()

	var out api.DatabaseActivity

	// COUNT(state) detects the privilege restriction. PostgreSQL hides state,
	// query and wait_event from a role that does not own the session, so
	// platform_reader sees the ROWS of a tenant's sessions but NULL in every
	// column it would filter on. Reporting the resulting zeros would be a
	// confident "nothing is running" during exactly the incident this panel is
	// for. The suite could not catch it: the harness connects as superuser.
	const counts = `
		SELECT
			COUNT(*)::int,
			COUNT(state)::int,
			COUNT(*) FILTER (WHERE state = 'active')::int,
			COUNT(*) FILTER (WHERE state = 'idle in transaction')::int,
			COUNT(*) FILTER (WHERE wait_event_type = 'Lock')::int
		FROM pg_stat_activity
		WHERE usename = $1`
	var visibleState int
	if err := p.db.QueryRowContext(ctx, counts, schema).Scan(
		&out.Connections, &visibleState, &out.Active, &out.IdleInTransaction,
		&out.WaitingLocks); err != nil {
		return api.DatabaseActivity{}, fmt.Errorf("activity counts: %w", err)
	}
	if out.Connections > 0 && visibleState == 0 {
		// Deliberately an error, so the section reports itself unavailable.
		// The fix is NOT to grant pg_read_all_stats: that would expose every
		// session's full query text, with literal values substituted, across
		// every tenant — defeating column masking in one line.
		return api.DatabaseActivity{}, errActivityRestricted
	}

	// The leading keyword only, never the statement. pg_stat_activity stores
	// query text with literal values already substituted, so returning any of
	// it would route around the masking the row browser does in SQL.
	const running = `
		SELECT pid, state,
		       (EXTRACT(EPOCH FROM (now() - query_start)) * 1000)::int AS duration_ms,
		       upper((regexp_match(query, '^\s*([A-Za-z]+)'))[1])
		FROM pg_stat_activity
		WHERE usename = $1 AND state = 'active' AND query_start IS NOT NULL
		ORDER BY query_start
		LIMIT 20`

	rows, err := p.db.QueryContext(ctx, running, schema)
	if err != nil {
		return out, nil // counts are still useful
	}
	defer func() { _ = rows.Close() }()

	queries := []activityQuery{}
	longest := 0
	for rows.Next() {
		var pid, durationMs int
		var state, text string
		if err := rows.Scan(&pid, &state, &durationMs, &text); err != nil {
			continue
		}
		if durationMs > longest {
			longest = durationMs
		}
		q := text
		queries = append(queries, activityQuery{
			DurationMs: durationMs, Pid: pid, Query: &q, State: state,
		})
	}
	if len(queries) > 0 {
		out.Queries = &queries
		out.LongestRunningMs = &longest
	}
	return out, nil
}

// RowPage is a bounded, masked page of table contents.
func (p *PostgresClient) Rows(ctx context.Context, schema, table string, limit int, after string) (api.RowPage, error) {
	if !p.Configured() {
		return api.RowPage{}, fmt.Errorf("postgres not configured")
	}

	qSchema, err := quoteIdent(schema)
	if err != nil {
		return api.RowPage{}, err
	}
	qTable, err := quoteIdent(table)
	if err != nil {
		return api.RowPage{}, err
	}
	if limit <= 0 || limit > p.maxRows {
		limit = p.maxRows
	}

	ctx, cancel := context.WithTimeout(ctx, p.queryWait)
	defer cancel()

	columns, pk, err := p.columns(ctx, schema, table)
	if err != nil {
		return api.RowPage{}, err
	}
	if len(columns) == 0 {
		return api.RowPage{}, fmt.Errorf("table %s.%s not found or not readable", schema, table)
	}
	if pk == "" {
		// Without a single-column key there is no stable ordering, and a
		// paged browser over an unstable order shows duplicates and gaps.
		return api.RowPage{}, errNoSingleColumnPK
	}

	// Build the projection. A masked column is replaced by NULL in SQL, so its
	// value never leaves the database.
	projection := make([]string, 0, len(columns))
	masked := []string{}
	outCols := make([]api.Column, 0, len(columns))
	for _, col := range columns {
		quoted, err := quoteIdent(col.name)
		if err != nil {
			return api.RowPage{}, err
		}
		isMasked := masksValue(col.readable, col.name, pk)
		if isMasked {
			projection = append(projection, "NULL AS "+quoted)
			masked = append(masked, col.name)
		} else {
			projection = append(projection, quoted)
		}
		outCols = append(outCols, api.Column{
			Name:     col.name,
			DataType: col.dataType,
			Nullable: &col.nullable,
			Masked:   isMasked,
		})
	}

	qPK, err := quoteIdent(pk)
	if err != nil {
		return api.RowPage{}, err
	}

	// Keyset pagination, never OFFSET: OFFSET rescans and degrades on exactly
	// the large tables where paging matters. One extra row detects truncation.
	query := fmt.Sprintf("SELECT %s FROM %s.%s", strings.Join(projection, ", "), qSchema, qTable)
	args := []any{}
	if after != "" {
		query += fmt.Sprintf(" WHERE %s > $1", qPK)
		args = append(args, after)
	}
	query += fmt.Sprintf(" ORDER BY %s LIMIT %d", qPK, limit+1)

	rows, err := p.db.QueryContext(ctx, query, args...)
	if err != nil {
		return api.RowPage{}, fmt.Errorf("browse rows: %w", err)
	}
	defer func() { _ = rows.Close() }()

	page := api.RowPage{
		Schema:        schema,
		Table:         table,
		Columns:       outCols,
		MaskedColumns: masked,
		Rows:          []map[string]any{},
	}

	names, err := rows.Columns()
	if err != nil {
		return api.RowPage{}, err
	}

	for rows.Next() {
		if len(page.Rows) == limit {
			page.Truncated = true
			break
		}
		holders := make([]any, len(names))
		for i := range holders {
			holders[i] = new(any)
		}
		if err := rows.Scan(holders...); err != nil {
			return api.RowPage{}, fmt.Errorf("scan row: %w", err)
		}
		record := map[string]any{}
		for i, name := range names {
			record[name] = renderValue(*(holders[i].(*any)))
		}
		page.Rows = append(page.Rows, record)
	}
	if err := rows.Err(); err != nil {
		return api.RowPage{}, err
	}

	if page.Truncated && len(page.Rows) > 0 {
		if cursor, ok := page.Rows[len(page.Rows)-1][pk]; ok && cursor != nil {
			s := fmt.Sprint(cursor)
			page.NextCursor = &s
		}
	}

	now := time.Now().UTC()
	page.FetchedAt = &now
	return page, nil
}

// ErrNoSingleColumnPK is returned when a table cannot be paged stably. The
// handler turns it into a 409, which the contract declares.
var errNoSingleColumnPK = fmt.Errorf("table has no single-column primary key")

// errActivityRestricted marks pg_stat_activity columns hidden by privilege.
var errActivityRestricted = fmt.Errorf(
	"pg_stat_activity state is restricted for this role, so session counts would be misleading zeros")

// IsNoSingleColumnPK reports whether an error is the unpageable-table case.
func IsNoSingleColumnPK(err error) bool { return err == errNoSingleColumnPK }

// activityQuery aliases the anonymous struct the generator emits for
// DatabaseActivity.Queries.
type activityQuery = struct {
	DurationMs int     `json:"durationMs"`
	Pid        int     `json:"pid"`
	Query      *string `json:"query,omitempty"`
	State      string  `json:"state"`
}

type columnInfo struct {
	name     string
	dataType string
	nullable bool
	// readable is has_column_privilege: the owner granted SELECT on it.
	readable bool
}

func (p *PostgresClient) columns(ctx context.Context, schema, table string) ([]columnInfo, string, error) {
	const query = `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
		       COALESCE(i.indisprimary AND array_length(i.indkey::int[], 1) = 1, false),
		       has_column_privilege(current_user, c.oid, a.attnum, 'SELECT')
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary AND a.attnum = ANY(i.indkey)
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`

	rows, err := p.db.QueryContext(ctx, query, schema, table)
	if err != nil {
		return nil, "", fmt.Errorf("describe table: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []columnInfo
	var pk string
	for rows.Next() {
		var c columnInfo
		var isPK bool
		if err := rows.Scan(&c.name, &c.dataType, &c.nullable, &isPK, &c.readable); err != nil {
			return nil, "", err
		}
		if isPK && pk == "" {
			pk = c.name
		}
		out = append(out, c)
	}
	return out, pk, rows.Err()
}

// renderValue makes a driver value JSON-safe. []byte is the common surprise:
// it marshals as base64 unless converted, which turns text columns into noise.
func renderValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(t)
	case time.Time:
		return t.UTC()
	default:
		return v
	}
}
