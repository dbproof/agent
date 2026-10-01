package schema_test

import (
	"slices"
	"testing"

	"github.com/dbproof/agent/internal/pgtest"
	"github.com/dbproof/agent/schema"
)

// TestDiffDetectsChanges makes one change of each kind against a real
// database and checks Diff reports exactly those.
func TestDiffDetectsChanges(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			db := pgtest.NewDB(t, server)
			pgtest.ExecFile(t, db, "../testdata/schemas/coverage.sql")
			capture := pgtest.CaptureURL(t, db)
			before := inspect(t, capture)

			pgtest.Exec(t, db, `
				ALTER TABLE app.customers ADD COLUMN phone text;
				ALTER TABLE app.invoices ALTER COLUMN due_date SET NOT NULL;
				ALTER TABLE app.invoices ALTER COLUMN legacy_discount TYPE numeric(12, 2);
				ALTER TABLE app.invoices DROP COLUMN updated_at CASCADE;
				DROP INDEX app.invoices_open_idx;
				CREATE INDEX customers_tax_id_idx ON app.customers (tax_id);
				ALTER TYPE app.status ADD VALUE 'refunded';
				CREATE OR REPLACE VIEW app.open_invoice_count AS SELECT count(*) AS n, 1 AS one FROM app.open_invoices;
				ALTER TABLE app.invoices ENABLE TRIGGER invoices_touch_insert;
				GRANT SELECT ON app.bookings TO app_reader;
				CREATE TABLE app.notes (id int PRIMARY KEY, body text);
				DROP TABLE audit.log;`)
			after := inspect(t, capture)

			got := map[string]bool{}
			for _, c := range schema.Diff(before, after, schema.DiffOptions{}) {
				got[string(c.Op)+" "+string(c.Kind)+" "+c.ID] = true
			}
			want := []string{
				"add column app.customers.phone",
				"alter column app.invoices.due_date",
				"alter column app.invoices.legacy_discount",
				"drop column app.invoices.updated_at",
				"drop index app.invoices.invoices_open_idx",
				"add index app.customers.customers_tax_id_idx",
				"alter type app.status",
				"alter view app.open_invoice_count",
				"alter trigger app.invoices.invoices_touch_insert",
				"add grant table app.bookings SELECT app_reader",
				"add table app.notes",
				"add constraint app.notes.notes_pkey",
				"drop table audit.log",
			}
			for _, w := range want {
				if !got[w] {
					t.Errorf("missing change %q", w)
				}
				delete(got, w)
			}
			// Nothing else may appear.
			var extra []string
			for k := range got {
				extra = append(extra, k)
			}
			slices.Sort(extra)
			if len(extra) > 0 {
				t.Errorf("unexpected changes: %q", extra)
			}
		})
	}
}

// The same schema captured on different Postgres majors diffs to nothing,
// so an upgrade isn't drift. Two differences are allowed: they come from
// creating each database fresh, and pg_upgrade keeps them as they were.
func TestSameSchemaAcrossMajors(t *testing.T) {
	servers := pgtest.Servers(t)
	if len(servers) < 2 {
		t.Skip("needs two Postgres majors")
	}
	fresh := func(c schema.Change) bool {
		// A new database gets the extension's newest version.
		if c.Kind == schema.KindExtension && slices.Equal(c.Fields, []string{"version"}) {
			return true
		}
		// Postgres 15 stopped granting CREATE on public to new databases.
		g := c.Before
		if g == nil {
			g = c.After
		}
		return c.Kind == schema.KindGrant && g.Grant.Object == "public" && g.Grant.Privilege == "CREATE" && g.Grant.Grantee == "PUBLIC"
	}
	for _, name := range []string{"coverage", "billing"} {
		var snaps []*schema.Schema
		for _, s := range servers {
			db := pgtest.NewDB(t, s)
			pgtest.ExecFile(t, db, "../testdata/schemas/"+name+".sql")
			snaps = append(snaps, inspect(t, pgtest.CaptureURL(t, db)))
		}
		for i := 1; i < len(snaps); i++ {
			changes := slices.DeleteFunc(schema.Diff(snaps[0], snaps[i], schema.DiffOptions{}), fresh)
			if len(changes) > 0 {
				t.Errorf("%s from %s to %s:\n%s", name, servers[0].Name, servers[i].Name, describe(changes))
			}
		}
	}
}
