// Package check tests pull request migrations against a production snapshot
// in the customer's own runner: it restores the snapshot into a throwaway
// database, applies pending migrations one version at a time with the
// customer's migrate command, and records what each one changed.
package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/borovikovd/dbproof-agent/capture"
	"github.com/borovikovd/dbproof-agent/schema"
	"github.com/borovikovd/dbproof-agent/snapshot"
)

// Migrator applies migrations up to and including a version. CommandMigrator
// runs the customer's migrate command; tests use their own.
type Migrator interface {
	Migrate(ctx context.Context, target string) (output string, err error)
}

// Config is one check run.
type Config struct {
	// Conn is a superuser connection to an empty throwaway database.
	Conn     *pgx.Conn
	Snapshot *snapshot.Snapshot
	// SnapshotLabel is the snapshot's version, e.g. S-24, for step names.
	SnapshotLabel string
	Tool          snapshot.Tool
	// Files are the tool's migrations at the pull request's head.
	Files []File
	// PullRequestFiles are the paths of the files the pull request adds or
	// changes; other pending files were merged to the base branch.
	PullRequestFiles []string
	Migrator         Migrator
	// MigratorName names the migrate command in step details.
	MigratorName string
}

// Status is how a step ended.
type Status string

const (
	StatusOK           Status = "ok"
	StatusFailed       Status = "failed"
	StatusSkipped      Status = "skipped"
	StatusSetupProblem Status = "setup_problem"
)

// Step is one step of the run, as the console shows it.
type Step struct {
	Name     string
	Detail   string
	Status   Status
	Duration time.Duration
}

// Migration is one pending migration and what applying it did.
type Migration struct {
	File
	FromPullRequest bool
	SQL             string
	Applied         bool
	Error           string
	Changes         []schema.Change
}

// Report is everything the check sends to DbProof.
type Report struct {
	Steps              []Step
	SetupProblem       string
	Migrations         []Migration
	AppliedFileChanged []string
	ResultSchema       *schema.Schema
	Duration           time.Duration
}

// Run restores, verifies and migrates. Problems with the migrations end up in
// the report; an error means the check itself couldn't run.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	start := time.Now()
	r := &Report{}
	done := func() *Report { r.Duration = time.Since(start); return r }

	var major int32
	if err := cfg.Conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major); err != nil {
		return nil, fmt.Errorf("read server version: %w", err)
	}
	stepStart := time.Now()
	restoreName := "Restored snapshot " + cfg.SnapshotLabel
	if want := cfg.Snapshot.Schema.Major(); want != major {
		r.setup(restoreName, fmt.Sprintf("Production runs Postgres %d but the check database is Postgres %d. Use a Postgres %d service container.", want, major, want), stepStart)
		return done(), nil
	}
	if err := restore(ctx, cfg.Conn, cfg.Snapshot); err != nil {
		r.setup(restoreName, fmt.Sprintf("The snapshot didn't restore: %v", err), stepStart)
		return done(), nil
	}
	r.step(restoreName, fmt.Sprintf("Postgres %d · %s", major, since(stepStart)), StatusOK, stepStart)

	stepStart = time.Now()
	verifyName := "Verified the restore matches " + cfg.SnapshotLabel
	mismatch, err := verify(ctx, cfg.Conn, cfg.Snapshot)
	if err != nil {
		return nil, fmt.Errorf("verify restore: %w", err)
	}
	if mismatch != "" {
		r.setup(verifyName, "The restored schema didn't match "+cfg.SnapshotLabel+": "+mismatch+". This is a setup problem, not a migration failure.", stepStart)
		r.step("Skipped migrations and rules", "Nothing to test until the restore matches", StatusSkipped, time.Now())
		return done(), nil
	}
	r.step(verifyName, "Schema identical", StatusOK, stepStart)
	if err := atlasHistoryToDefault(ctx, &cfg); err != nil {
		return nil, err
	}

	applied := capture.Applied(cfg.Snapshot.History)
	// A pull request that edits a migration production already ran, or adds
	// one with a version it already ran, applies nothing here and would pass,
	// then fail on deploy.
	for _, f := range cfg.Files {
		if slices.Contains(applied, f.Version) && slices.Contains(cfg.PullRequestFiles, f.Path) {
			r.changedApplied(f.Version)
		}
	}
	var base, pr []Migration
	for _, f := range cfg.Files {
		if slices.Contains(applied, f.Version) {
			continue
		}
		sql, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, err
		}
		m := Migration{File: f, SQL: string(sql), FromPullRequest: slices.Contains(cfg.PullRequestFiles, f.Path)}
		if m.FromPullRequest {
			pr = append(pr, m)
		} else {
			base = append(base, m)
		}
	}

	current := cfg.Snapshot.Schema
	// failed is the version that failed to apply.
	failed := ""
	for _, group := range []struct {
		name       string
		migrations []Migration
	}{
		{"Applied merged, undeployed migrations", base},
		{"Applied pull request migrations", pr},
	} {
		stepStart = time.Now()
		if failed != "" {
			r.Migrations = append(r.Migrations, group.migrations...)
			r.step(group.name, "Skipped after a failure", StatusSkipped, stepStart)
			continue
		}
		if len(group.migrations) == 0 {
			r.step(group.name, "None", StatusOK, stepStart)
			continue
		}
		for i := range group.migrations {
			m := &group.migrations[i]
			next, changedFiles, err := r.apply(ctx, cfg, m, current)
			if errors.Is(err, errNotRecorded) {
				r.setup(group.name, fmt.Sprintf("The migrate command succeeded, but the check database's history doesn't show V%s, so it ran against another database. Point it at the check database: -url=$DBPROOF_CHECK_JDBC_URL for Flyway, --url \"$DBPROOF_CHECK_DSN\" for Atlas.", m.Version), stepStart)
				return done(), nil
			}
			if err != nil {
				return nil, err
			}
			for _, v := range changedFiles {
				r.changedApplied(v)
			}
			r.Migrations = append(r.Migrations, *m)
			if !m.Applied {
				// The rest aren't attempted, but still name the pull
				// request's versions.
				r.Migrations = append(r.Migrations, group.migrations[i+1:]...)
				failed = m.Version
				break
			}
			current = next
		}
		versions := make([]string, len(group.migrations))
		for i, m := range group.migrations {
			versions[i] = "V" + m.Version
		}
		detail := strings.Join(versions, ", ") + " · " + cfg.MigratorName
		status := StatusOK
		if failed != "" {
			detail, status = "V"+failed+" failed", StatusFailed
		}
		r.step(group.name, detail, status, stepStart)
	}
	r.ResultSchema = current
	return done(), nil
}

