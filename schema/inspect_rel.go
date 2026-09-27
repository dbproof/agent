package schema

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (in *inspector) sequences(ctx context.Context) error {
	// Identity columns own their sequences internally (deptype 'i'); the column
	// definition covers them.
	rows, err := in.tx.Query(ctx, `
		SELECT n.nspname, c.relname, pg_catalog.format_type(s.seqtypid, NULL),
		       s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
		       coalesce(pg_catalog.quote_ident(tn.nspname) || '.' || pg_catalog.quote_ident(tc.relname)
		                || '.' || pg_catalog.quote_ident(a.attname), '')
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_sequence s ON s.seqrelid = c.oid
		LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = c.oid
		     AND d.refclassid = 'pg_class'::regclass AND d.deptype = 'a'
		LEFT JOIN pg_catalog.pg_class tc ON tc.oid = d.refobjid
		LEFT JOIN pg_catalog.pg_namespace tn ON tn.oid = tc.relnamespace
		LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE c.relkind = 'S' AND n.nspname = ANY($1)
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend i
		                  WHERE i.classid = 'pg_class'::regclass AND i.objid = c.oid AND i.deptype = 'i')
		  AND `+extFilter("pg_class", "c.oid"), in.schemas)
	if err != nil {
		return err
	}
	seqs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sequence, error) {
		var s Sequence
		err := r.Scan(&s.Schema, &s.Name, &s.DataType, &s.Start, &s.Increment, &s.Min, &s.Max, &s.Cache, &s.Cycle, &s.OwnedBy)
		return s, err
	})
	in.out.Sequences = seqs
	return err
}

func (in *inspector) tables(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT c.oid, c.reltype, n.nspname, c.relname, c.relkind = 'p',
		       CASE WHEN c.relkind = 'p' THEN pg_catalog.pg_get_partkeydef(c.oid) ELSE '' END,
		       coalesce(pg_catalog.quote_ident(pn.nspname) || '.' || pg_catalog.quote_ident(pc.relname), ''),
		       CASE WHEN c.relispartition THEN pg_catalog.pg_get_expr(c.relpartbound, c.oid) ELSE '' END,
		       c.relpersistence = 'u', coalesce(c.reloptions, '{}'), c.relrowsecurity, c.relforcerowsecurity
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_catalog.pg_inherits i ON c.relispartition AND i.inhrelid = c.oid
		LEFT JOIN pg_catalog.pg_class pc ON pc.oid = i.inhparent
		LEFT JOIN pg_catalog.pg_namespace pn ON pn.oid = pc.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ANY($1)
		  AND `+extFilter("pg_class", "c.oid"), in.schemas)
	if err != nil {
		return err
	}
	var tables []Table
	var oids []uint32
	for rows.Next() {
		var oid, reltype uint32
		var t Table
		if err := rows.Scan(&oid, &reltype, &t.Schema, &t.Name, &t.Partitioned, &t.PartitionKey, &t.PartitionOf,
			&t.PartitionBound, &t.Unlogged, &t.Options, &t.RLSEnabled, &t.RLSForced); err != nil {
			return err
		}
		oids = append(oids, oid)
		tables = append(tables, t)
		in.relID[oid] = t.ID()
		in.rowType[reltype] = t.ID()
	}
	if err := rows.Err(); err != nil {
		return err
	}
	in.relOIDs = append(in.relOIDs, oids...)

	byOID := make(map[uint32]*Table, len(oids))
	for i := range tables {
		byOID[oids[i]] = &tables[i]
	}
	cols, err := in.tx.Query(ctx, `
		SELECT a.attrelid, a.attname, pg_catalog.format_type(a.atttypid, a.atttypmod), a.attnotnull,
		       coalesce(pg_catalog.pg_get_expr(ad.adbin, ad.adrelid), ''),
		       coalesce((SELECT pg_catalog.quote_ident(cn.nspname) || '.' || pg_catalog.quote_ident(co.collname)
		                 FROM pg_catalog.pg_collation co JOIN pg_catalog.pg_namespace cn ON cn.oid = co.collnamespace
		                 WHERE co.oid = a.attcollation AND a.attcollation <> t.typcollation), ''),
		       a.attidentity::text, a.attgenerated::text
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		LEFT JOIN pg_catalog.pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE a.attrelid = ANY($1::oid[]) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attrelid, a.attnum`, oids)
	if err != nil {
		return err
	}
	var oid uint32
	var col Column
	var identity, generated string
	_, err = pgx.ForEachRow(cols, []any{&oid, &col.Name, &col.Type, &col.NotNull, &col.Default, &col.Collation, &identity, &generated}, func() error {
		c := col
		switch identity {
		case "a":
			c.Identity = "always"
		case "d":
			c.Identity = "by default"
		}
		switch generated {
		case "s":
			c.Generated = "stored"
		case "v":
			c.Generated = "virtual"
		}
		t := byOID[oid]
		t.Columns = append(t.Columns, c)
		return nil
	})
	if err != nil {
		return err
	}
	in.out.Tables = tables
	return nil
}

func (in *inspector) views(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT c.oid, c.reltype, n.nspname, c.relname, c.relkind = 'm',
		       pg_catalog.pg_get_viewdef(c.oid, true), coalesce(c.reloptions, '{}')
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('v', 'm') AND n.nspname = ANY($1)
		  AND `+extFilter("pg_class", "c.oid"), in.schemas)
	if err != nil {
		return err
	}
	var views []View
	var oids []uint32
	for rows.Next() {
		var oid, reltype uint32
		var v View
		if err := rows.Scan(&oid, &reltype, &v.Schema, &v.Name, &v.Materialized, &v.Definition, &v.Options); err != nil {
			return err
		}
		v.Definition = strings.TrimSuffix(strings.TrimSpace(v.Definition), ";")
		oids = append(oids, oid)
		views = append(views, v)
		in.relID[oid] = v.ID()
		in.rowType[reltype] = v.ID()
	}
	if err := rows.Err(); err != nil {
		return err
	}
	in.relOIDs = append(in.relOIDs, oids...)
	in.out.Views = views
	return nil
}

