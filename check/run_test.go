package check_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/borovikovd/stratum-agent/capture"
	"github.com/borovikovd/stratum-agent/check"
	"github.com/borovikovd/stratum-agent/internal/pgtest"
	"github.com/borovikovd/stratum-agent/schema"
	"github.com/borovikovd/stratum-agent/snapshot"
)

// flywayLike applies migration files up to a target the way Flyway would:
// each file once, recorded in flyway_schema_history.
type flywayLike struct {
	conn  *pgx.Conn
	files []check.File
}

func (f flywayLike) Migrate(ctx context.Context, target string) (string, error) {
	for i, file := range f.files {
		var done bool
		if err := f.conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM flyway_schema_history WHERE version = $1)", file.Version).Scan(&done); err != nil {
			return "", err
		}
		if !done {
			sql, err := os.ReadFile(file.Path)
			if err != nil {
				return "", err
			}
			if _, err := f.conn.Exec(ctx, string(sql)); err != nil {
				return fmt.Sprintf("ERROR: Migration %s failed\n%v", filepath.Base(file.Path), err), err
			}
			_, err = f.conn.Exec(ctx, `INSERT INTO flyway_schema_history VALUES ($1, $2, 'x', 'SQL', $3, 0, 'check', now(), 1, true)`,
				100+i, file.Version, filepath.Base(file.Path))
			if err != nil {
				return "", err
			}
		}
		if file.Version == target {
			return "ok", nil
		}
	}
	return "ok", nil
}

