package check_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/borovikovd/dbproof-agent/capture"
	"github.com/borovikovd/dbproof-agent/check"
	"github.com/borovikovd/dbproof-agent/internal/pgtest"
	"github.com/borovikovd/dbproof-agent/snapshot"
)

// atlas runs the real Atlas CLI, which `just tools` installs into bin.
func atlas(t *testing.T, args ...string) {
	t.Helper()
	path, err := exec.LookPath("atlas")
	if err != nil {
		t.Fatal("atlas isn't on PATH; run `just tools`")
	}
	if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
		t.Fatalf("atlas %v: %v\n%s", args, err, out)
	}
}

// noSSL is how the check action hands Atlas the throwaway database.
func noSSL(dbURL string) string { return dbURL + "?sslmode=disable" }

// writeAtlasDir writes migrations and the atlas.sum Atlas requires.
func writeAtlasDir(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, sql := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	atlas(t, "migrate", "hash", "--dir", "file://"+dir)
}

// On Neon, Atlas keeps its history in public (--revisions-schema public,
// since Neon ignores search_path). The check restores that history, then
// runs the workflow's plain `atlas migrate apply` against an ordinary
// Postgres, where Atlas looks for it in atlas_schema_revisions.
func TestCheckWithAtlasHistoryInPublic(t *testing.T) {
	server := pgtest.Servers(t)[0]
	dir := t.TempDir()
	writeAtlasDir(t, dir, map[string]string{
		"20260901120000_init.sql": "CREATE TABLE orgs (id bigint PRIMARY KEY, login text NOT NULL, avatar_url text NOT NULL DEFAULT '');",
	})
	prod := pgtest.NewDB(t, server)
	atlas(t, "migrate", "apply", "--dir", "file://"+dir, "--revisions-schema", "public", "--url", noSSL(prod))
	snap, err := capture.Run(context.Background(), pgtest.Connect(t, prod), capture.Config{Kind: snapshot.KindScheduled, Tool: snapshot.ToolAtlas})
	if err != nil {
		t.Fatal(err)
	}

	writeAtlasDir(t, dir, map[string]string{"20261001060000_drop_org_avatar.sql": "ALTER TABLE orgs DROP COLUMN avatar_url;"})
	files, err := check.ListFiles(snapshot.ToolAtlas, dir)
	if err != nil {
		t.Fatal(err)
	}
	checkDB := pgtest.NewDB(t, server)
	r, err := check.Run(context.Background(), check.Config{
		Conn: pgtest.Connect(t, checkDB), Snapshot: snap, SnapshotLabel: "S-1", Tool: snapshot.ToolAtlas,
		Files: files, PullRequestFiles: []string{filepath.Join(dir, "20261001060000_drop_org_avatar.sql")},
		Migrator: check.CommandMigrator{
			Command: `atlas migrate apply --dir "file://` + dir + `" --url "$DBPROOF_CHECK_DSN"`,
			Tool:    snapshot.ToolAtlas, DSN: noSSL(checkDB),
		},
		MigratorName: "atlas migrate apply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.SetupProblem != "" {
		t.Fatalf("setup problem: %s", r.SetupProblem)
	}
	if len(r.Migrations) != 1 || !r.Migrations[0].Applied {
		t.Fatalf("migrations = %+v, want the pull request's migration applied", r.Migrations)
	}
}
