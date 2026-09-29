// Package schema inspects a Postgres schema from pg_catalog, diffs two schemas
// offline and generates the DDL that recreates or changes one.
//
// Definitions are stored as Postgres renders them (pg_get_viewdef,
// pg_get_indexdef and so on) with an empty search_path, so every user object
// is schema-qualified and two inspections of the same schema on the same
// major version produce identical text.
package schema

// Schema is everything captured about one database's schema. Slices are sorted
// by identity so that encoding the same schema twice gives the same bytes.
type Schema struct {
	// ServerVersionNum is the server's server_version_num, e.g. 150006.
	ServerVersionNum int32       `json:"server_version_num"`
	Extensions       []Extension `json:"extensions,omitempty"`
	Namespaces       []Namespace `json:"namespaces,omitempty"`
	// Roles lists the role names that grants and policies reference. Restores
	// create them as empty NOLOGIN roles.
	Roles       []string     `json:"roles,omitempty"`
	Types       []Type       `json:"types,omitempty"`
	Sequences   []Sequence   `json:"sequences,omitempty"`
	Tables      []Table      `json:"tables,omitempty"`
	Views       []View       `json:"views,omitempty"`
	Functions   []Function   `json:"functions,omitempty"`
	Constraints []Constraint `json:"constraints,omitempty"`
	Indexes     []Index      `json:"indexes,omitempty"`
	Triggers    []Trigger    `json:"triggers,omitempty"`
	Policies    []Policy     `json:"policies,omitempty"`
	Grants      []Grant      `json:"grants,omitempty"`
}

// Major returns the Postgres major version, e.g. 15.
func (s *Schema) Major() int32 { return s.ServerVersionNum / 10000 }

type Extension struct {
	Name    string `json:"name"`
	Schema  string `json:"schema"`
	Version string `json:"version"`
}

type Namespace struct {
	Name string `json:"name"`
}

type TypeKind string

const (
	TypeEnum      TypeKind = "enum"
	TypeDomain    TypeKind = "domain"
	TypeComposite TypeKind = "composite"
)

type Type struct {
	Schema string   `json:"schema"`
	Name   string   `json:"name"`
	Kind   TypeKind `json:"kind"`
	// Labels holds enum labels in sort order.
	Labels []string `json:"labels,omitempty"`
	// BaseType, NotNull, Default, Collation and Checks describe a domain.
	BaseType  string  `json:"base_type,omitempty"`
	NotNull   bool    `json:"not_null,omitempty"`
	Default   string  `json:"default,omitempty"`
	Collation string  `json:"collation,omitempty"`
	Checks    []Check `json:"checks,omitempty"`
	// Attributes describe a composite type.
	Attributes []Attribute `json:"attributes,omitempty"`
	// DependsOn lists the identities of types this type uses.
	DependsOn []string `json:"depends_on,omitempty"`
}

type Check struct {
	Name       string `json:"name"`
	Definition string `json:"definition"`
}

type Attribute struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type Sequence struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	DataType  string `json:"data_type"`
	Start     int64  `json:"start"`
	Increment int64  `json:"increment"`
	Min       int64  `json:"min"`
	Max       int64  `json:"max"`
	Cache     int64  `json:"cache"`
	Cycle     bool   `json:"cycle,omitempty"`
	// OwnedBy is "schema.table.column" for serial columns.
	OwnedBy string `json:"owned_by,omitempty"`
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Partitioned is true for a partitioned parent; PartitionKey is its key.
	Partitioned  bool   `json:"partitioned,omitempty"`
	PartitionKey string `json:"partition_key,omitempty"`
	// PartitionOf is the parent's identity for a partition, and
	// PartitionBound its "FOR VALUES ..." or "DEFAULT" clause.
	PartitionOf    string   `json:"partition_of,omitempty"`
	PartitionBound string   `json:"partition_bound,omitempty"`
	Unlogged       bool     `json:"unlogged,omitempty"`
	Options        []string `json:"options,omitempty"`
	RLSEnabled     bool     `json:"rls_enabled,omitempty"`
	RLSForced      bool     `json:"rls_forced,omitempty"`
	Columns        []Column `json:"columns"`
}

