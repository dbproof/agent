// Package capture reads a production database's schema, planner statistics
// and migration history into a snapshot. It connects as the dedicated capture
// role and never reads rows from application tables.
package capture

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/stratum-dev/agent/schema"
	"github.com/stratum-dev/agent/snapshot"
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

// Run captures the database behind conn.
func Run(ctx context.Context, conn *pgx.Conn, cfg Config) (*snapshot.Snapshot, error) {
	if cfg.HistoryTable == "" {
		cfg.HistoryTable = DefaultHistoryTable(cfg.Tool)
	}
	if cfg.Kind == snapshot.KindPostDeploy && cfg.ExpectVersion != "" {
		if err := waitForVersion(ctx, conn, cfg); err != nil {
			return nil, err
		}
	}
	var role string
	if err := conn.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		return nil, fmt.Errorf("read current role: %w", err)
	}
	s, err := schema.Inspect(ctx, conn, schema.Options{Exclude: cfg.Exclude, IgnoreGrantees: []string{role}})
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

// readStats calls stratum.table_stats(), the security-definer function the
// setup script creates. It returns only estimates, never values, and only for
// tables the inspection kept.
func readStats(ctx context.Context, conn *pgx.Conn, s *schema.Schema) ([]snapshot.TableStats, error) {
	rows, err := conn.Query(ctx, "SELECT schema_name, table_name, n_rows, column_name, null_frac, n_distinct, avg_width FROM stratum.table_stats()")
	if err != nil {
		return nil, fmt.Errorf("read statistics (did the DBA run the setup script?): %w", err)
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

// readHistory copies the history table row by row, as text. The simple
// protocol makes Postgres send every value as text, whatever its type.
func readHistory(ctx context.Context, conn *pgx.Conn, tool snapshot.Tool, table string) (*snapshot.History, error) {
	ident, err := quoteTable(table)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, "SELECT * FROM "+ident+" ORDER BY 1", pgx.QueryExecModeSimpleProtocol)
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
		if ok, err := succeeded(h, row); err == nil && ok {
			out = append(out, *v)
		}
	}
	return out
}

// succeeded reads a history row's outcome: Flyway records a success flag;
// Atlas records applied and total statement counts and an error.
func succeeded(h *snapshot.History, row []*string) (bool, error) {
	switch h.Tool {
	case snapshot.ToolFlyway:
		s := h.Value(row, "success")
		return s != nil && (*s == "t" || *s == "true"), nil
	case snapshot.ToolAtlas:
		errText, applied, total := h.Value(row, "error"), h.Value(row, "applied"), h.Value(row, "total")
		if errText != nil && *errText != "" {
			return false, nil
		}
		return applied != nil && total != nil && *applied == *total, nil
	}
	return false, errors.New("unknown migration tool")
}
