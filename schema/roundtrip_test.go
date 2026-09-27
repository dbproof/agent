package schema_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stratum-dev/agent/internal/pgtest"
	"github.com/stratum-dev/agent/schema"
)

// TestRoundTrip is the schema engine's acceptance test: inspecting a schema as
// the unprivileged capture role, restoring it into an empty database and
// inspecting that must give no differences.
func TestRoundTrip(t *testing.T) {
	for _, server := range pgtest.Servers(t) {
		for _, name := range []string{"coverage", "billing", "pagila"} {
			t.Run(server.Name+"/"+name, func(t *testing.T) {
				if name == "pagila" && server.Major < 18 {
					t.Skip("pagila.sql uses Postgres 18 features")
				}
				src := pgtest.NewDB(t, server)
				pgtest.ExecFile(t, src, "../testdata/schemas/"+name+".sql")

				start := time.Now()
				captured := inspect(t, pgtest.CaptureURL(t, src))
				t.Logf("inspected %d tables in %s", len(captured.Tables), time.Since(start))

				// The capture role sees exactly what a superuser sees.
				if changes := schema.Diff(inspect(t, src), captured, schema.DiffOptions{}); len(changes) > 0 {
					t.Fatalf("capture role sees a different schema than the owner:\n%s", describe(changes))
				}

				dst := pgtest.NewDB(t, server)
				restore(t, dst, schema.RestoreDDL(captured))
				restored := inspect(t, pgtest.CaptureURL(t, dst))
				if changes := schema.Diff(captured, restored, schema.DiffOptions{}); len(changes) > 0 {
					t.Fatalf("restore differs from the captured schema:\n%s", describe(changes))
				}

				// The JSON encoding keeps everything Diff compares.
				b, err := json.Marshal(captured)
				if err != nil {
					t.Fatal(err)
				}
				var decoded schema.Schema
				if err := json.Unmarshal(b, &decoded); err != nil {
					t.Fatal(err)
				}
				if changes := schema.Diff(captured, &decoded, schema.DiffOptions{}); len(changes) > 0 {
					t.Fatalf("JSON round trip lost data:\n%s", describe(changes))
				}
			})
		}
	}
}

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

func hasView(s *schema.Schema, id string) bool {
	for _, v := range s.Views {
		if v.ID() == id {
			return true
		}
	}
	return false
}

func inspect(t *testing.T, dbURL string) *schema.Schema {
	t.Helper()
	conn := pgtest.Connect(t, dbURL)
	s, err := schema.Inspect(context.Background(), conn, schema.Options{IgnoreGrantees: []string{pgtest.CaptureRole}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func restore(t *testing.T, dbURL string, stmts []string) {
	t.Helper()
	conn := pgtest.Connect(t, dbURL)
	for _, stmt := range stmts {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("restore statement failed: %v\n%s", err, stmt)
		}
	}
}

func describe(changes []schema.Change) string {
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(string(c.Op) + " " + string(c.Kind) + " " + c.ID)
		if len(c.Fields) > 0 {
			b.WriteString(" (" + strings.Join(c.Fields, ", ") + ")")
			before, _ := json.Marshal(c.Before)
			after, _ := json.Marshal(c.After)
			b.WriteString("\n    before: " + string(before) + "\n    after:  " + string(after))
		}
		b.WriteString("\n")
	}
	return b.String()
}