// viewDependencies runs after functions so that views can depend on them.
func (in *inspector) viewDependencies(ctx context.Context) error {
	byID := make(map[string]*View, len(in.out.Views))
	for i := range in.out.Views {
		byID[in.out.Views[i].ID()] = &in.out.Views[i]
	}
	rows, err := in.tx.Query(ctx, `
		SELECT DISTINCT r.ev_class, d.refclassid = 'pg_proc'::regclass, d.refobjid
		FROM pg_catalog.pg_rewrite r
		JOIN pg_catalog.pg_depend d ON d.classid = 'pg_rewrite'::regclass AND d.objid = r.oid
		WHERE r.ev_class = ANY($1::oid[]) AND d.refobjid <> r.ev_class
		  AND d.refclassid IN ('pg_class'::regclass, 'pg_proc'::regclass)`, in.relOIDs)
	if err != nil {
		return err
	}
	var viewOID, ref uint32
	var isFunc bool
	_, err = pgx.ForEachRow(rows, []any{&viewOID, &isFunc, &ref}, func() error {
		v := byID[in.relID[viewOID]]
		if v == nil {
			return nil
		}
		id := in.relID[ref]
		if isFunc {
			id = in.funcID[ref]
		}
		if id != "" {
			v.DependsOn = appendUnique(v.DependsOn, id)
		}
		return nil
	})
	return err
}

func (in *inspector) constraints(ctx context.Context) error {
	// Constraints a partition inherits from its parent (conparentid, or not
	// local) are created by the parent's constraint and left out.
	rows, err := in.tx.Query(ctx, `
		SELECT c.conrelid, c.conname, c.contype::text, pg_catalog.pg_get_constraintdef(c.oid),
		       ARRAY(SELECT a.attname::text FROM unnest(c.conkey) WITH ORDINALITY k(num, ord)
		             JOIN pg_catalog.pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.num ORDER BY k.ord),
		       c.confrelid,
		       ARRAY(SELECT a.attname::text FROM unnest(c.confkey) WITH ORDINALITY k(num, ord)
		             JOIN pg_catalog.pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.num ORDER BY k.ord)
		FROM pg_catalog.pg_constraint c
		WHERE c.conrelid = ANY($1::oid[]) AND c.contype IN ('p', 'u', 'c', 'f', 'x')
		  AND c.conparentid = 0 AND c.conislocal`, in.relOIDs)
	if err != nil {
		return err
	}
	types := map[string]ConstraintType{
		"p": ConstraintPrimaryKey, "u": ConstraintUnique, "c": ConstraintCheck,
		"f": ConstraintForeignKey, "x": ConstraintExclusion,
	}
	var (
		relOID, refOID uint32
		con            Constraint
		contype        string
		cols, refCols  []string
	)
	_, err = pgx.ForEachRow(rows, []any{&relOID, &con.Name, &contype, &con.Definition, &cols, &refOID, &refCols}, func() error {
		c := con
		c.Table = in.relID[relOID]
		c.Type = types[contype]
		c.Columns = cols
		if c.Type == ConstraintForeignKey {
			c.RefTable = in.relID[refOID]
			if c.RefTable == "" {
				// The referenced table isn't captured, so the key can't be restored.
				return nil
			}
			c.RefColumns = refCols
		}
		in.out.Constraints = append(in.out.Constraints, c)
		return nil
	})
	return err
}

