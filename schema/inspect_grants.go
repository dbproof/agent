package schema

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

func sprintfGrantee(expr string) string { return fmt.Sprintf(granteeName, expr) }

// grants reads privileges with aclexplode. A NULL ACL means the default for
// the object kind, so it's expanded with acldefault; privileges held by the
// object's owner are implicit and left out. Each query returns an OID (or
// zero), a name used to build the identity, then grantee, privilege and
// grantability.
func (in *inspector) grants(ctx context.Context) error {
	queries := []struct {
		kind  ObjectKind
		query string
		arg   any
	}{
		{ObjectTable, `
			SELECT c.oid, '', ` + sprintfGrantee("a.grantee") + `, a.privilege_type, a.is_grantable
			FROM pg_catalog.pg_class c, aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) a
			WHERE c.oid = ANY($1::oid[]) AND a.grantee <> c.relowner`, in.relOIDs},
		{ObjectColumn, `
			SELECT a.attrelid, a.attname, ` + sprintfGrantee("x.grantee") + `, x.privilege_type, x.is_grantable
			FROM pg_catalog.pg_attribute a
			JOIN pg_catalog.pg_class c ON c.oid = a.attrelid,
			     aclexplode(a.attacl) x
			WHERE a.attrelid = ANY($1::oid[]) AND a.attacl IS NOT NULL AND a.attnum > 0
			  AND NOT a.attisdropped AND x.grantee <> c.relowner`, in.relOIDs},
		{ObjectSequence, `
			SELECT 0::oid, pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(c.relname),
			       ` + sprintfGrantee("a.grantee") + `, a.privilege_type, a.is_grantable
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace,
			     aclexplode(coalesce(c.relacl, acldefault('s', c.relowner))) a
			WHERE c.relkind = 'S' AND n.nspname = ANY($1) AND a.grantee <> c.relowner
			  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend i
			                  WHERE i.classid = 'pg_class'::regclass AND i.objid = c.oid AND i.deptype = 'i')
			  AND ` + extFilter("pg_class", "c.oid"), in.schemas},
		{ObjectFunction, `
			SELECT p.oid, '', ` + sprintfGrantee("a.grantee") + `, a.privilege_type, a.is_grantable
			FROM pg_catalog.pg_proc p, aclexplode(coalesce(p.proacl, acldefault('f', p.proowner))) a
			WHERE p.oid = ANY($1::oid[]) AND a.grantee <> p.proowner`, in.funcOIDs},
		{ObjectType, `
			SELECT t.oid, '', ` + sprintfGrantee("a.grantee") + `, a.privilege_type, a.is_grantable
			FROM pg_catalog.pg_type t, aclexplode(coalesce(t.typacl, acldefault('T', t.typowner))) a
			WHERE t.oid = ANY($1::oid[]) AND a.grantee <> t.typowner`, in.typeOIDs},
		{ObjectSchema, `
			SELECT 0::oid, pg_catalog.quote_ident(n.nspname), ` + sprintfGrantee("a.grantee") + `, a.privilege_type, a.is_grantable
			FROM pg_catalog.pg_namespace n, aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a
			WHERE n.nspname = ANY($1) AND a.grantee <> n.nspowner`, in.schemas},
	}
	for _, q := range queries {
		rows, err := in.tx.Query(ctx, q.query, q.arg)
		if err != nil {
			return err
		}
		var (
			oid  uint32
			name string
			g    Grant
		)
		_, err = pgx.ForEachRow(rows, []any{&oid, &name, &g.Grantee, &g.Privilege, &g.Grantable}, func() error {
			if slices.Contains(in.opts.IgnoreGrantees, g.Grantee) {
				return nil
			}
			grant := g
			grant.ObjectKind = q.kind
			switch q.kind {
			case ObjectTable:
				grant.Object = in.relID[oid]
			case ObjectColumn:
				grant.Object = in.relID[oid] + "." + QuoteIdent(name)
			case ObjectFunction:
				grant.Object = in.funcID[oid]
			case ObjectType:
				grant.Object = in.typeID[oid]
			default:
				grant.Object = name
			}
			in.out.Grants = append(in.out.Grants, grant)
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// referencedRoles lists the roles that grants and policies name.
func referencedRoles(s *Schema) []string {
	var roles []string
	add := func(r string) {
		if r != "PUBLIC" && !slices.Contains(roles, r) {
			roles = append(roles, r)
		}
	}
	for _, g := range s.Grants {
		add(g.Grantee)
	}
	for _, p := range s.Policies {
		for _, r := range p.Roles {
			add(r)
		}
	}
	slices.Sort(roles)
	return roles
}
