package capture_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/borovikovd/stratum-agent/capture"
	"github.com/borovikovd/stratum-agent/internal/pgtest"
	"github.com/borovikovd/stratum-agent/snapshot"
)

// setup runs the DBA setup script, minus CREATE ROLE: roles are cluster-wide,
// so pgtest creates the capture role once for every test database.
func setup(t *testing.T, db string, tool snapshot.Tool) {
	t.Helper()
	sql, err := capture.SetupSQL(tool, "")
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(sql, "\n") {
		if !strings.HasPrefix(line, "CREATE ROLE") {
			kept = append(kept, line)
		}
	}
	pgtest.CaptureURL(t, db)
	pgtest.Exec(t, db, strings.Join(kept, "\n"))
}

func TestCaptureAsTheCaptureRole(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			db := pgtest.NewDB(t, server)
			pgtest.ExecFile(t, db, "../testdata/schemas/billing.sql")
			pgtest.Exec(t, db, `
				INSERT INTO flyway_schema_history VALUES
				  (1, '1', 'init', 'SQL', 'V1__init.sql', 123, 'deploy', now(), 10, true),
				  (2, '2', 'payments', 'SQL', 'V2__payments.sql', 456, 'deploy', now(), 10, true),
				  (3, '3', 'broken', 'SQL', 'V3__broken.sql', 789, 'deploy', now(), 10, false);
				INSERT INTO customers (name, email) SELECT 'c' || i, i || '@example.com' FROM generate_series(1, 200) i;
				INSERT INTO invoices (customer_id, number, amount_cents, due_date)
				  SELECT 1 + i % 200, 'INV-' || i, 100, CASE WHEN i % 4 = 0 THEN NULL ELSE current_date END
				  FROM generate_series(1, 1000) i;
				ANALYZE;`)
			setup(t, db, snapshot.ToolFlyway)

			conn := pgtest.Connect(t, pgtest.CaptureURL(t, db))
			// The capture role can't read application rows.
			if _, err := conn.Exec(context.Background(), "SELECT * FROM invoices LIMIT 1"); err == nil {
				t.Fatal("the capture role could read invoices")
			}

			snap, err := capture.Run(context.Background(), conn, capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Schema.Table("public.invoices") == nil {
				t.Fatal("invoices missing from the schema")
			}
			for _, n := range snap.Schema.Namespaces {
				if n.Name == "stratum" {
					t.Error("Stratum's own schema was captured")
				}
			}
			for _, g := range snap.Schema.Grants {
				if g.Grantee == pgtest.CaptureRole {
					t.Errorf("the capture role's own grant was captured: %+v", g)
				}
			}

			invoices := findStats(snap, "public.invoices")
			if invoices == nil || invoices.Rows != 1000 {
				t.Fatalf("invoices stats = %+v, want 1000 rows", invoices)
			}
			var dueDate *snapshot.ColumnStats
			for i := range invoices.Columns {
				if invoices.Columns[i].Column == "due_date" {
					dueDate = &invoices.Columns[i]
				}
			}
			if dueDate == nil || dueDate.NullFrac < 0.2 || dueDate.NullFrac > 0.3 {
				t.Fatalf("due_date stats = %+v, want a null fraction near 0.25", dueDate)
			}

			if got := capture.Applied(snap.History); !slices.Equal(got, []string{"1", "2"}) {
				t.Fatalf("Applied = %v, want [1 2]; the failed V3 must not count", got)
			}
			if snap.History.Table != "public.flyway_schema_history" || len(snap.History.Rows) != 3 {
				t.Fatalf("history = %+v", snap.History)
			}
		})
	}
}

func TestPostDeployCaptureWaitsForTheDeployedVersion(t *testing.T) {
	server := pgtest.Servers(t)[0]
	db := pgtest.NewDB(t, server)
	pgtest.ExecFile(t, db, "../testdata/schemas/billing.sql")
	setup(t, db, snapshot.ToolFlyway)
	conn := pgtest.Connect(t, pgtest.CaptureURL(t, db))

	cfg := capture.Config{Kind: snapshot.KindPostDeploy, Tool: snapshot.ToolFlyway, ExpectVersion: "7", WaitTimeout: 1}
	if _, err := capture.Run(context.Background(), conn, cfg); err == nil || !strings.Contains(err.Error(), "didn't appear") {
		t.Fatalf("err = %v, want a timeout waiting for version 7", err)
	}

	pgtest.Exec(t, db, `INSERT INTO flyway_schema_history VALUES (1, '7', 'x', 'SQL', 'V7__x.sql', 1, 'deploy', now(), 1, true)`)
	if _, err := capture.Run(context.Background(), conn, cfg); err != nil {
		t.Fatalf("after the deploy recorded version 7: %v", err)
	}
}

func findStats(s *snapshot.Snapshot, table string) *snapshot.TableStats {
	for i := range s.Stats {
		if s.Stats[i].Table == table {
			return &s.Stats[i]
		}
	}
	return nil
}

// Capture reads only a history table's own columns, and refuses a table
// that isn't one, even for a role that could read the application's rows.
func TestCaptureReadsOnlyTheHistoryTable(t *testing.T) {
	server := pgtest.Servers(t)[0]
	db := pgtest.NewDB(t, server)
	pgtest.ExecFile(t, db, "../testdata/schemas/billing.sql")
	pgtest.Exec(t, db, `
		ALTER TABLE flyway_schema_history ADD COLUMN note text;
		INSERT INTO flyway_schema_history VALUES (1, '1', 'init', 'SQL', 'V1__init.sql', 123, 'deploy', now(), 10, true, 'private');
		INSERT INTO customers (name, email) VALUES ('Ada', 'ada@example.com');`)
	setup(t, db, snapshot.ToolFlyway)
	owner := pgtest.Connect(t, db)
	ctx := context.Background()

	snap, err := capture.Run(ctx, owner, capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(snap.History.Columns, "note") {
		t.Fatalf("history columns = %v, want Flyway's own only", snap.History.Columns)
	}
	_, err = capture.Run(ctx, owner, capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway, HistoryTable: "public.customers"})
	if err == nil || !strings.Contains(err.Error(), "isn't a flyway history table") {
		t.Fatalf("capture with customers as the history = %v, want a refusal", err)
	}
}
