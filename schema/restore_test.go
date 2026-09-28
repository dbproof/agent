package schema

import (
	"slices"
	"testing"
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
