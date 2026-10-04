package schema

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// DDLOptions tunes ChangeDDL.
type DDLOptions struct {
	// Reverse returns the statements that undo the changes instead.
	Reverse bool
	// Guarded adds IF [NOT] EXISTS wherever Postgres allows it, so the script
	// can run against a database that already has some of the changes.
	Guarded bool
}

// ChangeDDL returns statements that apply changes, as Diff returns them, to
// the "before" schema. Drops run first, dependents before what they depend
// on; then adds and alters. What Postgres can't do in place, such as removing
// an enum label, comes out as a SQL comment explaining it.
func ChangeDDL(changes []Change, opts DDLOptions) []string {
	cs := slices.Clone(changes)
	if opts.Reverse {
		for i := range cs {
			cs[i] = invert(cs[i])
		}
	}
	var drops, rest []Change
	dropped := droppedObjects(cs)
	for _, c := range cs {
		switch {
		case c.Op != OpDrop:
			rest = append(rest, c)
		case !impliedDrop(c, dropped):
			drops = append(drops, c)
		}
	}
	slices.SortStableFunc(drops, func(a, b Change) int { return cmp.Compare(ddlRank(b), ddlRank(a)) })
	slices.SortStableFunc(rest, func(a, b Change) int { return cmp.Compare(ddlRank(a), ddlRank(b)) })
	drops = topoSort(drops, changeID, dependents(drops))
	rest = topoSort(rest, changeID, dependencies)

	g := ddlGen{guarded: opts.Guarded}
	// late runs last: a sequence can only be owned by a column once the
	// column exists, and can only be dropped once no column default uses it.
	var out, late []string
	for _, c := range rest {
		// An identity column's sequence goes with the identity, freeing its
		// name for a serial column's sequence added below.
		if c.Kind == KindColumn && c.Op == OpAlter && c.Before.Column.Identity != "" && c.After.Column.Identity == "" {
			out = append(out, "ALTER TABLE "+c.Table+" ALTER COLUMN "+QuoteIdent(c.After.Column.Name)+" DROP IDENTITY"+map[bool]string{true: " IF EXISTS"}[g.guarded])
		}
	}
	for _, c := range drops {
		if c.Kind == KindSequence {
			late = append(late, g.drop(c)...)
		} else {
			out = append(out, g.drop(c)...)
		}
	}
	for _, c := range rest {
		switch {
		case c.Op == OpAdd && c.Kind == KindSequence && c.After.Sequence.OwnedBy != "":
			out = append(out, g.add(c)...)
			late = append(late, "ALTER SEQUENCE "+c.ID+" OWNED BY "+c.After.Sequence.OwnedBy)
		case c.Op == OpAdd:
			out = append(out, g.add(c)...)
		default:
			out = append(out, g.alter(c)...)
		}
	}
	return append(out, late...)
}

func changeID(c *Change) string { return c.ID }

// dependencies lists what a change's object needs to exist first: the
// relations and functions a view, function or type uses, and the relation
// an index is on.
func dependencies(c *Change) []string {
	o := c.object()
	switch {
	case o.View != nil:
		return o.View.DependsOn
	case o.Function != nil:
		return o.Function.DependsOn
	case o.Type != nil:
		return o.Type.DependsOn
	case o.Index != nil:
		return []string{o.Index.Table}
	}
	return nil
}

// dependents inverts dependencies over cs, so drops run dependents first.
func dependents(cs []Change) func(*Change) []string {
	of := map[string][]string{}
	for i := range cs {
		for _, d := range dependencies(&cs[i]) {
			of[d] = append(of[d], cs[i].ID)
		}
	}
	return func(c *Change) []string { return of[c.ID] }
}

// droppedObjects returns the identities of tables, views and columns being
// dropped.
func droppedObjects(cs []Change) map[string]bool {
	dropped := map[string]bool{}
	for _, c := range cs {
		if c.Op == OpDrop && (c.Kind == KindTable || c.Kind == KindView || c.Kind == KindColumn) {
			dropped[c.ID] = true
		}
	}
	return dropped
}

// impliedDrop reports whether dropping a relation or column already drops
// this object: its indexes, constraints, triggers, policies, grants and owned
// sequences.
func impliedDrop(c Change, dropped map[string]bool) bool {
	if dropped[c.Table] {
		return true
	}
	o := c.Before
	switch {
	case o.Grant != nil && o.Grant.ObjectKind == ObjectColumn:
		table, _ := cutLast(o.Grant.Object)
		return dropped[table]
	case o.Grant != nil:
		return dropped[o.Grant.Object]
	case o.Sequence != nil && o.Sequence.OwnedBy != "":
		table, _ := cutLast(o.Sequence.OwnedBy)
		return dropped[table] || dropped[o.Sequence.OwnedBy]
	}
	return false
}

