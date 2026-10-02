package schema

import (
	"cmp"
	"slices"
	"strings"
)

// applyExclusions removes relations matching "schema.table" patterns, and
// everything that can't exist without them: partitions, dependent views and
// functions, sub-objects, foreign keys that reference them, owned sequences
// and grants.
func applyExclusions(s *Schema, patterns []string) {
	excluded := map[string]bool{}
	for _, p := range patterns {
		schema, name, ok := strings.Cut(p, ".")
		if !ok || name == "*" {
			continue
		}
		excluded[Qualified(schema, name)] = true
	}
	if len(excluded) == 0 {
		return
	}
	for changed := true; changed; {
		changed = false
		mark := func(id string, deps ...string) {
			if excluded[id] {
				return
			}
			for _, d := range deps {
				if excluded[d] {
					excluded[id] = true
					changed = true
					return
				}
			}
		}
		for _, t := range s.Tables {
			mark(t.ID(), t.PartitionOf)
		}
		for _, v := range s.Views {
			mark(v.ID(), v.DependsOn...)
		}
		for _, f := range s.Functions {
			mark(f.ID(), f.DependsOn...)
		}
	}
	s.Tables = slices.DeleteFunc(s.Tables, func(t Table) bool { return excluded[t.ID()] })
	s.Views = slices.DeleteFunc(s.Views, func(v View) bool { return excluded[v.ID()] })
	s.Functions = slices.DeleteFunc(s.Functions, func(f Function) bool { return excluded[f.ID()] })
	s.Constraints = slices.DeleteFunc(s.Constraints, func(c Constraint) bool { return excluded[c.Table] || excluded[c.RefTable] })
	s.Indexes = slices.DeleteFunc(s.Indexes, func(i Index) bool { return excluded[i.Table] })
	s.Triggers = slices.DeleteFunc(s.Triggers, func(t Trigger) bool { return excluded[t.Table] })
	s.Policies = slices.DeleteFunc(s.Policies, func(p Policy) bool { return excluded[p.Table] })
	s.Sequences = slices.DeleteFunc(s.Sequences, func(q Sequence) bool {
		table, _ := cutLast(q.OwnedBy)
		return excluded[table]
	})
	s.Grants = slices.DeleteFunc(s.Grants, func(g Grant) bool {
		if g.ObjectKind == ObjectColumn {
			table, _ := cutLast(g.Object)
			return excluded[table]
		}
		return excluded[g.Object]
	})
}

// cutLast splits "a.b.c" into "a.b" and "c". Quoted names containing dots
// are split on the last unquoted dot.
func cutLast(id string) (before, after string) {
	inQuote := false
	last := -1
	for i, r := range id {
		switch {
		case r == '"':
			inQuote = !inQuote
		case r == '.' && !inQuote:
			last = i
		}
	}
	if last < 0 {
		return "", id
	}
	return id[:last], id[last+1:]
}

// Normalize sorts every list by identity so that equal schemas encode to
// equal bytes. Column order is kept: it's part of the table.
func Normalize(s *Schema) {
	slices.SortFunc(s.Extensions, func(a, b Extension) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(s.Namespaces, func(a, b Namespace) int { return cmp.Compare(a.Name, b.Name) })
	slices.Sort(s.Roles)
	sortByID(s.Types, (*Type).ID)
	sortByID(s.Sequences, (*Sequence).ID)
	sortByID(s.Tables, (*Table).ID)
	sortByID(s.Views, (*View).ID)
	sortByID(s.Functions, (*Function).ID)
	sortByID(s.Constraints, (*Constraint).ID)
	sortByID(s.Indexes, (*Index).ID)
	sortByID(s.Triggers, (*Trigger).ID)
	sortByID(s.Policies, (*Policy).ID)
	sortByID(s.Grants, (*Grant).ID)
	for i := range s.Types {
		slices.Sort(s.Types[i].DependsOn)
		slices.SortFunc(s.Types[i].Checks, func(a, b Check) int { return cmp.Compare(a.Name, b.Name) })
	}
	for i := range s.Tables {
		slices.Sort(s.Tables[i].Options)
	}
	for i := range s.Views {
		slices.Sort(s.Views[i].Options)
		slices.Sort(s.Views[i].DependsOn)
	}
	for i := range s.Functions {
		slices.Sort(s.Functions[i].DependsOn)
	}
	for i := range s.Policies {
		slices.Sort(s.Policies[i].Roles)
	}
}

func sortByID[T any](list []T, id func(*T) string) {
	slices.SortFunc(list, func(a, b T) int { return cmp.Compare(id(&a), id(&b)) })
}
