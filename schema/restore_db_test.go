package schema_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/borovikovd/stratum-agent/internal/pgtest"
	"github.com/borovikovd/stratum-agent/schema"
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