func invert(c Change) Change {
	c.Before, c.After = c.After, c.Before
	switch c.Op {
	case OpAdd:
		c.Op = OpDrop
	case OpDrop:
		c.Op = OpAdd
	case OpAlter:
		// Swapping Before and After reverses an alter.
	}
	return c
}

// ddlRank orders kinds so each object comes after what it needs. Foreign
// keys come after indexes, which unique keys may rely on.
func ddlRank(c Change) int {
	order := []Kind{
		KindSchema, KindExtension, KindType, KindFunc, KindSequence, KindTable, KindColumn,
		KindConstraint, KindIndex, "foreign_key", KindView, KindTrigger, KindPolicy, KindGrant,
	}
	k := c.Kind
	o := c.object()
	switch {
	case o.Constraint != nil && o.Constraint.Type == ConstraintForeignKey:
		k = "foreign_key"
	case o.Function != nil && len(o.Function.DependsOn) > 0:
		// Like RestoreDDL: a function that uses relations comes with the views.
		k = KindView
	}
	return slices.Index(order, k)
}

// object returns whichever side of the change is present, preferring after.
func (c Change) object() *Object {
	if c.After != nil {
		return c.After
	}
	return c.Before
}

type ddlGen struct {
	guarded bool
}

func (g ddlGen) add(c Change) []string {
	o := c.After
	switch c.Kind {
	case KindSchema:
		return []string{createSchemaSQL(*o.Namespace, g.guarded)}
	case KindExtension:
		return []string{createExtensionSQL(*o.Extension)}
	case KindType:
		out := []string{createTypeSQL(*o.Type)}
		for _, chk := range o.Type.Checks {
			out = append(out, domainCheckSQL(*o.Type, chk))
		}
		return out
	case KindSequence:
		// ChangeDDL sets OWNED BY last, once the owning column exists.
		return []string{strings.Replace(createSequenceSQL(*o.Sequence), "CREATE SEQUENCE ", "CREATE SEQUENCE "+ifNotExistsSQL(g.guarded), 1)}
	case KindTable:
		stmts := createTableSQL(*o.Table, nil)
		stmts[0] = strings.Replace(stmts[0], "TABLE ", "TABLE "+ifNotExistsSQL(g.guarded), 1)
		return append(stmts, rlsSQL(*o.Table)...)
	case KindColumn:
		return []string{"ALTER TABLE " + c.Table + " ADD COLUMN " + ifNotExistsSQL(g.guarded) + columnSQL(*o.Column)}
	case KindConstraint:
		con := *o.Constraint
		if g.guarded {
			return []string{unlessExists(fmt.Sprintf("SELECT FROM pg_catalog.pg_constraint WHERE conrelid = %s::regclass AND conname = %s",
				QuoteLiteral(con.Table), QuoteLiteral(con.Name)), addConstraintSQL(con))}
		}
		return []string{addConstraintSQL(con)}
	case KindIndex:
		def := o.Index.Definition
		if g.guarded {
			def = strings.Replace(def, " INDEX ", " INDEX IF NOT EXISTS ", 1)
		}
		return []string{def}
	case KindView:
		// Unlike a restore, a change runs where the data is: fill the view.
		return []string{strings.TrimSuffix(createViewSQL(*o.View, g.guarded), "\nWITH NO DATA")}
	case KindFunc:
		return []string{o.Function.Definition}
	case KindTrigger:
		return triggerSQL(*o.Trigger)
	case KindPolicy:
		return []string{createPolicySQL(*o.Policy)}
	case KindGrant:
		return []string{grantSQL(*o.Grant)}
	}
	return nil
}

