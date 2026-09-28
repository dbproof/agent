package schema

import (
	"cmp"
	"reflect"
	"slices"
)

// Op is what happened to an object between two schemas.
type Op string

const (
	OpAdd   Op = "add"
	OpDrop  Op = "drop"
	OpAlter Op = "alter"
)

// Kind is the kind of object a Change is about.
type Kind string

const (
	KindSchema     Kind = "schema"
	KindExtension  Kind = "extension"
	KindType       Kind = "type"
	KindSequence   Kind = "sequence"
	KindTable      Kind = "table"
	KindColumn     Kind = "column"
	KindConstraint Kind = "constraint"
	KindIndex      Kind = "index"
	KindView       Kind = "view"
	KindFunc       Kind = "function"
	KindTrigger    Kind = "trigger"
	KindPolicy     Kind = "policy"
	KindGrant      Kind = "grant"
)

// kindOrder is the order changes are listed and applied in.
var kindOrder = []Kind{
	KindSchema, KindExtension, KindType, KindSequence, KindTable, KindColumn, KindConstraint,
	KindIndex, KindView, KindFunc, KindTrigger, KindPolicy, KindGrant,
}

// Change is one difference between two schemas. For an alter, Fields names
// what changed; Before and After carry the whole object on each side.
type Change struct {
	Op   Op     `json:"op"`
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
	// Table is the owning table or view of a column, constraint, index,
	// trigger or policy.
	Table  string   `json:"table,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Before *Object  `json:"before,omitempty"`
	After  *Object  `json:"after,omitempty"`
}

// Object holds exactly one schema object.
type Object struct {
	Namespace  *Namespace  `json:"namespace,omitempty"`
	Extension  *Extension  `json:"extension,omitempty"`
	Type       *Type       `json:"type,omitempty"`
	Sequence   *Sequence   `json:"sequence,omitempty"`
	Table      *Table      `json:"table,omitempty"`
	Column     *Column     `json:"column,omitempty"`
	Constraint *Constraint `json:"constraint,omitempty"`
	Index      *Index      `json:"index,omitempty"`
	View       *View       `json:"view,omitempty"`
	Function   *Function   `json:"function,omitempty"`
	Trigger    *Trigger    `json:"trigger,omitempty"`
	Policy     *Policy     `json:"policy,omitempty"`
	Grant      *Grant      `json:"grant,omitempty"`
}

// DiffOptions tunes what Diff compares.
type DiffOptions struct {
	// IgnoreTables lists table identities to leave out entirely, such as a
	// migration tool's history table.
	IgnoreTables []string
	// IgnoreExtensionVersions skips extension version changes, e.g. when
	// comparing a restore with the snapshot it came from.
	IgnoreExtensionVersions bool
}

// Diff lists what changed from a to b. It needs no database: both sides are
// compared structurally, by identity, ignoring the order of objects.
func Diff(a, b *Schema, opts DiffOptions) []Change {
	a, b = withoutTables(a, opts.IgnoreTables), withoutTables(b, opts.IgnoreTables)
	// Postgres majors render view definitions differently (16 stopped
	// qualifying columns with their table), so across an upgrade the text
	// can't be compared; everything else about a view still is.
	sameMajor := a.ServerVersionNum/10000 == b.ServerVersionNum/10000
	d := &differ{}

	diffList(d, KindSchema, a.Namespaces, b.Namespaces, func(n *Namespace) string { return QuoteIdent(n.Name) },
		func(n *Namespace) *Object { return &Object{Namespace: n} }, nil)
	diffList(d, KindExtension, a.Extensions, b.Extensions, func(e *Extension) string { return QuoteIdent(e.Name) },
		func(e *Extension) *Object { return &Object{Extension: e} },
		func(x, y *Extension) []string {
			f := fields(map[string]bool{"schema": x.Schema != y.Schema, "version": x.Version != y.Version && !opts.IgnoreExtensionVersions})
			return f
		})
	diffList(d, KindType, a.Types, b.Types, (*Type).ID, func(t *Type) *Object { return &Object{Type: t} },
		func(x, y *Type) []string {
			return fields(map[string]bool{
				"kind": x.Kind != y.Kind, "labels": !slices.Equal(x.Labels, y.Labels),
				"base_type": x.BaseType != y.BaseType, "not_null": x.NotNull != y.NotNull,
				"default": x.Default != y.Default, "collation": x.Collation != y.Collation,
				"checks": !slices.Equal(x.Checks, y.Checks), "attributes": !slices.Equal(x.Attributes, y.Attributes),
			})
		})
	diffList(d, KindSequence, a.Sequences, b.Sequences, (*Sequence).ID, func(s *Sequence) *Object { return &Object{Sequence: s} },
		func(x, y *Sequence) []string {
			return fields(map[string]bool{
				"data_type": x.DataType != y.DataType, "start": x.Start != y.Start, "increment": x.Increment != y.Increment,
				"min": x.Min != y.Min, "max": x.Max != y.Max, "cache": x.Cache != y.Cache, "cycle": x.Cycle != y.Cycle,
				"owned_by": x.OwnedBy != y.OwnedBy,
			})
		})
	d.tables(a, b)
	diffList(d, KindConstraint, a.Constraints, b.Constraints, (*Constraint).ID, func(c *Constraint) *Object { return &Object{Constraint: c} },
		func(x, y *Constraint) []string {
			return fields(map[string]bool{"definition": x.Definition != y.Definition})
		})
	diffList(d, KindIndex, a.Indexes, b.Indexes, (*Index).ID, func(i *Index) *Object { return &Object{Index: i} },
		func(x, y *Index) []string { return fields(map[string]bool{"definition": x.Definition != y.Definition}) })
	diffList(d, KindView, a.Views, b.Views, (*View).ID, func(v *View) *Object { return &Object{View: v} },
		func(x, y *View) []string {
			return fields(map[string]bool{
				"materialized": x.Materialized != y.Materialized, "definition": sameMajor && x.Definition != y.Definition,
				"options": !slices.Equal(x.Options, y.Options),
			})
		})
	diffList(d, KindFunc, a.Functions, b.Functions, (*Function).ID, func(f *Function) *Object { return &Object{Function: f} },
		func(x, y *Function) []string {
			return fields(map[string]bool{"kind": x.Kind != y.Kind, "definition": x.Definition != y.Definition})
		})
	diffList(d, KindTrigger, a.Triggers, b.Triggers, (*Trigger).ID, func(t *Trigger) *Object { return &Object{Trigger: t} },
		func(x, y *Trigger) []string {
			return fields(map[string]bool{"definition": x.Definition != y.Definition, "enabled": x.Enabled != y.Enabled})
		})
	diffList(d, KindPolicy, a.Policies, b.Policies, (*Policy).ID, func(p *Policy) *Object { return &Object{Policy: p} },
		func(x, y *Policy) []string {
			if reflect.DeepEqual(x, y) {
				return nil
			}
			return []string{"definition"}
		})
	diffList(d, KindGrant, a.Grants, b.Grants, (*Grant).ID, func(g *Grant) *Object { return &Object{Grant: g} },
		func(x, y *Grant) []string { return fields(map[string]bool{"grantable": x.Grantable != y.Grantable}) })

	slices.SortStableFunc(d.changes, func(x, y Change) int {
		if c := cmp.Compare(slices.Index(kindOrder, x.Kind), slices.Index(kindOrder, y.Kind)); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})
	return d.changes
}

type differ struct {
	changes []Change
}

// tables diffs table-level attributes, then columns of tables on both sides.
func (d *differ) tables(a, b *Schema) {
	diffList(d, KindTable, a.Tables, b.Tables, (*Table).ID, func(t *Table) *Object { return &Object{Table: t} },
		func(x, y *Table) []string {
			return fields(map[string]bool{
				"partition_key": x.PartitionKey != y.PartitionKey, "partition_of": x.PartitionOf != y.PartitionOf,
				"partition_bound": x.PartitionBound != y.PartitionBound, "unlogged": x.Unlogged != y.Unlogged,
				"options": !slices.Equal(x.Options, y.Options), "rls_enabled": x.RLSEnabled != y.RLSEnabled,
				"rls_forced": x.RLSForced != y.RLSForced,
			})
		})
	for i := range b.Tables {
		after := &b.Tables[i]
		before := a.Table(after.ID())
		if before == nil {
			continue
		}
		colID := func(c *Column) string { return after.ID() + "." + QuoteIdent(c.Name) }
		start := len(d.changes)
		diffList(d, KindColumn, before.Columns, after.Columns, colID, func(c *Column) *Object { return &Object{Column: c} },
			func(x, y *Column) []string {
				return fields(map[string]bool{
					"type": x.Type != y.Type, "not_null": x.NotNull != y.NotNull, "default": x.Default != y.Default,
					"collation": x.Collation != y.Collation, "identity": x.Identity != y.Identity, "generated": x.Generated != y.Generated,
				})
			})
		for j := start; j < len(d.changes); j++ {
			d.changes[j].Table = after.ID()
		}
	}
}

func diffList[T any](d *differ, kind Kind, before, after []T, id func(*T) string, obj func(*T) *Object, changed func(x, y *T) []string) {
	old := make(map[string]*T, len(before))
	for i := range before {
		old[id(&before[i])] = &before[i]
	}
	seen := make(map[string]bool, len(after))
	for i := range after {
		y := &after[i]
		key := id(y)
		seen[key] = true
		x, ok := old[key]
		if !ok {
			d.changes = append(d.changes, Change{Op: OpAdd, Kind: kind, ID: key, Table: ownerOf(obj(y)), After: obj(y)})
			continue
		}
		if changed == nil {
			continue
		}
		if f := changed(x, y); len(f) > 0 {
			d.changes = append(d.changes, Change{Op: OpAlter, Kind: kind, ID: key, Table: ownerOf(obj(y)), Fields: f, Before: obj(x), After: obj(y)})
		}
	}
	for i := range before {
		x := &before[i]
		if key := id(x); !seen[key] {
			d.changes = append(d.changes, Change{Op: OpDrop, Kind: kind, ID: key, Table: ownerOf(obj(x)), Before: obj(x)})
		}
	}
}

// ownerOf returns the table a sub-object belongs to.
func ownerOf(o *Object) string {
	switch {
	case o.Constraint != nil:
		return o.Constraint.Table
	case o.Index != nil:
		return o.Index.Table
	case o.Trigger != nil:
		return o.Trigger.Table
	case o.Policy != nil:
		return o.Policy.Table
	}
	return ""
}

// fields returns the names of the fields marked true, sorted.
func fields(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

func withoutTables(s *Schema, ids []string) *Schema {
	if len(ids) == 0 {
		return s
	}
	skip := map[string]bool{}
	for _, id := range ids {
		skip[id] = true
	}
	c := *s
	c.Tables = slices.DeleteFunc(slices.Clone(s.Tables), func(t Table) bool { return skip[t.ID()] })
	c.Constraints = slices.DeleteFunc(slices.Clone(s.Constraints), func(x Constraint) bool { return skip[x.Table] })
	c.Indexes = slices.DeleteFunc(slices.Clone(s.Indexes), func(x Index) bool { return skip[x.Table] })
	c.Triggers = slices.DeleteFunc(slices.Clone(s.Triggers), func(x Trigger) bool { return skip[x.Table] })
	c.Policies = slices.DeleteFunc(slices.Clone(s.Policies), func(x Policy) bool { return skip[x.Table] })
	c.Grants = slices.DeleteFunc(slices.Clone(s.Grants), func(g Grant) bool {
		if g.ObjectKind == ObjectColumn {
			t, _, _ := cutLast(g.Object)
			return skip[t]
		}
		return skip[g.Object]
	})
	c.Sequences = slices.DeleteFunc(slices.Clone(s.Sequences), func(q Sequence) bool {
		t, _, _ := cutLast(q.OwnedBy)
		return skip[t]
	})
	return &c
}
