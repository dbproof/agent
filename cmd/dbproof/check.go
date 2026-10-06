package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/dbproof/agent/check"
	"github.com/dbproof/agent/client"
	agentv1 "github.com/dbproof/agent/gen/dbproof/agent/v1"
	"github.com/dbproof/agent/snapshot"
)

// errUnavailable means DbProof couldn't be reached or didn't answer in time.
var errUnavailable = errors.New("dbproof unavailable")

// unavailableMessage is the annotation a fail-open check leaves.
const unavailableMessage = "DbProof unavailable, check skipped."

type checkFlags struct {
	project, database, postgresImage, migrationsDir, migrateCommand, prFiles, baseRef, repoRoot string
	verdictTimeout                                                                              time.Duration
	failOnUnreachable                                                                           bool
	pr                                                                                          pullRequest
}

// runCheck runs the pull request check and returns the exit code: 1 only
// when the migrations have errors, or when DbProof is unreachable and the
// workflow asked for -fail-on-unreachable.
func runCheck(args []string) int {
	var f checkFlags
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&f.project, "project", os.Getenv("DBPROOF_PROJECT"), "the DbProof project, org/slug")
	fs.StringVar(&f.database, "database", os.Getenv("DBPROOF_CHECK_DSN"), "postgres:// URL of an empty throwaway database, as a superuser; default: start -postgres-image in Docker")
	fs.StringVar(&f.postgresImage, "postgres-image", envOr("DBPROOF_POSTGRES_IMAGE", "postgres:{major}"), "the Docker image the check starts when there's no -database; {major} is production's Postgres major version")
	fs.StringVar(&f.migrationsDir, "migrations-dir", "", "the migrations folder")
	fs.StringVar(&f.migrateCommand, "migrate-command", "", "your migrate command, e.g. flyway -url=$DBPROOF_CHECK_JDBC_URL -user=$DBPROOF_CHECK_USER -password=$DBPROOF_CHECK_PASSWORD migrate")
	fs.StringVar(&f.prFiles, "pull-request-files", "", "comma-separated migration files the pull request adds or changes, relative to the working directory; default: git diff against -base-ref")
	fs.StringVar(&f.baseRef, "base-ref", os.Getenv("GITHUB_BASE_REF"), "the pull request's base branch")
	fs.StringVar(&f.repoRoot, "repo-root", envOr("GITHUB_WORKSPACE", "."), "the repository root, for file paths in annotations")
	fs.DurationVar(&f.verdictTimeout, "verdict-timeout", 3*time.Minute, "how long to wait for DbProof's verdict")
	fs.BoolVar(&f.failOnUnreachable, "fail-on-unreachable", false, "fail instead of passing with a warning when DbProof is unreachable")
	f.pr.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if f.project == "" || f.migrationsDir == "" || f.migrateCommand == "" {
		fmt.Fprintln(os.Stderr, "dbproof check: -project, -migrations-dir and -migrate-command are required")
		return 2
	}
	dbproofURL := os.Getenv("DBPROOF_URL")
	if dbproofURL == "" {
		fmt.Fprintln(os.Stderr, "dbproof check: DBPROOF_URL is not set")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	verdict, err := checkPullRequest(ctx, dbproofURL, f)
	if errors.Is(err, errUnavailable) {
		warn("%s", unavailableMessage)
		if f.failOnUnreachable {
			return 1
		}
		return 0
	}
	if err != nil {
		// A broken check is a setup problem, not the migrations' fault.
		warn("DbProof check couldn't run: %v", err)
		return 0
	}
	return report(os.Stdout, verdict)
}