func (g ddlGen) drop(c Change) []string {
	o := c.Before
	ie := ifExistsSQL(g.guarded)
	switch c.Kind {
	case KindSchema:
		return []string{"DROP SCHEMA " + ie + QuoteIdent(o.Namespace.Name)}
	case KindExtension:
		return []string{"DROP EXTENSION " + ie + QuoteIdent(o.Extension.Name)}
	case KindType:
		kind := "TYPE "
		if o.Type.Kind == TypeDomain {
			kind = "DOMAIN "
		}
		return []string{"DROP " + kind + ie + o.Type.ID()}
	case KindSequence:
		id := o.Sequence.ID()
		if g.guarded {
			// An identity column's own sequence may have taken the name, as
			// when a serial column turns identity.
			return []string{when(fmt.Sprintf("EXISTS (SELECT FROM pg_catalog.pg_class s WHERE s.oid = to_regclass(%s) AND NOT EXISTS "+
				"(SELECT FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = s.oid AND d.deptype = 'i'))",
				QuoteLiteral(id)), "DROP SEQUENCE "+id)}
		}
		return []string{"DROP SEQUENCE " + id}
	case KindTable:
		return []string{"DROP TABLE " + ie + o.Table.ID()}
	case KindColumn:
		return []string{"ALTER TABLE " + c.Table + " DROP COLUMN " + ie + QuoteIdent(o.Column.Name)}
	case KindConstraint:
		return []string{"ALTER TABLE " + o.Constraint.Table + " DROP CONSTRAINT " + ie + QuoteIdent(o.Constraint.Name)}
	case KindIndex:
		schema, _ := cutLast(o.Index.Table)
		return []string{"DROP INDEX " + ie + schema + "." + QuoteIdent(o.Index.Name)}
	case KindView:
		kind := "VIEW "
		if o.View.Materialized {
			kind = "MATERIALIZED VIEW "
		}
		return []string{"DROP " + kind + ie + o.View.ID()}
	case KindFunc:
		return []string{"DROP " + strings.ToUpper(string(o.Function.Kind)) + " " + ie + o.Function.ID()}
	case KindTrigger:
		return []string{"DROP TRIGGER " + ie + QuoteIdent(o.Trigger.Name) + " ON " + o.Trigger.Table}
	case KindPolicy:
		return []string{"DROP POLICY " + ie + QuoteIdent(o.Policy.Name) + " ON " + o.Policy.Table}
	case KindGrant:
		return []string{revokeSQL(*o.Grant)}
	}
	return nil
}

func (g ddlGen) alter(c Change) []string {
	switch c.Kind {
	case KindExtension:
		return alterExtensionSQL(*c.Before.Extension, *c.After.Extension)
	case KindType:
		return g.alterType(c)
	case KindSequence:
		return alterSequenceSQL(*c.Before.Sequence, *c.After.Sequence)
	case KindTable:
		return alterTableSQL(*c.Before.Table, *c.After.Table)
	case KindColumn:
		return g.alterColumnSQL(c.Table, *c.Before.Column, *c.After.Column)
	case KindView:
		var out []string
		if c.Before.View.Materialized || c.After.View.Materialized {
			out = g.recreate(c)
		} else {
			// CREATE OR REPLACE can't remove, rename or retype a view's columns
			// (invalid_table_definition); only then does the view go and come back.
			v := *c.After.View
			out = []string{"DO $dbproof$ BEGIN " + createViewSQL(v, true) + "; EXCEPTION WHEN invalid_table_definition THEN DROP VIEW " + v.ID() + "; " +
				createViewSQL(v, false) + "; END $dbproof$"}
		}
		// The kept objects may have survived (CREATE OR REPLACE), so add them
		// guarded.
		for _, k := range c.Keep {
			kind := KindIndex
			if k.Grant != nil {
				kind = KindGrant
			}
			out = append(out, ddlGen{guarded: true}.add(Change{Op: OpAdd, Kind: kind, After: &k})...)
		}
		return out
	case KindFunc:
		if c.Before.Function.Kind != c.After.Function.Kind || c.After.Function.Kind == KindAggregate {
			return g.recreate(c)
		}
		return []string{c.After.Function.Definition}
	case KindTrigger:
		if c.Before.Trigger.Definition != c.After.Trigger.Definition {
			return g.recreate(c)
		}
		t := *c.After.Trigger
		state := map[string]string{"origin": "ENABLE", "disabled": "DISABLE", "replica": "ENABLE REPLICA", "always": "ENABLE ALWAYS"}
		return []string{"ALTER TABLE " + t.Table + " " + state[t.Enabled] + " TRIGGER " + QuoteIdent(t.Name)}
	default:
		// Constraints, indexes, policies and grants change by being recreated.
		return g.recreate(c)
	}
}

