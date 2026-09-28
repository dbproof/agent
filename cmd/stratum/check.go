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

	"github.com/borovikovd/stratum-agent/check"
	"github.com/borovikovd/stratum-agent/client"
	agentv1 "github.com/borovikovd/stratum-agent/gen/stratum/agent/v1"
	"github.com/borovikovd/stratum-agent/snapshot"
)

// errUnavailable means Stratum couldn't be reached or didn't answer in time.
var errUnavailable = errors.New("stratum unavailable")

// unavailableMessage is the annotation a fail-open check leaves.
const unavailableMessage = "Stratum unavailable, check skipped."

type checkFlags struct {
	project, database, migrationsDir, migrateCommand, prFiles, baseRef, repoRoot string
	verdictTimeout                                                               time.Duration
	failOnUnreachable                                                            bool
	pr                                                                           pullRequest
}

// runCheck runs the pull request check and returns the exit code: 1 only
// when the migrations have errors, or when Stratum is unreachable and the
// workflow asked for -fail-on-unreachable.
func runCheck(args []string) int {
	var f checkFlags
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.StringVar(&f.project, "project", os.Getenv("STRATUM_PROJECT"), "the Stratum project, org/slug")
	fs.StringVar(&f.database, "database", os.Getenv("STRATUM_CHECK_DSN"), "postgres:// URL of an empty throwaway database, as a superuser")
	fs.StringVar(&f.migrationsDir, "migrations-dir", "", "the migrations folder")
	fs.StringVar(&f.migrateCommand, "migrate-command", "", "your migrate command, e.g. flyway -url=$STRATUM_CHECK_JDBC_URL -user=$STRATUM_CHECK_USER -password=$STRATUM_CHECK_PASSWORD migrate")
	fs.StringVar(&f.prFiles, "pull-request-files", "", "comma-separated migration files the pull request adds or changes; default: git diff against -base-ref")
	fs.StringVar(&f.baseRef, "base-ref", os.Getenv("GITHUB_BASE_REF"), "the pull request's base branch")
	fs.StringVar(&f.repoRoot, "repo-root", envOr("GITHUB_WORKSPACE", "."), "the repository root, for file paths in annotations")
	fs.DurationVar(&f.verdictTimeout, "verdict-timeout", 3*time.Minute, "how long to wait for Stratum's verdict")
	fs.BoolVar(&f.failOnUnreachable, "fail-on-unreachable", false, "fail instead of passing with a warning when Stratum is unreachable")
	f.pr.register(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if f.project == "" || f.database == "" || f.migrationsDir == "" || f.migrateCommand == "" {
		fmt.Fprintln(os.Stderr, "stratum check: -project, -database, -migrations-dir and -migrate-command are required")
		return 2
	}
	stratumURL := os.Getenv("STRATUM_URL")
	if stratumURL == "" {
		fmt.Fprintln(os.Stderr, "stratum check: STRATUM_URL is not set")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	verdict, err := checkPullRequest(ctx, stratumURL, f)
	if errors.Is(err, errUnavailable) {
		warn("%s", unavailableMessage)
		if f.failOnUnreachable {
			return 1
		}
		return 0
	}
	if err != nil {
		// A broken check is a setup problem, not the migrations' fault.
		warn("Stratum check couldn't run: %v", err)
		return 0
	}
	return report(os.Stdout, verdict)
}

func checkPullRequest(ctx context.Context, stratumURL string, f checkFlags) (*agentv1.GetCheckVerdictResponse, error) {
	token := os.Getenv("STRATUM_CHECK_TOKEN")
	if token == "" {
		var err error
		if token, err = client.ActionsOIDCToken(ctx); err != nil {
			return nil, err
		}
	}
	c := client.New(client.Options{BaseURL: stratumURL, Token: token, Project: f.project})

	pr, err := f.pr.resolve()
	if err != nil {
		return nil, err
	}
	begin, err := c.Check.BeginCheck(ctx, connect.NewRequest(&agentv1.BeginCheckRequest{
		Project:           f.project,
		PullRequest:       pr,
		WorkflowRunId:     envInt64("GITHUB_RUN_ID"),
		RunAttempt:        int32(envInt64("GITHUB_RUN_ATTEMPT")),
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
	prFiles, err := pullRequestFiles(f)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.Connect(ctx, f.database)
	if err != nil {
		return nil, fmt.Errorf("connect to the check database: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	r, err := check.Run(ctx, check.Config{
		Conn: conn, Snapshot: snap, SnapshotLabel: begin.Msg.GetSnapshotVersion(), Tool: tool,
		Files: files, PullRequestFiles: prFiles, MigratorName: migratorName(f.migrateCommand),
		Migrator: check.CommandMigrator{Command: f.migrateCommand, Tool: tool, DSN: f.database},
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

// waitForVerdict polls until Stratum has evaluated the check, or the wait
// runs out, which counts as Stratum being unavailable.
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

// isUnreachable reports whether err means Stratum is down, failing or slow,
// rather than refusing the request.
func isUnreachable(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeUnknown, connect.CodeInternal:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// pullRequestFiles returns the migration files the pull request adds or
// changes, from the flag or from git.
func pullRequestFiles(f checkFlags) ([]string, error) {
	var names []string
	if f.prFiles != "" {
		names = strings.Split(f.prFiles, ",")
	} else {
		if f.baseRef == "" {
			return nil, errors.New("set -pull-request-files or -base-ref")
		}
		// Renamed files count: renumbering a migration is a rename.
		out, err := exec.Command("git", "diff", "--name-only", "--diff-filter=AMR", "origin/"+f.baseRef+"...HEAD", "--", f.migrationsDir).Output()
		if err != nil {
			return nil, fmt.Errorf("git diff against %s (check out with fetch-depth: 0): %w", f.baseRef, err)
		}
		names = strings.Fields(string(out))
		// git names files from the repository's top level, which isn't the
		// workspace when the workflow checks out into a subfolder.
		top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
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
		if !filepath.IsAbs(p) {
			p = filepath.Join(f.repoRoot, p)
		}
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
	for _, tool := range []string{"flyway", "atlas"} {
		for _, f := range fields {
			if strings.HasSuffix(f, tool) || strings.Contains(f, "/"+tool) {
				if tool == "atlas" {
					return "atlas migrate apply"
				}
				return "flyway migrate"
			}
		}
	}
	return "your migrate command"
}
