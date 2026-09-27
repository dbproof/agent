package schema

import (
	"slices"
	"strings"
)

// Line is one line of a readable diff. Op is "+", "-" or "~", or " " for a
// line that only gives context, such as the table a column belongs to.
type Line struct {
	Op     string `json:"op"`
	Text   string `json:"text"`
	Indent int    `json:"indent,omitempty"`
}

// Describe renders changes the way people read them: changes inside a table
// grouped under it, objects in the public schema without their prefix, and
// types in their short spelling.
func Describe(changes []Change) []Line {
	var out []Line
	byTable := map[string][]Change{}
	var tables []string
	tableChange := map[string]Change{}
	for _, c := range changes {
		switch {
		case c.Kind == KindTable:
			tableChange[c.ID] = c
			if !slices.Contains(tables, c.ID) {
				tables = append(tables, c.ID)
			}
		case c.Table != "":
			if !slices.Contains(tables, c.Table) {
				tables = append(tables, c.Table)
			}
			byTable[c.Table] = append(byTable[c.Table], c)
		}
	}
	for _, id := range tables {
		out = append(out, describeTable(id, tableChange, byTable[id])...)
	}
	for _, c := range changes {
		if c.Kind == KindTable || c.Table != "" {
			continue
		}
		out = append(out, Line{Op: opSign(c.Op), Text: describeObject(c)})
	}
	return out
}

func describeTable(id string, tableChange map[string]Change, subs []Change) []Line {
	var out []Line
	tc, changed := tableChange[id]
	switch {
	case changed && tc.Op == OpAdd:
		t := tc.After.Table
		names := make([]string, len(t.Columns))
		for i, c := range t.Columns {
			names[i] = c.Name
		}
		out = append(out, Line{Op: "+", Text: "TABLE " + DisplayName(id) + " (" + strings.Join(names, ", ") + ")"})
	case changed && tc.Op == OpDrop:
		return []Line{{Op: "-", Text: "TABLE " + DisplayName(id)}}
	case changed:
		out = append(out, Line{Op: "~", Text: "TABLE " + DisplayName(id) + " (" + strings.Join(tc.Fields, ", ") + ")"})
	default:
		out = append(out, Line{Op: " ", Text: "TABLE " + DisplayName(id)})
	}
	for _, c := range subs {
		out = append(out, Line{Op: opSign(c.Op), Text: describeSub(c), Indent: 1})
	}
	return out
}

func describeSub(c Change) string {
	o := c.object()
	switch {
	case o.Column != nil && c.Op == OpAlter:
		return describeColumnChange(*c.Before.Column, *c.After.Column)
	case o.Column != nil:
		return describeColumn(*o.Column)
	case o.Index != nil:
		kind := "INDEX "
		if o.Index.Unique {
			kind = "UNIQUE INDEX "
		}
		return kind + o.Index.Name + " (" + columnList(o.Index.Columns) + ")"
	case o.Constraint != nil:
		return describeConstraint(*o.Constraint)
	case o.Trigger != nil:
		return "TRIGGER " + o.Trigger.Name
	case o.Policy != nil:
		return "POLICY " + o.Policy.Name
	}
	return c.ID
}

func describeColumn(c Column) string {
	s := c.Name + " " + ShortType(c.Type)
	if c.NotNull {
		s += " NOT NULL"
	} else {
		s += " NULL"
	}
	if c.Default != "" && c.Generated == "" && c.Identity == "" {
		s += " DEFAULT " + c.Default
	}
	return s
}

