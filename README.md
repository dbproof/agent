# <img src="docs/logo.svg" width="36" height="36" align="absmiddle" alt=""> DbProof agent

[![CI](https://github.com/dbproof/agent/actions/workflows/ci.yml/badge.svg)](https://github.com/dbproof/agent/actions/workflows/ci.yml)

The open-source half of [DbProof](https://dbproof.dev), which tests every pull request's database migrations against a snapshot of production's schema and statistics. The agent runs in your GitHub Actions, so DbProof never connects to your database.

- **`capture`** connects with the connection string your migrations already use, reads production's schema, planner statistics and migration history, and uploads them as a snapshot.
- **`check`** restores the latest snapshot into a throwaway Postgres and applies the pull request's migrations one at a time. DbProof then reports on the pull request what would fail or lock in production.

PostgreSQL 13 and later, with Flyway, Atlas, Prisma Migrate or Drizzle Kit. Apache-2.0.

## What capture reads

- **The catalog:** tables, columns, indexes, constraints, views, functions, triggers, policies, roles and grants.
- **Planner estimates:** each table's row count, and each column's null fraction, distinct count and average width.
- **Your migration history table,** with Flyway's `installed_by` and Prisma's error `logs` replaced before upload.

It never reads rows from your tables, or the value samples in `pg_stats` (`most_common_vals`, `histogram_bounds`). Every query is in [`capture/capture.go`](capture/capture.go) and [`schema/inspect*.go`](schema).

## GitHub Actions

DbProof's setup pull request adds both, pinned by commit.

| Action | Runs | Authenticates with |
| --- | --- | --- |
| [`actions/check`](actions/check/action.yml) | On pull requests that change migrations | The job's GitHub OIDC token, so pull requests hold no DbProof secret |
| [`actions/capture`](actions/capture/action.yml) | Before and after each deploy, and every six hours | The project's upload-only capture token |

In your deploy job, capture goes on either side of the migration step:

```yaml
- uses: dbproof/agent/actions/capture@<commit> # v0.1.6
  with:
    kind: pre # and post, after your migrations
    dbproof-url: https://app.dbproof.dev
    capture-token: ${{ secrets.DBPROOF_CAPTURE_TOKEN }}
    capture-dsn: ${{ secrets.DBPROOF_CAPTURE_DSN }}
```

- Each action builds the agent from its own checkout, so what runs is exactly the commit you pinned.
- Capture never fails your deploy. If it can't run, it says why in a warning.
- Check runs on production's Postgres major version. If your migrations need an extension, set `postgres-image`, e.g. `pgvector/pgvector:pg{major}`.

## Development

```
just check            # starts Postgres 13 and 18, lints, runs every test
go test -short ./...  # skips the tests that need Postgres
DBPROOF_TEST_NODE_TOOLS=1 just check  # also runs the real Prisma and Drizzle Kit (needs Node.js)
```

`schema` inspects a Postgres schema from `pg_catalog`, diffs two schemas offline and generates DDL ([how and why](docs/schema-engine.md)). `snapshot` is the format uploaded to DbProof.

Test files follow the source files: `diff_test.go` tests `diff.go`, tests that need Postgres go in `diff_db_test.go` beside it, and a package's shared test helpers in `helpers_test.go`.
