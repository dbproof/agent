package schema

import "testing"

func TestHoldsAndUndone(t *testing.T) {
	before := &Schema{Tables: []Table{{Schema: "public", Name: "customers", Columns: []Column{{Name: "id", Type: "bigint"}}}}}
	after := &Schema{Tables: []Table{{Schema: "public", Name: "customers", Columns: []Column{{Name: "id", Type: "bigint"}, {Name: "tax_id", Type: "text"}}}}}
	changes := Diff(before, after, DiffOptions{})
	if len(changes) != 1 {
		t.Fatalf("changes = %+v", changes)
	}
	c := changes[0]
	if !after.Holds(c) || after.Undone(c) {
		t.Error("after should hold the change")
	}
	if before.Holds(c) || !before.Undone(c) {
		t.Error("before should have the change undone")
	}

	widened := &Schema{Tables: []Table{{Schema: "public", Name: "customers", Columns: []Column{{Name: "id", Type: "bigint"}, {Name: "tax_id", Type: "character varying(32)"}}}}}
	alter := Diff(after, widened, DiffOptions{})[0]
	if !widened.Holds(alter) || !after.Undone(alter) || after.Holds(alter) {
		t.Error("alter: widened should hold it and after should have it undone")
	}
}
