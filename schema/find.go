package schema

import "reflect"

// Find returns the object of a kind with the given identity, as Diff names
// them, or nil. For a column the identity is table.column.
func (s *Schema) Find(kind Kind, id string) *Object {
	switch kind {
	case KindSchema:
		for i := range s.Namespaces {
			if QuoteIdent(s.Namespaces[i].Name) == id {
				return &Object{Namespace: &s.Namespaces[i]}
			}
		}
	case KindExtension:
		for i := range s.Extensions {
			if QuoteIdent(s.Extensions[i].Name) == id {
				return &Object{Extension: &s.Extensions[i]}
			}
		}
	case KindTable:
		if t := s.Table(id); t != nil {
			return &Object{Table: t}
		}
	case KindColumn:
		table, column := cutLast(id)
		if t := s.Table(table); t != nil {
			for i := range t.Columns {
				if QuoteIdent(t.Columns[i].Name) == column {
					return &Object{Column: &t.Columns[i]}
				}
			}
		}
	case KindType:
		return find(s.Types, (*Type).ID, id, func(t *Type) *Object { return &Object{Type: t} })
	case KindSequence:
		return find(s.Sequences, (*Sequence).ID, id, func(q *Sequence) *Object { return &Object{Sequence: q} })
	case KindConstraint:
		return find(s.Constraints, (*Constraint).ID, id, func(c *Constraint) *Object { return &Object{Constraint: c} })
	case KindIndex:
		return find(s.Indexes, (*Index).ID, id, func(x *Index) *Object { return &Object{Index: x} })
	case KindView:
		return find(s.Views, (*View).ID, id, func(v *View) *Object { return &Object{View: v} })
	case KindFunc:
		return find(s.Functions, (*Function).ID, id, func(f *Function) *Object { return &Object{Function: f} })
	case KindTrigger:
		return find(s.Triggers, (*Trigger).ID, id, func(t *Trigger) *Object { return &Object{Trigger: t} })
	case KindPolicy:
		return find(s.Policies, (*Policy).ID, id, func(p *Policy) *Object { return &Object{Policy: p} })
	case KindGrant:
		return find(s.Grants, (*Grant).ID, id, func(g *Grant) *Object { return &Object{Grant: g} })
	}
	return nil
}

func find[T any](list []T, id func(*T) string, want string, wrap func(*T) *Object) *Object {
	for i := range list {
		if id(&list[i]) == want {
			return wrap(&list[i])
		}
	}
	return nil
}

// Holds reports whether s is in the state a change leads to: an added
// object exists, a dropped one doesn't, an altered one matches its after
// state.
func (s *Schema) Holds(c Change) bool {
	current := s.Find(c.Kind, c.ID)
	switch c.Op {
	case OpAdd:
		return current != nil
	case OpDrop:
		return current == nil
	default:
		return current != nil && reflect.DeepEqual(current, c.After)
	}
}

// Undone reports whether s is back in the state before a change.
func (s *Schema) Undone(c Change) bool {
	return s.Holds(Change{Op: map[Op]Op{OpAdd: OpDrop, OpDrop: OpAdd, OpAlter: OpAlter}[c.Op], Kind: c.Kind, ID: c.ID, After: c.Before})
}