// production builds a billing database with V1–V3 applied and captures it.
func production(t *testing.T, server pgtest.Server) *snapshot.Snapshot {
	t.Helper()
	db := pgtest.NewDB(t, server)
	pgtest.ExecFile(t, db, "../testdata/schemas/billing.sql")
	pgtest.Exec(t, db, `
		INSERT INTO flyway_schema_history VALUES
		  (1, '1', 'init', 'SQL', 'V1__init.sql', 1, 'deploy', now(), 1, true),
		  (2, '2', 'payments', 'SQL', 'V2__payments.sql', 1, 'deploy', now(), 1, true),
		  (3, '3', 'credit notes', 'SQL', 'V3__credit_notes.sql', 1, 'deploy', now(), 1, true);
		CREATE SCHEMA stratum;
		CREATE FUNCTION stratum.table_stats() RETURNS TABLE (schema_name text, table_name text, n_rows real, column_name text, null_frac real, n_distinct real, avg_width integer)
		  LANGUAGE sql AS $$ SELECT NULL::text, NULL::text, NULL::real, NULL::text, NULL::real, NULL::real, NULL::integer WHERE false $$;`)
	snap, err := capture.Run(context.Background(), pgtest.Connect(t, db), capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func writeFiles(t *testing.T, files map[string]string) (string, []check.File) {
	t.Helper()
	dir := t.TempDir()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := check.ListFiles(snapshot.ToolFlyway, dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, list
}

func runCheck(t *testing.T, server pgtest.Server, snap *snapshot.Snapshot, dir string, files []check.File, prFiles ...string) *check.Report {
	t.Helper()
	conn := pgtest.Connect(t, pgtest.NewDB(t, server))
	var pr []string
	for _, f := range prFiles {
		pr = append(pr, filepath.Join(dir, f))
	}
	report, err := check.Run(context.Background(), check.Config{
		Conn: conn, Snapshot: snap, SnapshotLabel: "S-7", Tool: snapshot.ToolFlyway,
		Files: files, PullRequestFiles: pr, Migrator: flywayLike{conn: conn, files: files}, MigratorName: "flyway migrate",
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

var deployed = map[string]string{
	"V1__init.sql":         "-- already applied",
	"V2__payments.sql":     "-- already applied",
	"V3__credit_notes.sql": "-- already applied",
}

func with(extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range deployed {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestCheckAppliesBaseThenPullRequestMigrations(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			snap := production(t, server)
			dir, files := writeFiles(t, with(map[string]string{
				"V4__customer_tax_id.sql":        "ALTER TABLE customers ADD COLUMN tax_id varchar(32);",
				"V5__require_due_date.sql":       "ALTER TABLE invoices ALTER COLUMN due_date SET NOT NULL;",
				"V6__index_invoice_due_date.sql": "CREATE INDEX invoices_due_date_idx ON invoices (due_date);",
			}))
			r := runCheck(t, server, snap, dir, files, "V5__require_due_date.sql", "V6__index_invoice_due_date.sql")
			if r.SetupProblem != "" {
				t.Fatalf("setup problem: %s", r.SetupProblem)
			}
			var steps []string
			for _, s := range r.Steps {
				steps = append(steps, string(s.Status)+" "+s.Name+" — "+s.Detail)
			}
			want := []string{
				"ok Verified the restore matches S-7 — Schema identical",
				"ok Applied merged, undeployed migrations — V4 · flyway migrate",
				"ok Applied pull request migrations — V5, V6 · flyway migrate",
			}
			for i, w := range want {
				if steps[i+1] != w {
					t.Errorf("step %d = %q, want %q", i+1, steps[i+1], w)
				}
			}
			if len(r.Migrations) != 3 || r.Migrations[0].FromPullRequest || !r.Migrations[1].FromPullRequest {
				t.Fatalf("migrations = %+v", r.Migrations)
			}
			v5 := r.Migrations[1]
			if len(v5.Changes) != 1 || v5.Changes[0].ID != "public.invoices.due_date" || v5.Changes[0].Fields[0] != "not_null" {
				t.Fatalf("V5 changes = %+v, want only due_date becoming NOT NULL", v5.Changes)
			}
			if r.ResultSchema.Table("public.customers").Column("tax_id") == nil {
				t.Error("result schema is missing V4's column")
			}
		})
	}
}

func TestCheckReportsAFailedMigration(t *testing.T) {
	server := pgtest.Servers(t)[0]
	snap := production(t, server)
	dir, files := writeFiles(t, with(map[string]string{
		"V4__add_email.sql":  "ALTER TABLE customers ADD COLUMN email text;",
		"V5__after_fail.sql": "SELECT 1;",
	}))
	r := runCheck(t, server, snap, dir, files, "V4__add_email.sql", "V5__after_fail.sql")
	if len(r.Migrations) != 1 || r.Migrations[0].Applied || r.Migrations[0].Error == "" {
		t.Fatalf("migrations = %+v, want V4 failed and V5 not attempted", r.Migrations)
	}
	last := r.Steps[len(r.Steps)-1]
	if last.Status != check.StatusFailed || last.Detail != "V4 failed" {
		t.Fatalf("last step = %+v", last)
	}
}

func TestCheckReportsASetupProblem(t *testing.T) {
	server := pgtest.Servers(t)[0]
	snap := production(t, server)
	// Production has an extension the check image doesn't.
	snap.Schema.Extensions = append(snap.Schema.Extensions, schema.Extension{Name: "timescaledb", Schema: "public", Version: "2.14"})
	dir, files := writeFiles(t, deployed)
	r := runCheck(t, server, snap, dir, files)
	if r.SetupProblem == "" || len(r.Migrations) != 0 {
		t.Fatalf("report = %+v, want a setup problem and no migrations", r)
	}
}

func TestCheckRefusesADatabaseWithTables(t *testing.T) {
	server := pgtest.Servers(t)[0]
	snap := production(t, server)
	db := pgtest.NewDB(t, server)
	pgtest.Exec(t, db, "CREATE TABLE precious (id int)")
	conn := pgtest.Connect(t, db)
	r, err := check.Run(context.Background(), check.Config{Conn: conn, Snapshot: snap, SnapshotLabel: "S-7", Tool: snapshot.ToolFlyway})
	if err != nil {
		t.Fatal(err)
	}
	if r.SetupProblem == "" {
		t.Fatal("the check ran against a database that already had tables")
	}
}

// A pull request that edits a migration production already ran applies
// nothing here; it must still be reported, or the check passes and the
// deploy fails.
func TestCheckFlagsAnEditedDeployedMigration(t *testing.T) {
	server := pgtest.Servers(t)[0]
	snap := production(t, server)
	dir, files := writeFiles(t, with(map[string]string{"V3__credit_notes.sql": "-- edited after deploy"}))
	r := runCheck(t, server, snap, dir, files, "V3__credit_notes.sql")
	if !slices.Equal(r.AppliedFileChanged, []string{"3"}) || len(r.Migrations) != 0 {
		t.Fatalf("report = %+v, want V3 reported as a changed applied file", r)
	}
}

// missesTheDatabase is a migrate command that succeeds somewhere else.
type missesTheDatabase struct{}

func (missesTheDatabase) Migrate(context.Context, string) (string, error) { return "ok", nil }

func TestCheckNoticesAMigrateCommandAimedElsewhere(t *testing.T) {
	server := pgtest.Servers(t)[0]
	snap := production(t, server)
	dir, files := writeFiles(t, with(map[string]string{"V4__add_email.sql": "ALTER TABLE customers ADD COLUMN email text;"}))
	conn := pgtest.Connect(t, pgtest.NewDB(t, server))
	r, err := check.Run(context.Background(), check.Config{
		Conn: conn, Snapshot: snap, SnapshotLabel: "S-7", Tool: snapshot.ToolFlyway,
		Files: files, PullRequestFiles: []string{filepath.Join(dir, "V4__add_email.sql")}, Migrator: missesTheDatabase{}, MigratorName: "flyway migrate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.SetupProblem, "ran against another database") {
		t.Fatalf("report = %+v, want a setup problem", r)
	}
}