type Column struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	NotNull   bool   `json:"not_null,omitempty"`
	Default   string `json:"default,omitempty"`
	Collation string `json:"collation,omitempty"`
	// Identity is "always" or "by default" for identity columns.
	Identity string `json:"identity,omitempty"`
	// Generated is "stored" or "virtual", with the expression in Default.
	Generated string `json:"generated,omitempty"`
}

type View struct {
	Schema       string   `json:"schema"`
	Name         string   `json:"name"`
	Materialized bool     `json:"materialized,omitempty"`
	Definition   string   `json:"definition"`
	Options      []string `json:"options,omitempty"`
	// DependsOn lists the identities of the relations and functions the view uses.
	DependsOn []string `json:"depends_on,omitempty"`
}

type FunctionKind string

const (
	KindFunction  FunctionKind = "function"
	KindProcedure FunctionKind = "procedure"
	KindAggregate FunctionKind = "aggregate"
)

type Function struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Args is pg_get_function_identity_arguments; together with Schema and
	// Name it identifies an overload.
	Args       string       `json:"args"`
	Kind       FunctionKind `json:"kind"`
	Definition string       `json:"definition"`
	// DependsOn lists relations and functions the signature or a SQL-standard
	// body references; such functions are created after tables and views.
	DependsOn []string `json:"depends_on,omitempty"`
}

type ConstraintType string

const (
	ConstraintPrimaryKey ConstraintType = "primary_key"
	ConstraintUnique     ConstraintType = "unique"
	ConstraintCheck      ConstraintType = "check"
	ConstraintForeignKey ConstraintType = "foreign_key"
	ConstraintExclusion  ConstraintType = "exclusion"
)

type Constraint struct {
	Table      string         `json:"table"`
	Name       string         `json:"name"`
	Type       ConstraintType `json:"type"`
	Definition string         `json:"definition"`
	Columns    []string       `json:"columns,omitempty"`
	// RefTable and RefColumns are set for foreign keys.
	RefTable   string   `json:"ref_table,omitempty"`
	RefColumns []string `json:"ref_columns,omitempty"`
}

type Index struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
	Unique     bool   `json:"unique,omitempty"`
	// Columns lists key columns in order; expression keys appear as "".
	Columns []string `json:"columns,omitempty"`
	Partial bool     `json:"partial,omitempty"`
	// Invalid marks an index left behind by a failed CREATE INDEX CONCURRENTLY.
	Invalid bool `json:"invalid,omitempty"`
}

type Trigger struct {
	Table      string `json:"table"`
	Name       string `json:"name"`
	Definition string `json:"definition"`
	// Enabled is "origin" (the default), "disabled", "replica" or "always".
	Enabled string `json:"enabled"`
}

type Policy struct {
	Table      string   `json:"table"`
	Name       string   `json:"name"`
	Permissive bool     `json:"permissive"`
	Command    string   `json:"command"`
	Roles      []string `json:"roles"`
	Using      string   `json:"using,omitempty"`
	WithCheck  string   `json:"with_check,omitempty"`
}

// ObjectKind names what a Grant is on.
type ObjectKind string

const (
	ObjectSchema   ObjectKind = "schema"
	ObjectTable    ObjectKind = "table"
	ObjectSequence ObjectKind = "sequence"
	ObjectFunction ObjectKind = "function"
	ObjectType     ObjectKind = "type"
	ObjectColumn   ObjectKind = "column"
)

// Grant is one privilege on one object. Privileges an object's owner holds
// implicitly are not listed; PUBLIC's default privileges on functions and
// types are, so revoking them shows up as a missing grant.
type Grant struct {
	ObjectKind ObjectKind `json:"object_kind"`
	Object     string     `json:"object"`
	Grantee    string     `json:"grantee"`
	Privilege  string     `json:"privilege"`
	Grantable  bool       `json:"grantable,omitempty"`
}
