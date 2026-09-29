package schema

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Options controls what Inspect reports.
type Options struct {
	// Exclude holds patterns of the form "schema.*" or "schema.table". An
	// excluded relation takes everything that depends on it with it.
	Exclude []string
	// IgnoreGrantees lists roles whose grants are left out, such as a test
	// harness's own role.
	IgnoreGrantees []string
}

// Beginner starts a transaction; *pgx.Conn and *pgxpool.Pool satisfy it.
type Beginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Inspect reads the schema of the connected database from pg_catalog. It
// needs no privileges on application tables and never reads their rows.
func Inspect(ctx context.Context, db Beginner, opts Options) (*Schema, error) {
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin inspection: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// An empty search_path makes every pg_get_*def schema-qualify user objects.
	// The lock timeout keeps a capture from queueing behind a migration.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = ''; SET LOCAL lock_timeout = '10s'"); err != nil {
		return nil, fmt.Errorf("configure inspection: %w", err)
	}
	in := &inspector{
		tx:      tx,
		out:     &Schema{},
		opts:    opts,
		relID:   map[uint32]string{},
		rowType: map[uint32]string{},
		typeID:  map[uint32]string{},
		funcID:  map[uint32]string{},
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"version", in.version},
		{"schemas", in.namespaces},
		{"extensions", in.extensions},
		{"types", in.types},
		{"sequences", in.sequences},
		{"tables", in.tables},
		{"views", in.views},
		{"functions", in.functions},
		{"view dependencies", in.viewDependencies},
		{"constraints", in.constraints},
		{"indexes", in.indexes},
		{"triggers", in.triggers},
		{"policies", in.policies},
		{"grants", in.grants},
	}
	for _, step := range steps {
		if err := step.fn(ctx); err != nil {
			return nil, fmt.Errorf("inspect %s: %w", step.name, err)
		}
	}
	applyExclusions(in.out, opts.Exclude)
	in.out.Roles = referencedRoles(in.out)
	Normalize(in.out)
	return in.out, nil
}

type inspector struct {
	tx      pgx.Tx
	out     *Schema
	opts    Options
	schemas []string
	// relOIDs and the maps below translate catalog OIDs into identities for
	// the objects this inspection captured.
	relOIDs  []uint32
	relID    map[uint32]string
	rowType  map[uint32]string
	typeOIDs []uint32
	typeID   map[uint32]string
	funcOIDs []uint32
	funcID   map[uint32]string
}

// notExtensionMember filters out objects that belong to an extension.
const notExtensionMember = `NOT EXISTS (
	SELECT 1 FROM pg_catalog.pg_depend d
	WHERE d.classid = '%s'::regclass AND d.objid = %s AND d.deptype = 'e')`

func extFilter(catalog, oidExpr string) string {
	return fmt.Sprintf(notExtensionMember, catalog, oidExpr)
}

func (in *inspector) version(ctx context.Context) error {
	var num int
	if err := in.tx.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		return err
	}
	in.out.ServerVersionNum = num
	return nil
}

func (in *inspector) namespaces(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT n.nspname FROM pg_catalog.pg_namespace n
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname !~ '^pg_(toast|temp_|toast_temp_)'
		  AND `+extFilter("pg_namespace", "n.oid"))
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, name := range names {
		if slices.Contains(in.opts.Exclude, name+".*") {
			continue
		}
		in.schemas = append(in.schemas, name)
		in.out.Namespaces = append(in.out.Namespaces, Namespace{Name: name})
	}
	return nil
}

func (in *inspector) extensions(ctx context.Context) error {
	rows, err := in.tx.Query(ctx, `
		SELECT e.extname, n.nspname, e.extversion
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace`)
	if err != nil {
		return err
	}
	exts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Extension, error) {
		var e Extension
		err := r.Scan(&e.Name, &e.Schema, &e.Version)
		return e, err
	})
	in.out.Extensions = exts
	return err
}

// grantee renders an aclexplode grantee OID as a role name.
const granteeName = `CASE WHEN %[1]s = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(%[1]s)::text END`
