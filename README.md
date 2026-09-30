# DbProof agent

The open-source agent that DbProof customers run in their own CI or cluster. It captures the production schema and planner statistics, and runs pull request checks against a restored snapshot. It never reads rows from application tables.

Licensed under Apache-2.0 so security teams can audit exactly what runs in their network.

## GitHub Actions

DbProof's setup pull request adds workflows that use two actions from this repository, pinned by commit:

- `actions/check` runs on pull requests: it restores the latest snapshot into the job's throwaway Postgres, applies the pull request's migrations one version at a time with your migrate command, and reports DbProof's verdict. It authenticates with the job's GitHub OIDC token (`permissions: id-token: write`), so pull request workflows hold no DbProof secret.
- `actions/capture` runs on a schedule and around deploys: it captures the schema, the planner's statistics and the migration history, using the connection your migrations run with, and uploads them with the project's capture token. It never reads a row of your application's tables, and never fails the job.

Each builds the agent from its own checkout with the Go version in `go.mod`, so what runs is exactly the pinned commit.

While this repository is private, a repository can only use its actions if Settings → Actions → General → Access here allows repositories owned by the same account.

## Packages

- `schema`: inspects a Postgres schema from `pg_catalog`, diffs two schemas offline and generates DDL.
- `snapshot`: the versioned snapshot format uploaded to DbProof.

See [docs/schema-engine.md](docs/schema-engine.md) for how the schema engine works and why it isn't built on a library.

## Development

```
just check   # starts Postgres 13 and 18, lints, runs every test
```

`go test -short ./...` skips tests that need Postgres.

Test files follow the source files: `diff_test.go` tests `diff.go`. Tests that need Postgres go in `diff_db_test.go` beside it, and helpers shared by a package's tests in `helpers_test.go`.
