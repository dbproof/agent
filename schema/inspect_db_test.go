package schema_test

import (
	"context"
	"testing"

	"github.com/dbproof/agent/internal/pgtest"
	"github.com/dbproof/agent/schema"
)

func TestInspectExclusions(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		t.Run(server.Name, func(t *testing.T) {
			db := pgtest.NewDB(t, server)
			pgtest.ExecFile(t, db, "../testdata/schemas/coverage.sql")
			conn := pgtest.Connect(t, pgtest.CaptureURL(t, db))
			s, err := schema.Inspect(context.Background(), conn, schema.Options{
				Exclude:        []string{"audit.*", "app.customers"},
				IgnoreGrantees: []string{pgtest.CaptureRole},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range s.Namespaces {
				if n.Name == "audit" {
					t.Error("excluded schema audit was inspected")
				}
			}
			// Everything that needs customers goes with it: the view that joins
			// it, the view on that view, the function taking its row type, and
			// the foreign key from invoices.
			for _, id := range []string{"app.customers", "app.open_invoices", "app.open_invoice_count"} {
				if s.Table(id) != nil || hasView(s, id) {
					t.Errorf("%s should be excluded", id)
				}
			}
			for _, f := range s.Functions {
				if f.Name == "customer_label" {
					t.Error("function using the excluded table's row type should be excluded")
				}
			}
			for _, c := range s.Constraints {
				if c.RefTable == "app.customers" || c.Table == "app.customers" {
					t.Errorf("constraint %s should be excluded", c.ID())
				}
			}
			if s.Table("app.invoices") == nil {
				t.Error("app.invoices should stay")
			}
			// What's left must still restore.
			dst := pgtest.NewDB(t, server)
			restore(t, dst, schema.RestoreDDL(s))
		})
	}
}
