package schema

import (
	"reflect"
	"testing"
)

func TestDescribe(t *testing.T) {
	invoices := "public.invoices"
	creditNotes := &Table{Schema: "public", Name: "credit_notes", Columns: []Column{{Name: "id", Type: "bigint"}, {Name: "invoice_id", Type: "bigint"}}}
	fk := &Constraint{Table: "public.credit_notes", Name: "credit_notes_invoice_id_fkey", Type: ConstraintForeignKey, Columns: []string{"invoice_id"}, RefTable: invoices, RefColumns: []string{"id"}}
	changes := []Change{
		{Op: OpAdd, Kind: KindTable, ID: "public.credit_notes", After: &Object{Table: creditNotes}},
		{Op: OpAlter, Kind: KindColumn, ID: invoices + ".due_date", Table: invoices, Fields: []string{"not_null"},
			Before: &Object{Column: &Column{Name: "due_date", Type: "date"}}, After: &Object{Column: &Column{Name: "due_date", Type: "date", NotNull: true}}},
		{Op: OpAlter, Kind: KindColumn, ID: invoices + ".number", Table: invoices, Fields: []string{"type"},
			Before: &Object{Column: &Column{Name: "number", Type: "character varying(20)", NotNull: true}}, After: &Object{Column: &Column{Name: "number", Type: "character varying(32)", NotNull: true}}},
		{Op: OpAdd, Kind: KindIndex, ID: invoices + ".invoices_due_date_idx", Table: invoices, After: &Object{Index: &Index{Table: invoices, Name: "invoices_due_date_idx", Columns: []string{"due_date"}}}},
		{Op: OpDrop, Kind: KindColumn, ID: invoices + ".legacy_discount", Table: invoices, Before: &Object{Column: &Column{Name: "legacy_discount", Type: "numeric(10,2)"}}},
		{Op: OpAdd, Kind: KindConstraint, ID: fk.ID(), Table: fk.Table, After: &Object{Constraint: fk}},
		{Op: OpAdd, Kind: KindColumn, ID: "public.customers.tax_id", Table: "public.customers", After: &Object{Column: &Column{Name: "tax_id", Type: "character varying(32)"}}},
	}
	want := []Line{
		{Op: "+", Text: "TABLE credit_notes (id, invoice_id)"},
		{Op: "+", Text: "FOREIGN KEY credit_notes_invoice_id_fkey (invoice_id) → invoices(id)", Indent: 1},
		{Op: " ", Text: "TABLE invoices"},
		{Op: "~", Text: "due_date date NULL → NOT NULL", Indent: 1},
		{Op: "~", Text: "number varchar(20) → varchar(32)", Indent: 1},
		{Op: "+", Text: "INDEX invoices_due_date_idx (due_date)", Indent: 1},
		{Op: "-", Text: "legacy_discount numeric(10,2) NULL", Indent: 1},
		{Op: " ", Text: "TABLE customers"},
		{Op: "+", Text: "tax_id varchar(32) NULL", Indent: 1},
	}
	if got := Describe(changes); !reflect.DeepEqual(got, want) {
		t.Fatalf("Describe =\n%v\nwant\n%v", got, want)
	}
}
