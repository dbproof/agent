package capture_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/dbproof/agent/capture"
	"github.com/dbproof/agent/internal/pgtest"
	"github.com/dbproof/agent/snapshot"
)

// seed fills billing with history and rows, and gathers statistics.
func seed(t *testing.T, db string) {
	t.Helper()
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
}

// Capture runs as the role migrations run as, which owns the tables: the
// schema, each table's statistics and the migration history.
func TestCaptureAsTheMigrationRole(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			db := pgtest.NewDB(t, server)
			seed(t, db)

			snap, err := capture.Run(context.Background(), pgtest.Connect(t, db), capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway})
			if err != nil {
				t.Fatal(err)
			}
			if snap.Schema.Table("public.invoices") == nil {
				t.Fatal("invoices missing from the schema")
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

// A role that can read the history but not the application's tables still
// captures the whole schema and every table's row count; only the column
// statistics of tables it can't read are missing.
func TestCaptureWithoutReadingTheTables(t *testing.T) {
	server := pgtest.Servers(t)[0]
	db := pgtest.NewDB(t, server)
	seed(t, db)
	reader := pgtest.CaptureURL(t, db)
	pgtest.Exec(t, db, "GRANT SELECT ON flyway_schema_history TO "+pgtest.CaptureRole)

	snap, err := capture.Run(context.Background(), pgtest.Connect(t, reader), capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolFlyway})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Schema.Table("public.invoices") == nil {
		t.Fatal("invoices missing from the schema")
	}
	if invoices := findStats(snap, "public.invoices"); invoices == nil || invoices.Rows != 1000 || len(invoices.Columns) != 0 {
		t.Fatalf("invoices stats = %+v, want its row count and no column statistics", invoices)
	}
	if got := capture.Applied(snap.History); !slices.Equal(got, []string{"1", "2"}) {
		t.Fatalf("Applied = %v", got)
	}
}

func TestPostDeployCaptureWaitsForTheDeployedVersion(t *testing.T) {
	server := pgtest.Servers(t)[0]
	db := pgtest.NewDB(t, server)
	pgtest.ExecFile(t, db, "../testdata/schemas/billing.sql")
	conn := pgtest.Connect(t, db)

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

// On Neon, which ignores the search_path Atlas connects with, Atlas's own
// advice is --revisions-schema public, so its history is public's
// atlas_schema_revisions. Capture finds it there without being told.
func TestCaptureFindsAtlasHistoryInPublic(t *testing.T) {
	server := pgtest.Servers(t)[0]
	db := pgtest.NewDB(t, server)
	pgtest.Exec(t, db, `
		CREATE TABLE customers (id bigint PRIMARY KEY, email text NOT NULL);
		-- As Atlas creates it with --revisions-schema public.
		CREATE TABLE atlas_schema_revisions (
		  version character varying NOT NULL PRIMARY KEY,
		  description character varying NOT NULL,
		  type bigint NOT NULL,
		  applied bigint NOT NULL,
		  total bigint NOT NULL,
		  executed_at timestamp with time zone NOT NULL,
		  execution_time bigint NOT NULL,
		  error text,
		  error_stmt text,
		  hash character varying NOT NULL,
		  partial_hashes jsonb,
		  operator_version character varying NOT NULL
		);
		INSERT INTO atlas_schema_revisions VALUES
		  ('20260901120000', 'init', 2, 3, 3, now(), 1000, NULL, NULL, 'h1', NULL, 'Atlas CLI v1.3.0'),
		  ('20260915120000', 'customers', 2, 1, 1, now(), 1000, NULL, NULL, 'h2', NULL, 'Atlas CLI v1.3.0');`)

	snap, err := capture.Run(context.Background(), pgtest.Connect(t, db), capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolAtlas})
	if err != nil {
		t.Fatal(err)
	}
	if snap.History.Table != "public.atlas_schema_revisions" {
		t.Errorf("history table = %q, want public.atlas_schema_revisions", snap.History.Table)
	}
	if got := capture.Applied(snap.History); !slices.Equal(got, []string{"20260901120000", "20260915120000"}) {
		t.Errorf("Applied = %v, want both migrations", got)
	}
}
