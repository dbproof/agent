package snapshot

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stratum-dev/agent/schema"
)

func TestEncodeDecode(t *testing.T) {
	v := "1"
	in := &Snapshot{
		FormatVersion: FormatVersion,
		CapturedAt:    time.Date(2026, 9, 27, 6, 0, 0, 0, time.UTC),
		Kind:          KindScheduled,
		AgentVersion:  "v0.1.0",
		Schema: &schema.Schema{
			ServerVersionNum: 150006,
			Tables:           []schema.Table{{Schema: "public", Name: "invoices", Columns: []schema.Column{{Name: "due_date", Type: "date"}}}},
		},
		Stats:      []TableStats{{Table: "public.invoices", Rows: 12.4e6, Columns: []ColumnStats{{Column: "due_date", NullFrac: 0.0031, NDistinct: -0.2, AvgWidth: 4}}}},
		History:    &History{Tool: ToolFlyway, Table: "public.flyway_schema_history", Columns: []string{"version", "script"}, Rows: [][]*string{{&v, nil}}},
		Exclusions: []string{"audit.*"},
	}
	b, err := Encode(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip changed the snapshot:\n in: %+v\nout: %+v", in, out)
	}
	if got := out.History.Value(out.History.Rows[0], "version"); got == nil || *got != "1" {
		t.Fatalf("History.Value(version) = %v, want 1", got)
	}
}

func TestDecodeRejectsNewerFormat(t *testing.T) {
	b, err := Encode(&Snapshot{FormatVersion: FormatVersion + 1, Schema: &schema.Schema{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(b); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Decode newer format: err = %v, want not supported", err)
	}
}
