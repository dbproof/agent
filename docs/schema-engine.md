# Schema engine

The `schema` package inspects a Postgres schema, diffs two schemas and generates DDL. Stratum's server imports the same package, so a capture, a check and a drift comparison all agree on what a schema is.

## Why not a library

We evaluated two Go libraries in September 2026. Neither meets the requirements below, so the engine is our own; both served as reading material, and no code was copied.

| Requirement | stripe/pg-schema-diff v1.0.9 | pgplex/pgschema v1.13.1 |
| --- | --- | --- |
| Schema model importable | No, `internal/schema` | Yes, `ir` |
| Diff two stored schemas without a database | No, every source is replayed into a temp database | Differ is pure but in `internal/diff` |
| Works for a role with no table privileges | Yes | No: tables and columns come from `information_schema`, which hides them |
| Postgres 13 through 18 | 14–17 | 14–18 |
| All schemas in one pass | Yes | One schema per call |
| Driver | pgx v4, lib/pq | database/sql |

## Requirements

- **Catalog only.** Inspection reads `pg_catalog` and nothing else, so the capture role needs no privileges on application tables and never sees a row. Statistics are separate: they come through the `stratum.table_stats()` security-definer function.
- **Offline diff.** The server diffs stored snapshots, so `Diff` compares two `Schema` values without a database.
- **Faithful restore.** A check restores the snapshot into an empty database. `RestoreDDL` must reproduce the schema exactly; the round-trip test proves it.
- **Every supported major.** Postgres 13 through 18.

## How it works

- **Definitions come from Postgres.** Views, functions, triggers, constraints, indexes, partition keys, defaults and policies are stored as `pg_get_*def` and `pg_get_expr` render them, inside a transaction with an empty `search_path`, so every user object is schema-qualified. Two inspections of the same schema on the same major give identical text, which is what makes a plain string comparison a correct diff.
- **Identity.** Objects are keyed by quoted, schema-qualified names (`public.invoices`, `public.invoices.due_date`, `public.touch(integer)`), quoted the same way as `quote_ident`.
- **Dependencies.** `pg_depend` gives view and function dependencies and marks extension members, which are skipped because `CREATE EXTENSION` recreates them. Restores create functions that don't touch relations before tables, with `check_function_bodies` off, and functions that do (through a row type or a SQL-standard body) together with views, in dependency order.
- **Partitions.** A partition's inherited constraints, indexes and triggers are skipped; the parent's definitions recreate them. `ON ONLY` is stripped from partitioned index definitions for the same reason.
- **Grants.** ACLs are expanded with `aclexplode`, using `acldefault` for a NULL ACL. Owner privileges are implicit and left out. PUBLIC's default privileges on functions and types are listed, so revoking them is a visible change. Referenced roles are recreated empty and `NOLOGIN`.
- **Exclusions.** A `schema.table` exclusion removes the table and everything that can't exist without it: partitions, views and functions that use it, foreign keys that reference it, and its grants. `schema.*` skips the schema.

## Known limits

- Across a Postgres major upgrade, view definitions aren't compared: 16 stopped qualifying a view's columns with their table, so the same view reads differently. Views added, dropped or changed in any other way still show. Procedure arguments are stored without the IN mode 14 started printing, so they match on every major. TestSameSchemaAcrossMajors captures the test schemas on each major and checks they diff to nothing.

- Not captured: object owners, comments, default privileges, range types, ordered-set aggregates, rules, event triggers, publications, foreign tables and non-partition inheritance (such a child is captured as a standalone table).
- A column default or domain check that calls a function which itself needs a table can't be restored in one pass.
- A generation expression can't be changed in place; `ChangeDDL` says so in a comment.
- Postgres can't remove an enum label; `ChangeDDL` says so in a comment.
