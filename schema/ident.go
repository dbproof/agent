package schema

import "strings"

// QuoteIdent quotes a name the way Postgres's quote_ident does: only when it
// isn't a plain lower-case identifier or is a non-unreserved keyword.
func QuoteIdent(name string) string {
	if isPlainIdent(name) {
		if _, kw := keywords[name]; !kw {
			return name
		}
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func isPlainIdent(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// Qualified returns schema.name with each part quoted as needed.
func Qualified(schema, name string) string {
	return QuoteIdent(schema) + "." + QuoteIdent(name)
}

// QuoteLiteral quotes a string as a SQL literal.
func QuoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ID returns the table's identity, e.g. public.invoices.
func (t *Table) ID() string { return Qualified(t.Schema, t.Name) }

// ID returns the view's identity.
func (v *View) ID() string { return Qualified(v.Schema, v.Name) }

// ID returns the type's identity.
func (t *Type) ID() string { return Qualified(t.Schema, t.Name) }

// ID returns the sequence's identity.
func (s *Sequence) ID() string { return Qualified(s.Schema, s.Name) }

// ID returns the function's identity including its argument types, e.g.
// public.touch(integer).
func (f *Function) ID() string { return Qualified(f.Schema, f.Name) + "(" + f.Args + ")" }

// ID returns the constraint's identity, e.g. public.invoices.invoices_pkey.
func (c *Constraint) ID() string { return c.Table + "." + QuoteIdent(c.Name) }

// ID returns the index's identity.
func (i *Index) ID() string { return i.Table + "." + QuoteIdent(i.Name) }

// ID returns the trigger's identity.
func (t *Trigger) ID() string { return t.Table + "." + QuoteIdent(t.Name) }

// ID returns the policy's identity.
func (p *Policy) ID() string { return p.Table + "." + QuoteIdent(p.Name) }

// ID returns the grant's identity.
func (g *Grant) ID() string {
	return string(g.ObjectKind) + " " + g.Object + " " + g.Privilege + " " + g.Grantee
}

// Column returns the named column, or nil.
func (t *Table) Column(name string) *Column {
	for i := range t.Columns {
		if t.Columns[i].Name == name {
			return &t.Columns[i]
		}
	}
	return nil
}

// tablesByID indexes s's tables by identity, for lookups in a loop over
// them, where Table would make the loop quadratic.
func tablesByID(s *Schema) map[string]*Table {
	m := make(map[string]*Table, len(s.Tables))
	for i := range s.Tables {
		m[s.Tables[i].ID()] = &s.Tables[i]
	}
	return m
}

// Table returns the table with the given identity, or nil.
func (s *Schema) Table(id string) *Table {
	for i := range s.Tables {
		if s.Tables[i].ID() == id {
			return &s.Tables[i]
		}
	}
	return nil
}
