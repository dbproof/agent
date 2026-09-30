package schema

import (
	"slices"
	"testing"
)

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
		"DO $dbproof$ BEGIN IF NOT EXISTS (SELECT FROM pg_catalog.pg_constraint WHERE conrelid = 'public.invoices'::regclass AND conname = 'invoices_customer_fkey') " +
			"THEN ALTER TABLE public.invoices ADD CONSTRAINT invoices_customer_fkey FOREIGN KEY (customer_id) REFERENCES public.customers(id); END IF; END $dbproof$",
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