func describeColumnChange(before, after Column) string {
	var parts []string
	if before.Type != after.Type {
		parts = append(parts, ShortType(before.Type)+" → "+ShortType(after.Type))
	}
	if before.NotNull != after.NotNull {
		null := map[bool]string{true: "NOT NULL", false: "NULL"}
		prefix := ""
		if before.Type == after.Type {
			prefix = ShortType(after.Type) + " "
		}
		parts = append(parts, prefix+null[before.NotNull]+" → "+null[after.NotNull])
	}
	if before.Default != after.Default {
		parts = append(parts, "DEFAULT "+orNone(before.Default)+" → "+orNone(after.Default))
	}
	for _, f := range []struct{ name, before, after string }{
		{"COLLATE", before.Collation, after.Collation},
		{"IDENTITY", before.Identity, after.Identity},
		{"GENERATED", before.Generated, after.Generated},
	} {
		if f.before != f.after {
			parts = append(parts, f.name+" "+orNone(f.before)+" → "+orNone(f.after))
		}
	}
	return after.Name + " " + strings.Join(parts, "; ")
}

func describeConstraint(c Constraint) string {
	switch c.Type {
	case ConstraintPrimaryKey:
		return "PRIMARY KEY " + c.Name + " (" + columnList(c.Columns) + ")"
	case ConstraintUnique:
		return "UNIQUE " + c.Name + " (" + columnList(c.Columns) + ")"
	case ConstraintForeignKey:
		return "FOREIGN KEY " + c.Name + " (" + columnList(c.Columns) + ") → " + DisplayName(c.RefTable) + "(" + columnList(c.RefColumns) + ")"
	case ConstraintExclusion:
		return "EXCLUDE " + c.Name
	}
	return "CHECK " + c.Name
}

func describeObject(c Change) string {
	o := c.object()
	suffix := ""
	if c.Op == OpAlter && len(c.Fields) > 0 {
		suffix = " (" + strings.Join(c.Fields, ", ") + ")"
	}
	switch {
	case o.Namespace != nil:
		return "SCHEMA " + o.Namespace.Name + suffix
	case o.Extension != nil:
		return "EXTENSION " + o.Extension.Name + suffix
	case o.Type != nil:
		if c.Op == OpAlter && c.Before.Type.Kind == TypeEnum && c.After.Type.Kind == TypeEnum {
			return "TYPE " + DisplayName(c.ID) + " " + labelChanges(c.Before.Type.Labels, c.After.Type.Labels)
		}
		return strings.ToUpper(string(o.Type.Kind)) + " " + DisplayName(c.ID) + suffix
	case o.Sequence != nil:
		return "SEQUENCE " + DisplayName(c.ID) + suffix
	case o.View != nil:
		kind := "VIEW "
		if o.View.Materialized {
			kind = "MATERIALIZED VIEW "
		}
		return kind + DisplayName(c.ID) + suffix
	case o.Function != nil:
		return strings.ToUpper(string(o.Function.Kind)) + " " + DisplayName(c.ID) + suffix
	case o.Grant != nil:
		g := o.Grant
		return "GRANT " + g.Privilege + " ON " + DisplayName(g.Object) + " TO " + g.Grantee
	}
	return c.ID
}

func labelChanges(before, after []string) string {
	var parts []string
	for _, l := range after {
		if !slices.Contains(before, l) {
			parts = append(parts, "+"+l)
		}
	}
	for _, l := range before {
		if !slices.Contains(after, l) {
			parts = append(parts, "-"+l)
		}
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

func opSign(op Op) string {
	switch op {
	case OpAdd:
		return "+"
	case OpDrop:
		return "-"
	}
	return "~"
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func columnList(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		if c == "" {
			c = "expression"
		}
		out[i] = c
	}
	return strings.Join(out, ", ")
}

// DisplayName drops the public schema from an identity: public.invoices
// reads as invoices.
func DisplayName(id string) string {
	return strings.TrimPrefix(id, "public.")
}

var shortTypes = []struct{ long, short string }{
	{"character varying", "varchar"},
	{"timestamp with time zone", "timestamptz"},
	{"timestamp without time zone", "timestamp"},
	{"time with time zone", "timetz"},
	{"time without time zone", "time"},
	{"character", "char"},
	{"bit varying", "varbit"},
}

// ShortType spells a type the way people write it: varchar(32), timestamptz.
func ShortType(t string) string {
	for _, s := range shortTypes {
		if strings.HasPrefix(t, s.long) {
			return s.short + strings.TrimPrefix(t, s.long)
		}
	}
	return t
}
