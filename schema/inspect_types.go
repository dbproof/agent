package schema

import (
	"context"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// aggregateDef builds CREATE AGGREGATE for a plain aggregate, since
// pg_get_functiondef rejects aggregates. Ordered-set and hypothetical-set
// aggregates aren't supported.
const aggregateDef = `'CREATE AGGREGATE ' || pg_catalog.quote_ident(n.nspname) || '.' || pg_catalog.quote_ident(p.proname)
	|| '(' || pg_catalog.pg_get_function_arguments(p.oid) || ') (SFUNC = ' || a.aggtransfn::regproc::text
	|| ', STYPE = ' || pg_catalog.format_type(a.aggtranstype, NULL)
	|| CASE WHEN a.aggfinalfn <> 0 THEN ', FINALFUNC = ' || a.aggfinalfn::regproc::text ELSE '' END
	|| CASE WHEN a.aggcombinefn <> 0 THEN ', COMBINEFUNC = ' || a.aggcombinefn::regproc::text ELSE '' END
	|| CASE WHEN a.agginitval IS NOT NULL THEN ', INITCOND = ' || pg_catalog.quote_literal(a.agginitval) ELSE '' END
	|| CASE p.proparallel WHEN 's' THEN ', PARALLEL = SAFE' WHEN 'r' THEN ', PARALLEL = RESTRICTED' ELSE '' END
	|| ')'`

func (in *inspector) types(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT t.oid, n.nspname, t.typname, t.typtype::text,
		       CASE WHEN t.typtype = 'd' THEN pg_catalog.format_type(t.typbasetype, t.typtypmod) END,
		       t.typnotnull, coalesce(t.typdefault, ''),
		       coalesce((SELECT pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(co.collname)
		                 FROM pg_catalog.pg_collation co JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
		                 JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype
		                 WHERE t.typtype = 'd' AND co.oid = t.typcollation AND t.typcollation <> bt.typcollation), ''),
		       coalesce((SELECT array_agg(e.enumlabel ORDER BY e.enumsortorder)
		                 FROM pg_catalog.pg_enum e WHERE e.enumtypid = t.oid), '{}')
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
		LEFT JOIN pg_catalog.pg_class c ON c.oid = t.typrelid
		WHERE n.nspname = ANY($1)
		  AND (t.typtype IN ('e', 'd') OR (t.typtype = 'c' AND c.relkind = 'c'))
		  AND `+extFilter("pg_type", "t.oid"), in.schemas)
	if err != nil {
		return err
	}
	var oids []uint32
	var types []Type
	for rows.Next() {
		var (
			oid      uint32
			t        Type
			typtype  string
			baseType *string
		)
		if err := rows.Scan(&oid, &t.Schema, &t.Name, &typtype, &baseType, &t.NotNull, &t.Default, &t.Collation, &t.Labels); err != nil {
			return err
		}
		switch typtype {
		case "e":
			t.Kind = TypeEnum
		case "d":
			t.Kind = TypeDomain
			t.BaseType = *baseType
		default:
			t.Kind = TypeComposite
			t.Labels = nil
		}
		oids = append(oids, oid)
		types = append(types, t)
		in.typeID[oid] = t.ID()
	}
	if err := rows.Err(); err != nil {
		return err
	}
	in.typeOIDs = oids

	byOID := make(map[uint32]*Type, len(oids))
	for i := range types {
		byOID[oids[i]] = &types[i]
	}
	if err := in.domainChecks(ctx, byOID); err != nil {
		return err
	}
	if err := in.compositeAttributes(ctx, byOID); err != nil {
		return err
	}
	if err := in.typeDependencies(ctx, byOID); err != nil {
		return err
	}
	in.out.Types = types
	return nil
}

func (in *inspector) domainChecks(ctx context.Context, byOID map[uint32]*Type) error {
	rows, err := in.tx.Query(ctx, `
		SELECT c.contypid, c.conname, pg_catalog.pg_get_constraintdef(c.oid)
		FROM pg_catalog.pg_constraint c
		WHERE c.contypid = ANY($1::oid[]) AND c.contype = 'c'
		ORDER BY c.conname`, in.typeOIDs)
	if err != nil {
		return err
	}
	var oid uint32
	var check Check
	_, err = pgx.ForEachRow(rows, []any{&oid, &check.Name, &check.Definition}, func() error {
		t := byOID[oid]
		t.Checks = append(t.Checks, check)
		return nil
	})
	return err
}

func (in *inspector) compositeAttributes(ctx context.Context, byOID map[uint32]*Type) error {
	rows, err := in.tx.Query(ctx, `
		SELECT t.oid, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod)
		FROM pg_catalog.pg_type t
		JOIN pg_catalog.pg_attribute a ON a.attrelid = t.typrelid
		WHERE t.oid = ANY($1::oid[]) AND t.typtype = 'c' AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY t.oid, a.attnum`, in.typeOIDs)
	if err != nil {
		return err
	}
	var oid uint32
	var attr Attribute
	_, err = pgx.ForEachRow(rows, []any{&oid, &attr.Name, &attr.Type}, func() error {
		t := byOID[oid]
		t.Attributes = append(t.Attributes, attr)
		return nil
	})
	return err
}

// typeDependencies records which captured types each type uses, through a
// domain's base type or a composite's attributes, looking through arrays.
func (in *inspector) typeDependencies(ctx context.Context, byOID map[uint32]*Type) error {
	rows, err := in.tx.Query(ctx, `
		WITH deps AS (
			SELECT d.objid AS type_oid, d.refobjid AS ref
			FROM pg_catalog.pg_depend d
			WHERE d.classid = 'pg_type'::regclass AND d.refclassid = 'pg_type'::regclass
			  AND d.objid = ANY($1::oid[]) AND d.deptype = 'n'
			UNION
			SELECT t.oid, d.refobjid
			FROM pg_catalog.pg_type t
			JOIN pg_catalog.pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = t.typrelid
			WHERE t.oid = ANY($1::oid[]) AND t.typtype = 'c' AND d.refclassid = 'pg_type'::regclass
		)
		SELECT deps.type_oid,
		       CASE WHEN rt.typelem <> 0 AND rt.typlen = -1 THEN rt.typelem ELSE rt.oid END
		FROM deps JOIN pg_catalog.pg_type rt ON rt.oid = deps.ref`, in.typeOIDs)
	if err != nil {
		return err
	}
	var oid, ref uint32
	_, err = pgx.ForEachRow(rows, []any{&oid, &ref}, func() error {
		if id, ok := in.typeID[ref]; ok && ref != oid {
			t := byOID[oid]
			t.DependsOn = appendUnique(t.DependsOn, id)
		}
		return nil
	})
	return err
}

func (in *inspector) functions(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT p.oid, n.nspname, p.proname, pg_catalog.pg_get_function_identity_arguments(p.oid), p.prokind::text,
		       CASE WHEN p.prokind = 'a' THEN `+aggregateDef+` ELSE pg_catalog.pg_get_functiondef(p.oid) END
		FROM pg_catalog.pg_proc p
		JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		LEFT JOIN pg_catalog.pg_aggregate a ON a.aggfnoid = p.oid
		WHERE n.nspname = ANY($1)
		  AND (p.prokind IN ('f', 'p', 'w') OR (p.prokind = 'a' AND a.aggkind = 'n'))
		  AND `+extFilter("pg_proc", "p.oid"), in.schemas)
	if err != nil {
		return err
	}
	var funcs []Function
	var oids []uint32
	for rows.Next() {
		var oid uint32
		var f Function
		var kind string
		if err := rows.Scan(&oid, &f.Schema, &f.Name, &f.Args, &kind, &f.Definition); err != nil {
			return err
		}
		switch kind {
		case "p":
			f.Kind = KindProcedure
		case "a":
			f.Kind = KindAggregate
		default:
			f.Kind = KindFunction
		}
		f.Definition = strings.TrimRight(f.Definition, "\n")
		// Postgres 14 started printing the default IN mode for procedure
		// arguments, in the identity and the definition's header; without
		// it both read the same on every major.
		if args := inMode.ReplaceAllString(f.Args, "$1"); args != f.Args {
			f.Definition = strings.Replace(f.Definition, "("+f.Args+")", "("+args+")", 1)
			f.Args = args
		}
		oids = append(oids, oid)
		funcs = append(funcs, f)
		in.funcID[oid] = f.ID()
	}
	if err := rows.Err(); err != nil {
		return err
	}
	in.funcOIDs = oids

	byOID := make(map[uint32]*Function, len(oids))
	for i := range funcs {
		byOID[oids[i]] = &funcs[i]
	}
	// A function depends on a relation when its signature uses the relation's
	// row type, or its SQL-standard body references the relation.
	deps, err := in.tx.Query(ctx, `
		SELECT d.objid, d.refclassid = 'pg_proc'::regclass, d.refclassid = 'pg_type'::regclass, d.refobjid
		FROM pg_catalog.pg_depend d
		WHERE d.classid = 'pg_proc'::regclass AND d.objid = ANY($1::oid[]) AND d.deptype = 'n'
		  AND d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass, 'pg_type'::regclass)`, oids)
	if err != nil {
		return err
	}
	var oid, ref uint32
	var isFunc, isType bool
	_, err = pgx.ForEachRow(deps, []any{&oid, &isFunc, &isType, &ref}, func() error {
		var id string
		switch {
		case isFunc:
			id = in.funcID[ref]
		case isType:
			id = in.rowType[ref]
		default:
			id = in.relID[ref]
		}
		if id != "" && ref != oid {
			f := byOID[oid]
			f.DependsOn = appendUnique(f.DependsOn, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	in.out.Functions = funcs
	return nil
}

func appendUnique(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// inMode matches an explicit IN mode at the start of an argument. Argument
// names are lowercase unless quoted, so an uppercase IN is always the mode.
var inMode = regexp.MustCompile(`(^|, )IN `)
