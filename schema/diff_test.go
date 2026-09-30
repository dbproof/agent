package schema

import (
	"slices"
	"testing"
)

func TestDiffColumnFieldsAndOrder(t *testing.T) {
	table := func(cols ...Column) Table { return Table{Schema: "public", Name: "invoices", Columns: cols} }
	a := &Schema{Tables: []Table{table(
		Column{Name: "id", Type: "bigint", NotNull: true},
		Column{Name: "due_date", Type: "date"},
		Column{Name: "legacy", Type: "numeric(10,2)"},
	)}}
	b := &Schema{Tables: []Table{table(
		Column{Name: "due_date", Type: "date", NotNull: true},
		Column{Name: "id", Type: "bigint", NotNull: true},
		Column{Name: "tax_id", Type: "character varying(32)"},
	)}, Indexes: []Index{{Table: "public.invoices", Name: "invoices_due_date_idx", Definition: "CREATE INDEX ..."}}}

	changes := Diff(a, b, DiffOptions{})
	var got []string
	for _, c := range changes {
		got = append(got, string(c.Op)+" "+string(c.Kind)+" "+c.ID)
	}
	want := []string{
		"alter column public.invoices.due_date",
		"drop column public.invoices.legacy",
		"add column public.invoices.tax_id",
		"add index public.invoices.invoices_due_date_idx",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Diff =\n%q\nwant\n%q", got, want)
	}
	if f := changes[0].Fields; !slices.Equal(f, []string{"not_null"}) {
		t.Errorf("due_date fields = %v, want [not_null]", f)
	}
	for _, c := range changes[:3] {
		if c.Table != "public.invoices" {
			t.Errorf("%s: Table = %q, want public.invoices", c.ID, c.Table)
		}
	}
}

func TestDiffIgnoreTables(t *testing.T) {
	history := Table{Schema: "public", Name: "flyway_schema_history", Columns: []Column{{Name: "version", Type: "text"}}}
	a := &Schema{}
	b := &Schema{Tables: []Table{history}, Grants: []Grant{{ObjectKind: ObjectTable, Object: "public.flyway_schema_history", Grantee: "dbproof_capture", Privilege: "SELECT"}}}
	if c := Diff(a, b, DiffOptions{IgnoreTables: []string{"public.flyway_schema_history"}}); len(c) != 0 {
		t.Fatalf("Diff with ignored table = %v, want none", c)
	}
}

func TestDiffComparesViewTextOnlyWithinAMajor(t *testing.T) {
	view := func(major int32, def string) *Schema {
		return &Schema{ServerVersionNum: major * 10000, Views: []View{{Schema: "app", Name: "draft_invoices", Definition: def}}}
	}
	qualified := "SELECT invoices.id\n   FROM app.invoices"
	bare := "SELECT id\n   FROM app.invoices"

	if got := Diff(view(15, qualified), view(15, bare), DiffOptions{}); len(got) != 1 || got[0].Fields[0] != "definition" {
		t.Errorf("same major: %+v, want the definition change", got)
	}
	if got := Diff(view(15, qualified), view(16, bare), DiffOptions{}); len(got) != 0 {
		t.Errorf("across majors: %+v, want no change", got)
	}
	if got := Diff(view(15, qualified), &Schema{ServerVersionNum: 160000}, DiffOptions{}); len(got) != 1 || got[0].Op != OpDrop {
		t.Errorf("a view dropped across majors: %+v, want the drop", got)
	}
}
