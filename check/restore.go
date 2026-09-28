package check

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/borovikovd/stratum-agent/schema"
	"github.com/borovikovd/stratum-agent/snapshot"
)

// ErrNotEmpty means the check database already holds tables. The check only
// ever runs against a throwaway database, so it refuses rather than touch it.
var ErrNotEmpty = errors.New("the check database isn't empty; point the check at a throwaway database")

// restore recreates the snapshot's schema in the empty database behind conn,
// including the migration history rows, so the migration tool sees exactly
// what production has applied.
func restore(ctx context.Context, conn *pgx.Conn, snap *snapshot.Snapshot) error {
	var tables int
	err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname !~ '^pg_'`).Scan(&tables)
	if err != nil {
		return fmt.Errorf("check the database is empty: %w", err)
	}
	if tables > 0 {
		return ErrNotEmpty
	}
	for _, stmt := range schema.RestoreDDL(snap.Schema) {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("restore: %w\n%s", err, firstLine(stmt))
		}
	}
	if snap.History == nil || len(snap.History.Rows) == 0 {
		return nil
	}
	cols := make([]string, len(snap.History.Columns))
	for i, c := range snap.History.Columns {
		cols[i] = schema.QuoteIdent(c)
	}
	insert := "INSERT INTO " + snap.History.Table + " (" + strings.Join(cols, ", ") + ") VALUES "
	for _, row := range snap.History.Rows {
		vals := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				vals[i] = "NULL"
			} else {
				vals[i] = schema.QuoteLiteral(*v)
			}
		}
		// Untyped literals take the column's type, whatever it is.
		if _, err := conn.Exec(ctx, insert+"("+strings.Join(vals, ", ")+")", pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("restore migration history: %w", err)
		}
	}
	return nil
}

// verify inspects the restored database and describes where it differs from
// the snapshot, if anywhere.
func verify(ctx context.Context, conn *pgx.Conn, snap *snapshot.Snapshot) (string, error) {
	restored, err := schema.Inspect(ctx, conn, schema.Options{})
	if err != nil {
		return "", err
	}
	changes := schema.Diff(snap.Schema, restored, schema.DiffOptions{IgnoreExtensionVersions: true})
	if len(changes) == 0 {
		return "", nil
	}
	var parts []string
	for _, l := range schema.Describe(changes) {
		if l.Op != " " {
			parts = append(parts, l.Op+" "+l.Text)
		}
		if len(parts) == 5 {
			break
		}
	}
	return fmt.Sprintf("%d differences, including: %s", len(changes), strings.Join(parts, "; ")), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
