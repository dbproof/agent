// Package capture reads a production database's schema, planner statistics
// and migration history into a snapshot. It connects as the dedicated capture
// role and never reads rows from application tables.
package capture

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/borovikovd/dbproof-agent/schema"
	"github.com/borovikovd/dbproof-agent/snapshot"
)

// Config says what to capture.
type Config struct {
	Kind snapshot.Kind
	Tool snapshot.Tool
	// HistoryTable is the tool's history table; empty means the tool's default.
	HistoryTable string
	// Exclude holds "schema.*" and "schema.table" patterns.
	Exclude      []string
	AgentVersion string
	// ExpectVersion, for a post-deploy capture, is the migration version the
	// deploy applied. Capture waits until the history table shows it.
	ExpectVersion string
	// WaitTimeout bounds that wait.
	WaitTimeout time.Duration
}

// DefaultHistoryTable returns where each tool keeps its history by default.
func DefaultHistoryTable(tool snapshot.Tool) string {
	if tool == snapshot.ToolAtlas {
		return "atlas_schema_revisions.atlas_schema_revisions"
	}
	return "public.flyway_schema_history"
}

// atlasInPublic is where Atlas keeps its history on Neon: Neon ignores the
// search_path Atlas connects with, so Atlas's advice there is
// --revisions-schema public.
const atlasInPublic = "public.atlas_schema_revisions"

// findHistoryTable returns the tool's default history table, or for Atlas
// the one in public when only that exists.
func findHistoryTable(ctx context.Context, conn *pgx.Conn, tool snapshot.Tool) (string, error) {
	table := DefaultHistoryTable(tool)
	if tool != snapshot.ToolAtlas {
		return table, nil
	}
	var inDefault, inPublic bool
	err := conn.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL, to_regclass($2) IS NOT NULL", table, atlasInPublic).Scan(&inDefault, &inPublic)
	if err != nil {
		return "", fmt.Errorf("find the Atlas history table: %w", err)
	}
	if !inDefault && inPublic {
		return atlasInPublic, nil
	}
	return table, nil
}

// Run captures the database behind conn.
func Run(ctx context.Context, conn *pgx.Conn, cfg Config) (*snapshot.Snapshot, error) {
	if cfg.HistoryTable == "" {
		table, err := findHistoryTable(ctx, conn, cfg.Tool)
		if err != nil {
			return nil, err
		}
		cfg.HistoryTable = table
	}
	if cfg.Kind == snapshot.KindPostDeploy && cfg.ExpectVersion != "" {
		if err := waitForVersion(ctx, conn, cfg); err != nil {
			return nil, err
		}
	}
	s, err := schema.Inspect(ctx, conn, schema.Options{Exclude: cfg.Exclude})
	if err != nil {
		return nil, err
	}
	stats, err := readStats(ctx, conn, s)
	if err != nil {
		return nil, err
	}
	history, err := readHistory(ctx, conn, cfg.Tool, cfg.HistoryTable)
	if err != nil {
		return nil, err
	}
	return &snapshot.Snapshot{
		FormatVersion: snapshot.FormatVersion,
		CapturedAt:    time.Now().UTC(),
		Kind:          cfg.Kind,
		AgentVersion:  cfg.AgentVersion,
		Schema:        s,
		Stats:         stats,
		History:       history,
		Exclusions:    cfg.Exclude,
	}, nil
}

// statsQuery reads the planner's estimates: each table's row count, and for
// each column its null fraction, distinct count and average width. It never
// selects pg_stats' value samples, most_common_vals or histogram_bounds, and
// pg_stats itself shows only the columns the role may read, so a table it
// can't read keeps its row count and has no column statistics.
const statsQuery = `
	SELECT DISTINCT ON (n.nspname, c.relname, s.attname)
	       n.nspname::text, c.relname::text, c.reltuples::float8, s.attname::text,
	       s.null_frac::float8, s.n_distinct::float8, s.avg_width
	FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	LEFT JOIN pg_catalog.pg_stats s ON s.schemaname = n.nspname AND s.tablename = c.relname
	WHERE c.relkind IN ('r', 'p', 'm')
	  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
	  AND n.nspname NOT LIKE 'pg\_%'
	ORDER BY n.nspname, c.relname, s.attname, s.inherited`

