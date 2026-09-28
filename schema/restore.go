package schema

import (
	"slices"
)

// RestoreDDL returns the statements that recreate s in an empty database, in
// dependency order. Run them in one session, as a superuser, in order.
//
// Functions that don't touch relations are created before tables, with
// check_function_bodies off, so column defaults and checks can call them.
// Functions whose signature or SQL-standard body uses a relation are created
// with the views, after the tables.
func RestoreDDL(s *Schema) []string {
	out := []string{"SET check_function_bodies = off"}
	for _, n := range s.Namespaces {
		out = append(out, createSchemaSQL(n, true))
	}
	for _, e := range s.Extensions {
		out = append(out, createExtensionSQL(e))
	}
	for _, r := range s.Roles {
		out = append(out, createRoleSQL(r))
	}

	types := topoSort(s.Types, (*Type).ID, func(t *Type) []string { return t.DependsOn })
	for _, t := range types {
		out = append(out, createTypeSQL(t))
	}

	early, late := splitFunctions(s)
	for _, f := range topoSort(early, (*Function).ID, func(f *Function) []string { return f.DependsOn }) {
		out = append(out, f.Definition)
	}
	for _, t := range types {
		for _, c := range t.Checks {
			out = append(out, domainCheckSQL(t, c))
		}
	}

	for _, q := range s.Sequences {
		out = append(out, createSequenceSQL(q))
	}
	tables := topoSort(s.Tables, (*Table).ID, func(t *Table) []string {
		if t.PartitionOf == "" {
			return nil
		}
		return []string{t.PartitionOf}
	})
	for _, t := range tables {
		out = append(out, createTableSQL(t, s.Table(t.PartitionOf))...)
	}
	for _, q := range s.Sequences {
		if q.OwnedBy != "" {
			out = append(out, "ALTER SEQUENCE "+q.ID()+" OWNED BY "+q.OwnedBy)
		}
	}

	// Late functions and views depend on each other in any order.
	type lateObject struct {
		id, sql string
		deps    []string
	}
	var objs []lateObject
	for _, f := range late {
		objs = append(objs, lateObject{f.ID(), f.Definition, f.DependsOn})
	}
	for _, v := range s.Views {
		objs = append(objs, lateObject{v.ID(), createViewSQL(v, false), v.DependsOn})
	}
	for _, o := range topoSort(objs, func(o *lateObject) string { return o.id }, func(o *lateObject) []string { return o.deps }) {
		out = append(out, o.sql)
	}

	// Keys and checks first, so foreign keys find their unique indexes.
	for _, c := range s.Constraints {
		if c.Type != ConstraintForeignKey {
			out = append(out, addConstraintSQL(c))
		}
	}
	for _, i := range s.Indexes {
		out = append(out, i.Definition)
	}
	for _, c := range s.Constraints {
		if c.Type == ConstraintForeignKey {
			out = append(out, addConstraintSQL(c))
		}
	}
	for _, t := range s.Triggers {
		out = append(out, triggerSQL(t)...)
	}
	for _, t := range tables {
		out = append(out, rlsSQL(t)...)
	}
	for _, p := range s.Policies {
		out = append(out, createPolicySQL(p))
	}
	return append(out, grantsSQL(s)...)
}

// splitFunctions separates functions that can be created before tables from
// those that need relations, directly or through another such function.
func splitFunctions(s *Schema) (early, late []Function) {
	relations := map[string]bool{}
	for _, t := range s.Tables {
		relations[t.ID()] = true
	}
	for _, v := range s.Views {
		relations[v.ID()] = true
	}
	for changed := true; changed; {
		changed = false
		for _, f := range s.Functions {
			if relations[f.ID()] {
				continue
			}
			if slices.ContainsFunc(f.DependsOn, func(d string) bool { return relations[d] }) {
				relations[f.ID()] = true
				changed = true
			}
		}
	}
	for _, f := range s.Functions {
		if relations[f.ID()] {
			late = append(late, f)
		} else {
			early = append(early, f)
		}
	}
	return early, late
}

// grantsSQL reproduces privileges exactly. PUBLIC holds some privileges by
// default (on functions, types and the public schema), so those are revoked
// first wherever the captured schema doesn't grant them.
func grantsSQL(s *Schema) []string {
	var out []string
	granted := map[string]bool{}
	for _, g := range s.Grants {
		if g.Grantee == "PUBLIC" {
			granted[string(g.ObjectKind)+" "+g.Object] = true
		}
	}
	for _, n := range s.Namespaces {
		out = append(out, "REVOKE ALL ON SCHEMA "+QuoteIdent(n.Name)+" FROM PUBLIC")
	}
	for _, f := range s.Functions {
		if !granted["function "+f.ID()] {
			out = append(out, "REVOKE ALL ON ROUTINE "+f.ID()+" FROM PUBLIC")
		}
	}
	for _, t := range s.Types {
		if !granted["type "+t.ID()] {
			out = append(out, "REVOKE ALL ON TYPE "+t.ID()+" FROM PUBLIC")
		}
	}
	for _, g := range s.Grants {
		out = append(out, grantSQL(g))
	}
	return out
}

// topoSort orders items so each comes after the items it depends on. Items
// keep their input order where dependencies allow; dependencies on unknown
// identities are ignored, and a cycle falls back to input order.
func topoSort[T any](items []T, id func(*T) string, deps func(*T) []string) []T {
	index := make(map[string]int, len(items))
	for i := range items {
		index[id(&items[i])] = i
	}
	out := make([]T, 0, len(items))
	state := make([]int, len(items)) // 0 new, 1 visiting, 2 done
	var visit func(i int)
	visit = func(i int) {
		if state[i] != 0 {
			return
		}
		state[i] = 1
		for _, d := range deps(&items[i]) {
			if j, ok := index[d]; ok && j != i {
				visit(j)
			}
		}
		state[i] = 2
		out = append(out, items[i])
	}
	for i := range items {
		visit(i)
	}
	return out
}