func checkPullRequest(ctx context.Context, dbproofURL string, f checkFlags) (*agentv1.GetCheckVerdictResponse, error) {
	token := os.Getenv("DBPROOF_CHECK_TOKEN")
	if token == "" {
		var err error
		if token, err = client.ActionsOIDCToken(ctx); err != nil {
			return nil, err
		}
	}
	c := client.New(client.Options{BaseURL: dbproofURL, Token: token, Project: f.project})

	pr, err := f.pr.resolve()
	if err != nil {
		return nil, err
	}
	begin, err := c.Check.BeginCheck(ctx, connect.NewRequest(&agentv1.BeginCheckRequest{
		Project:           f.project,
		PullRequest:       pr,
		WorkflowRunId:     envInt64("GITHUB_RUN_ID"),
		RunAttempt:        envInt32("GITHUB_RUN_ATTEMPT"),
		AgentVersion:      version,
		FailOnUnreachable: f.failOnUnreachable,
	}))
	if err != nil {
		return nil, unreachable(err)
	}
	snap, err := snapshot.Decode(begin.Msg.GetSnapshot())
	if err != nil {
		return nil, err
	}
	tool := client.Tool(begin.Msg.GetTool())
	files, err := check.ListFiles(tool, f.migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	// Paths compare as absolute paths from here on.
	for i := range files {
		if files[i].Path, err = filepath.Abs(files[i].Path); err != nil {
			return nil, err
		}
	}
	prFiles, err := pullRequestFiles(ctx, f)
	if err != nil {
		return nil, err
	}

	if f.database == "" {
		// Production's major, so migrations behave as they will there.
		pg, err := startPostgres(ctx, f.postgresImage, snap.Schema.Major())
		if err != nil {
			return nil, err
		}
		defer pg.stop()
		f.database = pg.dsn
	}
	conn, err := pgx.Connect(ctx, f.database)
	if err != nil {
		return nil, fmt.Errorf("connect to the check database: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	var migrator check.Migrator = check.CommandMigrator{Command: f.migrateCommand, Tool: tool, DSN: f.database}
	if tool == snapshot.ToolPrisma || tool == snapshot.ToolDrizzle {
		dir, err := filepath.Abs(f.migrationsDir)
		if err != nil {
			return nil, err
		}
		migrator = check.Staged{Migrator: migrator, Tool: tool, Dir: dir, Files: files}
	}
	r, err := check.Run(ctx, check.Config{
		Conn: conn, Snapshot: snap, SnapshotLabel: begin.Msg.GetSnapshotVersion(), Tool: tool,
		Files: files, PullRequestFiles: prFiles, MigratorName: migratorName(f.migrateCommand),
		Migrator: migrator,
	})
	if err != nil {
		return nil, err
	}
	req, err := reportRequest(begin.Msg.GetCheckId(), r, f.repoRoot)
	if err != nil {
		return nil, err
	}
	if _, err := c.Check.ReportCheck(ctx, connect.NewRequest(req)); err != nil {
		return nil, unreachable(err)
	}
	return waitForVerdict(ctx, c, begin.Msg.GetCheckId(), f.verdictTimeout)
}

// waitForVerdict polls until DbProof has evaluated the check, or the wait
// runs out, which counts as DbProof being unavailable.
func waitForVerdict(ctx context.Context, c *client.Client, checkID string, timeout time.Duration) (*agentv1.GetCheckVerdictResponse, error) {
	deadline := time.Now().Add(timeout)
	for {
		resp, err := c.Check.GetCheckVerdict(ctx, connect.NewRequest(&agentv1.GetCheckVerdictRequest{CheckId: checkID}))
		if err != nil {
			return nil, unreachable(err)
		}
		if resp.Msg.GetReady() {
			return resp.Msg, nil
		}
		if time.Now().After(deadline) {
			return nil, errUnavailable
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// unreachable turns transport failures into errUnavailable; other errors,
// such as a rejected token, stay as they are.
func unreachable(err error) error {
	if isUnreachable(err) {
		return errUnavailable
	}
	return err
}

// isUnreachable reports whether err means DbProof is down, failing or slow,
// rather than refusing the request.
func isUnreachable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeUnknown, connect.CodeInternal:
		return true
	default:
		return errors.Is(err, context.DeadlineExceeded)
	}
}

// pullRequestFiles returns the migration files the pull request adds or
// changes, from the flag or from git.
func pullRequestFiles(ctx context.Context, f checkFlags) ([]string, error) {
	var names []string
	if f.prFiles != "" {
		names = strings.Split(f.prFiles, ",")
	} else {
		if f.baseRef == "" {
			return nil, errors.New("set -pull-request-files or -base-ref")
		}
		// Renamed files count: renumbering a migration is a rename.
		// Arguments, not a shell: the base branch can't start an option, and
		// -- ends them before the folder.
		//nolint:gosec // see above
		out, err := exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=AMR", "origin/"+f.baseRef+"...HEAD", "--", f.migrationsDir).Output()
		if err != nil {
			return nil, fmt.Errorf("git diff against %s (check out with fetch-depth: 0): %w", f.baseRef, err)
		}
		names = strings.Fields(string(out))
		// git names files from the repository's top level, which isn't the
		// workspace when the workflow checks out into a subfolder.
		top, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
		if err != nil {
			return nil, fmt.Errorf("find the repository root: %w", err)
		}
		root := strings.TrimSpace(string(top))
		for i, n := range names {
			names[i] = filepath.Join(root, n)
		}
	}
	var paths []string
	for _, n := range names {
		p := strings.TrimSpace(n)
		if p == "" {
			continue
		}
		// Relative paths resolve from the working directory, like
		// -migrations-dir; -repo-root only shortens paths in annotations.
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		paths = append(paths, abs)
	}
	return paths, nil
}

func migratorName(command string) string {
	fields := strings.Fields(command)
	for _, tool := range []struct{ bin, name string }{
		{"flyway", "flyway migrate"}, {"atlas", "atlas migrate apply"},
		{"prisma", "prisma migrate deploy"}, {"drizzle-kit", "drizzle-kit migrate"},
	} {
		for _, f := range fields {
			if strings.HasSuffix(f, tool.bin) || strings.Contains(f, "/"+tool.bin) {
				return tool.name
			}
		}
	}
	return "your migrate command"
}
