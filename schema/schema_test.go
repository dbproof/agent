package schema

import (
	"slices"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"invoices":     "invoices",
		"due_date2":    "due_date2",
		"Display Name": `"Display Name"`,
		"user":         `"user"`,
		"order":        `"order"`,
		"2fa":          `"2fa"`,
		"a$b":          `"a$b"`,
		`say "hi"`:     `"say ""hi"""`,
		"bıgınt":       `"bıgınt"`,
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCutLast(t *testing.T) {
	cases := []struct{ in, before, after string }{
		{"public.invoices.due_date", "public.invoices", "due_date"},
		{`public."a.b".c`, `public."a.b"`, "c"},
		{`public.t."x.y"`, "public.t", `"x.y"`},
		{"solo", "", "solo"},
	}
	for _, c := range cases {
		before, after, _ := cutLast(c.in)
		if before != c.before || after != c.after {
			t.Errorf("cutLast(%q) = %q, %q; want %q, %q", c.in, before, after, c.before, c.after)
		}
	}
}

func TestTopoSort(t *testing.T) {
	type node struct {
		id   string
		deps []string
	}
	nodes := []node{
		{"c", []string{"b"}},
		{"a", nil},
		{"b", []string{"a", "unknown"}},
		{"x", []string{"y"}},
		{"y", []string{"x"}},
	}
	var got []string
	for _, n := range topoSort(nodes, func(n *node) string { return n.id }, func(n *node) []string { return n.deps }) {
		got = append(got, n.id)
	}
	want := []string{"a", "b", "c", "y", "x"}
	if !slices.Equal(got, want) {
		t.Fatalf("topoSort = %v, want %v", got, want)
	}
}

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
	b := &Schema{Tables: []Table{history}, Grants: []Grant{{ObjectKind: ObjectTable, Object: "public.flyway_schema_history", Grantee: "stratum_capture", Privilege: "SELECT"}}}
	if c := Diff(a, b, DiffOptions{IgnoreTables: []string{"public.flyway_schema_history"}}); len(c) != 0 {
		t.Fatalf("Diff with ignored table = %v, want none", c)
	}
}

func TestChangeDDLOrdersDropsBeforeAdds(t *testing.T) {
	col := &Column{Name: "tax_id", Type: "text"}
	idx := &Index{Table: "public.customers", Name: "customers_tax_id_idx", Definition: "CREATE INDEX customers_tax_id_idx ON public.customers USING btree (tax_id)"}
	fk := &Constraint{Table: "public.invoices", Name: "invoices_customer_fkey", Type: ConstraintForeignKey, Definition: "FOREIGN KEY (customer_id) REFERENCES public.customers(id)"}
	changes := []Change{
		{Op: OpAdd, Kind: KindConstraint, ID: fk.ID(), Table: fk.Table, After: &Object{Constraint: fk}},
		{Op: OpAdd, Kind: KindIndex, ID: idx.ID(), Table: idx.Table, After: &Object{Index: idx}},
		{Op: OpDrop, Kind: KindColumn, ID: "public.customers.tax_id", Table: "public.customers", Before: &Object{Column: col}},
	}
	got := ChangeDDL(changes, DDLOptions{Guarded: true})
	want := []string{
		"ALTER TABLE public.customers DROP COLUMN IF EXISTS tax_id",
		"CREATE INDEX IF NOT EXISTS customers_tax_id_idx ON public.customers USING btree (tax_id)",
		"ALTER TABLE public.invoices ADD CONSTRAINT invoices_customer_fkey FOREIGN KEY (customer_id) REFERENCES public.customers(id)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ChangeDDL =\n%q\nwant\n%q", got, want)
	}
	rev := ChangeDDL(changes, DDLOptions{Reverse: true})
	wantRev := []string{
		"ALTER TABLE public.invoices DROP CONSTRAINT invoices_customer_fkey",
		"DROP INDEX public.customers_tax_id_idx",
		"ALTER TABLE public.customers ADD COLUMN tax_id text",
	}
	if !slices.Equal(rev, wantRev) {
		t.Fatalf("reverse ChangeDDL =\n%q\nwant\n%q", rev, wantRev)
	}
}