func (in *inspector) indexes(ctx context.Context) error {
	// Constraint-backed indexes come with their constraint, and a partition's
	// indexes attached to a parent index come with the parent's.
	rows, err := in.tx.Query(ctx, `
		SELECT i.indrelid, ic.relname, pg_catalog.pg_get_indexdef(i.indexrelid), ic.relkind = 'I',
		       i.indisunique, NOT i.indisvalid, i.indpred IS NOT NULL,
		       ARRAY(SELECT coalesce(a.attname::text, '') FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(num, ord)
		             LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.num
		             WHERE k.ord <= i.indnkeyatts ORDER BY k.ord)
		FROM pg_catalog.pg_index i
		JOIN pg_catalog.pg_class ic ON ic.oid = i.indexrelid
		WHERE i.indrelid = ANY($1::oid[])
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_constraint c
		                  WHERE c.conindid = i.indexrelid AND c.conrelid = i.indrelid AND c.contype IN ('p', 'u', 'x'))
		  AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_inherits h WHERE h.inhrelid = i.indexrelid)`, in.relOIDs)
	if err != nil {
		return err
	}
	var (
		relOID      uint32
		idx         Index
		partitioned bool
	)
	_, err = pgx.ForEachRow(rows, []any{&relOID, &idx.Name, &idx.Definition, &partitioned, &idx.Unique, &idx.Invalid, &idx.Partial, &idx.Columns}, func() error {
		x := idx
		x.Table = in.relID[relOID]
		if partitioned {
			// pg_get_indexdef says ON ONLY for a partitioned parent; restoring it
			// that way would skip the partitions.
			x.Definition = strings.Replace(x.Definition, " ON ONLY ", " ON ", 1)
		}
		in.out.Indexes = append(in.out.Indexes, x)
		return nil
	})
	return err
}

func (in *inspector) triggers(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT t.tgrelid, t.tgname, pg_catalog.pg_get_triggerdef(t.oid), t.tgenabled::text
		FROM pg_catalog.pg_trigger t
		WHERE t.tgrelid = ANY($1::oid[]) AND NOT t.tgisinternal AND t.tgparentid = 0`, in.relOIDs)
	if err != nil {
		return err
	}
	states := map[string]string{"O": "origin", "D": "disabled", "R": "replica", "A": "always"}
	var (
		relOID  uint32
		trg     Trigger
		enabled string
	)
	_, err = pgx.ForEachRow(rows, []any{&relOID, &trg.Name, &trg.Definition, &enabled}, func() error {
		t := trg
		t.Table = in.relID[relOID]
		t.Enabled = states[enabled]
		in.out.Triggers = append(in.out.Triggers, t)
		return nil
	})
	return err
}

func (in *inspector) policies(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT p.polrelid, p.polname, p.polpermissive, p.polcmd::text,
		       ARRAY(SELECT `+sprintfGrantee("r")+` FROM unnest(p.polroles) r ORDER BY 1),
		       coalesce(pg_catalog.pg_get_expr(p.polqual, p.polrelid), ''),
		       coalesce(pg_catalog.pg_get_expr(p.polwithcheck, p.polrelid), '')
		FROM pg_catalog.pg_policy p
		WHERE p.polrelid = ANY($1::oid[])`, in.relOIDs)
	if err != nil {
		return err
	}
	commands := map[string]string{"*": "ALL", "r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE"}
	var (
		relOID uint32
		pol    Policy
		cmd    string
	)
	_, err = pgx.ForEachRow(rows, []any{&relOID, &pol.Name, &pol.Permissive, &cmd, &pol.Roles, &pol.Using, &pol.WithCheck}, func() error {
		p := pol
		p.Table = in.relID[relOID]
		p.Command = commands[cmd]
		in.out.Policies = append(in.out.Policies, p)
		return nil
	})
	return err
}
