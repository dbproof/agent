package schema_test

import (
	"testing"

	"github.com/borovikovd/dbproof-agent/internal/pgtest"
	"github.com/borovikovd/dbproof-agent/schema"
)

// TestChangeDDL applies the DDL for a diff to a restore of the "before"
// schema and expects the "after" schema, and the reverse DDL to a restore of
// "after" and expects "before".
func TestChangeDDL(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			db := pgtest.NewDB(t, server)
			pgtest.ExecFile(t, db, "../testdata/schemas/coverage.sql")
			before := inspect(t, db)
			pgtest.Exec(t, db, `
				ALTER TABLE app.customers ADD COLUMN phone text DEFAULT 'none' NOT NULL;
				ALTER TABLE app.invoices ALTER COLUMN due_date SET NOT NULL;
				ALTER TABLE app.invoices ALTER COLUMN legacy_discount TYPE numeric(12, 2);
				ALTER TABLE app.invoices ALTER COLUMN status SET DEFAULT 'open';
				ALTER TABLE app.invoices DROP COLUMN updated_at CASCADE;
				DROP INDEX app.invoices_open_idx;
				CREATE INDEX customers_tax_id_idx ON app.customers (tax_id);
				ALTER TABLE app.bookings DROP CONSTRAINT bookings_no_overlap;
				CREATE OR REPLACE VIEW app.open_invoice_count AS SELECT count(*) AS n FROM app.open_invoices WHERE id > 0;
				ALTER TABLE app.invoices ENABLE TRIGGER invoices_touch_insert;
				GRANT SELECT ON app.bookings TO app_reader;
				REVOKE SELECT (email) ON app.customers FROM app_reader;
				CREATE TABLE app.notes (id int PRIMARY KEY, body text);
				CREATE INDEX notes_body_idx ON app.notes (body);
				DROP TABLE audit.log;
				ALTER DOMAIN app.positive_cents SET DEFAULT 1;
				ALTER SEQUENCE app.invoice_number_seq INCREMENT BY 2;
				ALTER TABLE app.customers NO FORCE ROW LEVEL SECURITY;
				DROP POLICY customers_reader ON app.customers;
				CREATE POLICY customers_reader ON app.customers FOR SELECT TO app_reader USING (id < 1000);
				CREATE OR REPLACE FUNCTION app.concat_step(text, text) RETURNS text LANGUAGE sql IMMUTABLE AS $$ SELECT $1 || $2 $$;
				CREATE TYPE app.priority AS ENUM ('low', 'high');`)
			after := inspect(t, db)
			changes := schema.Diff(before, after, schema.DiffOptions{})
			if len(changes) == 0 {
				t.Fatal("no changes to test")
			}

			forward := pgtest.NewDB(t, server)
			restore(t, forward, schema.RestoreDDL(before))
			restore(t, forward, schema.ChangeDDL(changes, schema.DDLOptions{}))
			if left := schema.Diff(after, inspect(t, forward), schema.DiffOptions{}); len(left) > 0 {
				t.Errorf("forward DDL left differences:\n%s", describe(left))
			}

			backward := pgtest.NewDB(t, server)
			restore(t, backward, schema.RestoreDDL(after))
			restore(t, backward, schema.ChangeDDL(changes, schema.DDLOptions{Reverse: true}))
			if left := schema.Diff(before, inspect(t, backward), schema.DiffOptions{}); len(left) > 0 {
				t.Errorf("reverse DDL left differences:\n%s", describe(left))
			}

			// Guarded DDL can run twice.
			guarded := pgtest.NewDB(t, server)
			restore(t, guarded, schema.RestoreDDL(before))
			addsOnly := addedColumnsAndIndexes(before, changes)
			restore(t, guarded, schema.ChangeDDL(addsOnly, schema.DDLOptions{Guarded: true}))
			restore(t, guarded, schema.ChangeDDL(addsOnly, schema.DDLOptions{Guarded: true}))
		})
	}
}

// addedColumnsAndIndexes keeps the column and index additions on tables that
// already exist in before.
func addedColumnsAndIndexes(before *schema.Schema, changes []schema.Change) []schema.Change {
	var out []schema.Change
	for _, c := range changes {
		if c.Op == schema.OpAdd && (c.Kind == schema.KindColumn || c.Kind == schema.KindIndex) && before.Table(c.Table) != nil {
			out = append(out, c)
		}
	}
	return out
}