func (g ddlGen) alterType(c Change) []string {
	before, after := *c.Before.Type, *c.After.Type
	if before.Kind != after.Kind || before.BaseType != after.BaseType {
		return g.recreate(c)
	}
	id := after.ID()
	var out []string
	switch after.Kind {
	case TypeEnum:
		for i, l := range after.Labels {
			if slices.Contains(before.Labels, l) {
				continue
			}
			stmt := "ALTER TYPE " + id + " ADD VALUE " + ifNotExistsSQL(g.guarded) + QuoteLiteral(l)
			if i > 0 {
				stmt += " AFTER " + QuoteLiteral(after.Labels[i-1])
			} else if j := slices.IndexFunc(after.Labels, func(l string) bool { return slices.Contains(before.Labels, l) }); j >= 0 {
				// Without a position Postgres appends the label.
				stmt += " BEFORE " + QuoteLiteral(after.Labels[j])
			}
			out = append(out, stmt)
		}
		for _, l := range before.Labels {
			if !slices.Contains(after.Labels, l) {
				out = append(out, fmt.Sprintf("-- Postgres can't remove enum label %s from %s; recreate the type to drop it", QuoteLiteral(l), id))
			}
		}
	case TypeDomain:
		if before.Default != after.Default {
			if after.Default == "" {
				out = append(out, "ALTER DOMAIN "+id+" DROP DEFAULT")
			} else {
				out = append(out, "ALTER DOMAIN "+id+" SET DEFAULT "+after.Default)
			}
		}
		if before.NotNull != after.NotNull {
			out = append(out, "ALTER DOMAIN "+id+map[bool]string{true: " SET NOT NULL", false: " DROP NOT NULL"}[after.NotNull])
		}
		for _, chk := range before.Checks {
			if !slices.Contains(after.Checks, chk) {
				out = append(out, "ALTER DOMAIN "+id+" DROP CONSTRAINT "+QuoteIdent(chk.Name))
			}
		}
		for _, chk := range after.Checks {
			if !slices.Contains(before.Checks, chk) {
				out = append(out, domainCheckSQL(after, chk))
			}
		}
		if before.Collation != after.Collation {
			out = append(out, fmt.Sprintf("-- Postgres can't change the collation of domain %s in place", id))
		}
	case TypeComposite:
		for _, a := range before.Attributes {
			if !slices.ContainsFunc(after.Attributes, func(b Attribute) bool { return b.Name == a.Name }) {
				out = append(out, "ALTER TYPE "+id+" DROP ATTRIBUTE "+QuoteIdent(a.Name))
			}
		}
		for _, a := range after.Attributes {
			i := slices.IndexFunc(before.Attributes, func(b Attribute) bool { return b.Name == a.Name })
			switch {
			case i < 0:
				out = append(out, "ALTER TYPE "+id+" ADD ATTRIBUTE "+QuoteIdent(a.Name)+" "+a.Type)
			case before.Attributes[i].Type != a.Type:
				out = append(out, "ALTER TYPE "+id+" ALTER ATTRIBUTE "+QuoteIdent(a.Name)+" TYPE "+a.Type)
			}
		}
	}
	return out
}

func alterExtensionSQL(before, after Extension) []string {
	var out []string
	if before.Schema != after.Schema {
		out = append(out, "ALTER EXTENSION "+QuoteIdent(after.Name)+" SET SCHEMA "+QuoteIdent(after.Schema))
	}
	if before.Version != after.Version {
		out = append(out, "ALTER EXTENSION "+QuoteIdent(after.Name)+" UPDATE TO "+QuoteLiteral(after.Version))
	}
	return out
}

// recreate drops the old object and adds the new one.
func (g ddlGen) recreate(c Change) []string {
	return append(g.drop(Change{Op: OpDrop, Kind: c.Kind, ID: c.ID, Table: c.Table, Before: c.Before}),
		g.add(Change{Op: OpAdd, Kind: c.Kind, ID: c.ID, Table: c.Table, After: c.After})...)
}