// readStats reads the planner's estimates for the tables the inspection
// kept: never values.
func readStats(ctx context.Context, conn *pgx.Conn, s *schema.Schema) ([]snapshot.TableStats, error) {
	rows, err := conn.Query(ctx, statsQuery)
	if err != nil {
		return nil, fmt.Errorf("read statistics: %w", err)
	}
	byTable := map[string]*snapshot.TableStats{}
	var order []string
	var (
		schemaName, tableName string
		nRows                 float64
		column                *string
		nullFrac, nDistinct   *float64
		avgWidth              *int
	)
	_, err = pgx.ForEachRow(rows, []any{&schemaName, &tableName, &nRows, &column, &nullFrac, &nDistinct, &avgWidth}, func() error {
		id := schema.Qualified(schemaName, tableName)
		if s.Table(id) == nil && !isView(s, id) {
			return nil
		}
		t, ok := byTable[id]
		if !ok {
			t = &snapshot.TableStats{Table: id, Rows: nRows}
			byTable[id] = t
			order = append(order, id)
		}
		if column != nil {
			t.Columns = append(t.Columns, snapshot.ColumnStats{Column: *column, NullFrac: deref(nullFrac), NDistinct: deref(nDistinct), AvgWidth: deref(avgWidth)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read statistics: %w", err)
	}
	slices.Sort(order)
	out := make([]snapshot.TableStats, 0, len(order))
	for _, id := range order {
		out = append(out, *byTable[id])
	}
	return out, nil
}

func isView(s *schema.Schema, id string) bool {
	return slices.ContainsFunc(s.Views, func(v schema.View) bool { return v.ID() == id })
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// historyColumns are the columns each tool's history table has, in the
// order capture reads them; required are the ones it can't do without.
var historyColumns = map[snapshot.Tool]struct{ all, required []string }{
	snapshot.ToolFlyway: {
		all:      []string{"installed_rank", "version", "description", "type", "script", "checksum", "installed_by", "installed_on", "execution_time", "success"},
		required: []string{"installed_rank", "version", "script", "success"},
	},
	snapshot.ToolAtlas: {
		all:      []string{"version", "description", "type", "applied", "total", "executed_at", "execution_time", "error", "error_stmt", "hash", "partial_hashes", "operator_version"},
		required: []string{"version", "applied", "total"},
	},
}

// readHistory copies the history table row by row, as text. The simple
// protocol makes Postgres send every value as text, whatever its type. It
// reads only the tool's own history columns, and refuses a table that
// doesn't have them: capture never reads an application's rows, even when
// the history table is misconfigured and the role could read that table.
func readHistory(ctx context.Context, conn *pgx.Conn, tool snapshot.Tool, table string) (*snapshot.History, error) {
	ident, err := quoteTable(table)
	if err != nil {
		return nil, err
	}
	want, ok := historyColumns[tool]
	if !ok {
		return nil, fmt.Errorf("unknown migration tool %q", tool)
	}
	var present []string
	if err := pgxscan(ctx, conn, &present, `SELECT a.attname::text FROM pg_attribute a
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped`, ident); err != nil {
		return nil, fmt.Errorf("read the columns of %s: %w", table, err)
	}
	if len(present) == 0 {
		return nil, fmt.Errorf("there's no %s history table at %s", tool, table)
	}
	for _, c := range want.required {
		if !slices.Contains(present, c) {
			return nil, fmt.Errorf("%s isn't a %s history table: it has no %s column", table, tool, c)
		}
	}
	var cols []string
	for _, c := range want.all {
		if slices.Contains(present, c) {
			cols = append(cols, pgx.Identifier{c}.Sanitize())
		}
	}
	rows, err := conn.Query(ctx, "SELECT "+strings.Join(cols, ", ")+" FROM "+ident+" ORDER BY 1", pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("read migration history from %s: %w", table, err)
	}
	defer rows.Close()
	h := &snapshot.History{Tool: tool, Table: ident}
	for _, f := range rows.FieldDescriptions() {
		h.Columns = append(h.Columns, f.Name)
	}
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]*string, len(raw))
		for i, v := range raw {
			if v != nil {
				s := string(v)
				row[i] = &s
			}
		}
		h.Rows = append(h.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read migration history from %s: %w", table, err)
	}
	return h, nil
}

// quoteTable turns "schema.table" into a quoted identifier.
func quoteTable(table string) (string, error) {
	schemaName, name, ok := strings.Cut(table, ".")
	if !ok || schemaName == "" || name == "" {
		return "", fmt.Errorf("history table %q must be schema.table", table)
	}
	return schema.Qualified(schemaName, name), nil
}

// waitForVersion polls the history until the expected version has applied,
// so a capture right after a deploy sees what startup migrations did.
func waitForVersion(ctx context.Context, conn *pgx.Conn, cfg Config) error {
	timeout := cfg.WaitTimeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for {
		h, err := readHistory(ctx, conn, cfg.Tool, cfg.HistoryTable)
		if err != nil {
			return err
		}
		if slices.Contains(Applied(h), cfg.ExpectVersion) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("migration %s didn't appear in %s within %s", cfg.ExpectVersion, cfg.HistoryTable, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Applied returns the versions the history records as successfully applied,
// in order.
func Applied(h *snapshot.History) []string {
	if h == nil {
		return nil
	}
	var out []string
	for _, row := range h.Rows {
		v := h.Value(row, "version")
		if v == nil || *v == "" {
			continue
		}
		if succeeded(h, row) {
			out = append(out, *v)
		}
	}
	return out
}

// succeeded reads a history row's outcome: Flyway records a success flag;
// Atlas records applied and total statement counts and an error.
func succeeded(h *snapshot.History, row []*string) bool {
	switch h.Tool {
	case snapshot.ToolFlyway:
		s := h.Value(row, "success")
		return s != nil && (*s == "t" || *s == "true")
	case snapshot.ToolAtlas:
		errText, applied, total := h.Value(row, "error"), h.Value(row, "applied"), h.Value(row, "total")
		if errText != nil && *errText != "" {
			return false
		}
		return applied != nil && total != nil && *applied == *total
	}
	return false
}

// pgxscan reads one text column of a query into out.
func pgxscan(ctx context.Context, conn *pgx.Conn, out *[]string, sql string, args ...any) error {
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	*out, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return err
}
