package schema

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

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

// Diffing and restoring cost the same per table at any schema size: both run
// on every capture and check, and real schemas reach tens of thousands of
// tables. Quadratic code takes tens of seconds here.
func TestLargeSchemasDiffAndRestoreQuickly(t *testing.T) {
	s := &Schema{}
	for i := range 20000 {
		s.Tables = append(s.Tables, Table{Schema: "public", Name: fmt.Sprintf("table_%d", i), Columns: []Column{{Name: "id", Type: "bigint"}}})
	}
	changed := &Schema{Tables: slices.Clone(s.Tables)}
	changed.Tables[0].Columns = []Column{{Name: "id", Type: "integer"}}
	start := time.Now()
	RestoreDDL(s)
	Diff(s, changed, DiffOptions{})
	Diff(&Schema{}, s, DiffOptions{})
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("restoring and diffing 20,000 tables took %v", took)
	}
}