// apply runs the migrate command up to one migration and records what it
// changed.
func (r *Report) apply(ctx context.Context, cfg Config, m *Migration, before *schema.Schema) (*schema.Schema, []string, error) {
	out, err := cfg.Migrator.Migrate(ctx, m.Version)
	if err != nil {
		m.Error = lastLines(out, 20)
		if m.Error == "" {
			m.Error = err.Error()
		}
		return before, checksumMismatches(out), nil
	}
	after, err := schema.Inspect(ctx, cfg.Conn, schema.Options{})
	if err != nil {
		return nil, nil, fmt.Errorf("inspect after V%s: %w", m.Version, err)
	}
	recorded, err := historyShows(ctx, cfg, m.Version)
	if err != nil {
		return nil, nil, err
	}
	if !recorded {
		return before, nil, errNotRecorded
	}
	m.Applied = true
	var opts schema.DiffOptions
	if cfg.Snapshot.History != nil {
		opts.IgnoreTables = []string{cfg.Snapshot.History.Table}
	}
	m.Changes = schema.Diff(before, after, opts)
	return after, nil, nil
}

// errNotRecorded means the migrate command succeeded without applying the
// migration to the check database: it ran somewhere else.
var errNotRecorded = errors.New("migration not recorded in the check database")

// historyShows reports whether the check database's history table records
// version as applied. Without a history table there's nothing to check.
func historyShows(ctx context.Context, cfg Config, version string) (bool, error) {
	h := cfg.Snapshot.History
	if h == nil {
		return true, nil
	}
	sql := "SELECT EXISTS (SELECT 1 FROM " + h.Table + " WHERE version = $1)"
	if cfg.Tool == snapshot.ToolFlyway {
		sql = "SELECT EXISTS (SELECT 1 FROM " + h.Table + " WHERE version = $1 AND success)"
	}
	var ok bool
	if err := cfg.Conn.QueryRow(ctx, sql, version).Scan(&ok); err != nil {
		return false, fmt.Errorf("read the check database's history: %w", err)
	}
	return ok, nil
}

// changedApplied records a version whose applied migration file changed.
func (r *Report) changedApplied(version string) {
	if !slices.Contains(r.AppliedFileChanged, version) {
		r.AppliedFileChanged = append(r.AppliedFileChanged, version)
	}
}

var checksumMismatch = regexp.MustCompile(`(?i)checksum mismatch for migration version (\S+)`)

// checksumMismatches finds the versions a migrate command rejected because
// their file changed after being applied.
func checksumMismatches(output string) []string {
	var versions []string
	for _, m := range checksumMismatch.FindAllStringSubmatch(output, -1) {
		versions = append(versions, strings.TrimRight(m[1], ".,"))
	}
	if len(versions) == 0 && strings.Contains(strings.ToLower(output), "checksum mismatch") {
		versions = append(versions, "unknown")
	}
	return versions
}

func (r *Report) step(name, detail string, status Status, start time.Time) {
	r.Steps = append(r.Steps, Step{Name: name, Detail: detail, Status: status, Duration: time.Since(start)})
}

func (r *Report) setup(name, problem string, start time.Time) {
	r.SetupProblem = problem
	r.step(name, problem, StatusSetupProblem, start)
}

func since(t time.Time) string {
	return fmt.Sprintf("%ds", int(time.Since(t).Round(time.Second).Seconds()))
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ChangesJSON encodes a migration's changes the way the API carries them.
func (m Migration) ChangesJSON() ([]byte, error) {
	if m.Changes == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(m.Changes)
}
