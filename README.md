# Stratum agent

The open-source agent that Stratum customers run in their own CI or cluster. It captures the production schema and planner statistics, and runs pull request checks against a restored snapshot. It never reads rows from application tables.

Licensed under Apache-2.0 so security teams can audit exactly what runs in their network.

## Packages

- `schema`: inspects a Postgres schema from `pg_catalog`, diffs two schemas offline and generates DDL.
- `snapshot`: the versioned snapshot format uploaded to Stratum.

See [docs/schema-engine.md](docs/schema-engine.md) for how the schema engine works and why it isn't built on a library.

## Development

```
just check   # starts Postgres 13 and 18, lints, runs every test
```

`go test -short ./...` skips tests that need Postgres.
