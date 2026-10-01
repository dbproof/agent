package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/dbproof/agent/capture"
	"github.com/dbproof/agent/client"
	agentv1 "github.com/dbproof/agent/gen/dbproof/agent/v1"
	"github.com/dbproof/agent/snapshot"
)

type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// runCapture captures and uploads. It always exits 0 unless the flags are
// wrong: a capture that can't run or upload must not stop a deploy.
func runCapture(args []string) int {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	kind := fs.String("kind", "", "when this capture runs: pre (before migrations), post (after) or scheduled")
	expect := fs.String("expect-version", "", "post-deploy: wait until the history shows this migration version")
	wait := fs.Duration("wait-timeout", 10*time.Minute, "post-deploy: how long to wait for -expect-version")
	tool := fs.String("tool", "", "flyway or atlas; overrides DbProof's project setting")
	history := fs.String("history-table", "", "schema.table of the migration history; overrides DbProof's setting")
	var exclude stringList
	fs.Var(&exclude, "exclude", `"schema.*" or "schema.table" to leave out; repeatable; overrides DbProof's setting`)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: dbproof capture -kind=pre|post|scheduled [flags]

Environment:
  DBPROOF_URL            DbProof's address
  DBPROOF_CAPTURE_TOKEN  the project's upload-only capture token
  DBPROOF_CAPTURE_DSN    connection string for a role that can read the migration
                         history, such as the one your migrations run as

Flags:`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	kinds := map[string]snapshot.Kind{"pre": snapshot.KindPreDeploy, "post": snapshot.KindPostDeploy, "scheduled": snapshot.KindScheduled}
	k, ok := kinds[*kind]
	if !ok {
		fmt.Fprintln(os.Stderr, "dbproof capture: -kind must be pre, post or scheduled")
		return 2
	}
	env := map[string]string{}
	for _, name := range []string{"DBPROOF_URL", "DBPROOF_CAPTURE_TOKEN", "DBPROOF_CAPTURE_DSN"} {
		if env[name] = os.Getenv(name); env[name] == "" {
			warn("DbProof capture skipped: %s is not set", name)
			return 0
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := client.New(client.Options{BaseURL: env["DBPROOF_URL"], Token: env["DBPROOF_CAPTURE_TOKEN"], Project: os.Getenv("DBPROOF_PROJECT")})

	cfg := capture.Config{Kind: k, ExpectVersion: *expect, WaitTimeout: *wait, AgentVersion: version}
	remote, err := c.Capture.GetCaptureConfig(ctx, connect.NewRequest(&agentv1.GetCaptureConfigRequest{AgentVersion: version}))
	if err != nil {
		if !isUnreachable(err) {
			warn("DbProof capture skipped: %v", err)
			return 0
		}
		warn("DbProof is unreachable (%v); capturing with local settings, then retrying the upload", err)
	} else {
		cfg.Tool = client.Tool(remote.Msg.GetTool())
		cfg.HistoryTable = remote.Msg.GetHistoryTable()
		cfg.Exclude = remote.Msg.GetExclusions()
		fmt.Fprintf(os.Stderr, "Capturing %s for %s\n", k, remote.Msg.GetProject())
	}
	if *tool != "" {
		cfg.Tool = snapshot.Tool(*tool)
	}
	if *history != "" {
		cfg.HistoryTable = *history
	}
	if len(exclude) > 0 {
		cfg.Exclude = exclude
	}
	if cfg.Tool != snapshot.ToolFlyway && cfg.Tool != snapshot.ToolAtlas {
		warn("DbProof capture skipped: set -tool to flyway or atlas")
		return 0
	}

	snap, err := captureDatabase(ctx, env["DBPROOF_CAPTURE_DSN"], cfg)
	if err != nil {
		warn("DbProof capture skipped: %v", err)
		return 0
	}
	body, err := snapshot.Encode(snap)
	if err != nil {
		warn("DbProof capture skipped: %v", err)
		return 0
	}
	resp, err := c.Capture.UploadSnapshot(ctx, connect.NewRequest(&agentv1.UploadSnapshotRequest{
		UploadId:     uuid.NewString(),
		Snapshot:     body,
		AgentVersion: version,
		CommitSha:    os.Getenv("GITHUB_SHA"),
	}))
	if err != nil {
		warn("DbProof capture wasn't uploaded: %v", err)
		return 0
	}
	fmt.Fprintf(os.Stderr, "Uploaded capture %s (%d tables, %d KB)\n", resp.Msg.GetCaptureId(), len(snap.Schema.Tables), len(body)/1024)
	if minVersion := resp.Msg.GetMinAgentVersion(); client.OlderThan(version, minVersion) {
		warn("This agent (%s) is older than DbProof supports (%s); upgrade it", version, minVersion)
	}
	return 0
}

func captureDatabase(ctx context.Context, dsn string, cfg capture.Config) (*snapshot.Snapshot, error) {
	connCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(connCtx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	runCtx, cancelRun := context.WithTimeout(ctx, cfg.WaitTimeout+5*time.Minute)
	defer cancelRun()
	return capture.Run(runCtx, conn, cfg)
}