func alterSequenceSQL(before, after Sequence) []string {
	var parts []string
	if before.DataType != after.DataType {
		parts = append(parts, "AS "+after.DataType)
	}
	if before.Increment != after.Increment {
		parts = append(parts, fmt.Sprintf("INCREMENT BY %d", after.Increment))
	}
	if before.Min != after.Min {
		parts = append(parts, fmt.Sprintf("MINVALUE %d", after.Min))
	}
	if before.Max != after.Max {
		parts = append(parts, fmt.Sprintf("MAXVALUE %d", after.Max))
	}
	if before.Start != after.Start {
		parts = append(parts, fmt.Sprintf("START WITH %d", after.Start))
	}
	if before.Cache != after.Cache {
		parts = append(parts, fmt.Sprintf("CACHE %d", after.Cache))
	}
	if before.Cycle != after.Cycle {
		parts = append(parts, map[bool]string{true: "CYCLE", false: "NO CYCLE"}[after.Cycle])
	}
	if before.OwnedBy != after.OwnedBy {
		owner := after.OwnedBy
		if owner == "" {
			owner = "NONE"
		}
		parts = append(parts, "OWNED BY "+owner)
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{"ALTER SEQUENCE " + after.ID() + " " + strings.Join(parts, " ")}
}

func alterTableSQL(before, after Table) []string {
	id := after.ID()
	var out []string
	if before.Unlogged != after.Unlogged {
		out = append(out, "ALTER TABLE "+id+map[bool]string{true: " SET UNLOGGED", false: " SET LOGGED"}[after.Unlogged])
	}
	if !slices.Equal(before.Options, after.Options) {
		var reset []string
		for _, o := range before.Options {
			name, _, _ := strings.Cut(o, "=")
			if !slices.ContainsFunc(after.Options, func(a string) bool { return strings.HasPrefix(a, name+"=") }) {
				reset = append(reset, name)
			}
		}
		if len(reset) > 0 {
			out = append(out, "ALTER TABLE "+id+" RESET ("+strings.Join(reset, ", ")+")")
		}
		if len(after.Options) > 0 {
			out = append(out, "ALTER TABLE "+id+" SET ("+strings.Join(after.Options, ", ")+")")
		}
	}
	if before.RLSEnabled != after.RLSEnabled {
		out = append(out, "ALTER TABLE "+id+map[bool]string{true: " ENABLE", false: " DISABLE"}[after.RLSEnabled]+" ROW LEVEL SECURITY")
	}
	if before.RLSForced != after.RLSForced {
		out = append(out, "ALTER TABLE "+id+map[bool]string{true: " FORCE", false: " NO FORCE"}[after.RLSForced]+" ROW LEVEL SECURITY")
	}
	if before.PartitionKey != after.PartitionKey || before.PartitionOf != after.PartitionOf || before.PartitionBound != after.PartitionBound {
		out = append(out, fmt.Sprintf("-- Partitioning of %s changed; Postgres can't change it in place", id))
	}
	return out
}

func (g ddlGen) alterColumnSQL(table string, before, after Column) []string {
	col := "ALTER TABLE " + table + " ALTER COLUMN " + QuoteIdent(after.Name)
	var out []string
	if before.Generated != after.Generated || (after.Generated != "" && before.Default != after.Default) {
		return []string{fmt.Sprintf("-- The generation expression of %s.%s changed; drop and re-add the column to change it", table, QuoteIdent(after.Name))}
	}
	if before.Type != after.Type || before.Collation != after.Collation {
		stmt := col + " TYPE " + after.Type
		if after.Collation != "" {
			stmt += " COLLATE " + after.Collation
		}
		out = append(out, stmt)
	}
	toIdentity := before.Identity == "" && after.Identity != ""
	if before.Default != after.Default && !toIdentity {
		if after.Default == "" {
			out = append(out, col+" DROP DEFAULT")
		} else {
			out = append(out, col+" SET DEFAULT "+after.Default)
		}
	}
	if before.NotNull != after.NotNull {
		out = append(out, col+map[bool]string{true: " SET NOT NULL", false: " DROP NOT NULL"}[after.NotNull])
	}
	switch {
	case toIdentity:
		// An identity column has no default: a serial column drops its
		// nextval default first.
		stmt := col + " ADD GENERATED " + strings.ToUpper(after.Identity) + " AS IDENTITY"
		if before.Default != "" {
			stmt = col + " DROP DEFAULT; " + stmt
		}
		if g.guarded {
			stmt = unlessExists(fmt.Sprintf("SELECT FROM pg_catalog.pg_attribute WHERE attrelid = %s::regclass AND attname = %s AND attidentity <> ''",
				QuoteLiteral(table), QuoteLiteral(after.Name)), stmt)
		}
		out = append(out, stmt)
	case before.Identity != "" && after.Identity != "" && before.Identity != after.Identity:
		out = append(out, col+" SET GENERATED "+strings.ToUpper(after.Identity))
	}
	return out
}
