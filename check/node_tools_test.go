package check_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbproof/agent/capture"
	"github.com/dbproof/agent/check"
	"github.com/dbproof/agent/internal/pgtest"
	"github.com/dbproof/agent/snapshot"
)

// nodeTool is a Node.js migration tool's project: how to install and
// configure it, run it, and lay out its migrations.
type nodeTool struct {
	tool     snapshot.Tool
	packages []string
	config   map[string]string
	// dir is the migrations folder, relative to the project.
	dir     string
	command string
	// write lays out the first n migrations as the tool expects them.
	write func(t *testing.T, dir string, n int)
}

// migrations are the four migrations each tool's project has: production
// runs the first two, main has merged the third, and the pull request adds
// the fourth.
var migrations = []struct{ name, sql string }{
	{"init", "CREATE TABLE customers (id bigint PRIMARY KEY, name text NOT NULL);"},
	{"invoices", "CREATE TABLE invoices (id bigint PRIMARY KEY, customer_id bigint REFERENCES customers);"},
	{"tax_id", "ALTER TABLE customers ADD COLUMN tax_id text;"},
	{"invoice_customer", "CREATE INDEX invoices_customer_idx ON invoices (customer_id);"},
}

var prisma = nodeTool{
	tool:     snapshot.ToolPrisma,
	packages: []string{"prisma@7.10.0"},
	config: map[string]string{
		"prisma.config.ts": `import { defineConfig, env } from "prisma/config";
export default defineConfig({ schema: "prisma/schema.prisma", datasource: { url: env("DATABASE_URL") } });
`,
		"prisma/schema.prisma":                  "datasource db {\n  provider = \"postgresql\"\n}\n",
		"prisma/migrations/migration_lock.toml": "provider = \"postgresql\"\n",
	},
	dir:     "prisma/migrations",
	command: "npx prisma migrate deploy",
	write: func(t *testing.T, dir string, n int) {
		t.Helper()
		for i, m := range migrations[:n] {
			write(t, filepath.Join(dir, "2026010"+string(rune('1'+i))+"000000_"+m.name, "migration.sql"), m.sql)
		}
	},
}

var drizzle = nodeTool{
	tool:     snapshot.ToolDrizzle,
	packages: []string{"drizzle-kit@0.31.11", "drizzle-orm@0.45.3", "pg@8.23.1"},
	config: map[string]string{
		"drizzle.config.ts": `import { defineConfig } from "drizzle-kit";
export default defineConfig({ dialect: "postgresql", out: "./drizzle", dbCredentials: { url: process.env.DATABASE_URL! } });
`,
	},
	dir:     "drizzle",
	command: "npx drizzle-kit migrate",
	write: func(t *testing.T, dir string, n int) {
		t.Helper()
		var entries []string
		for i, m := range migrations[:n] {
			tag := capture.DrizzleVersion(i) + "_" + m.name
			write(t, filepath.Join(dir, tag+".sql"), m.sql)
			entries = append(entries, `{"idx":`+string(rune('0'+i))+`,"version":"7","when":170000000000`+string(rune('0'+i))+`,"tag":"`+tag+`","breakpoints":true}`)
		}
		write(t, filepath.Join(dir, "meta", "_journal.json"), `{"version":"7","dialect":"postgresql","entries":[`+strings.Join(entries, ",")+`]}`)
	},
}

// The real Prisma and Drizzle Kit, against real Postgres: production
// applies two migrations and is captured, then the check applies main's
// third and the pull request's fourth one at a time, though neither tool can
// stop at a migration on its own. It needs Node.js and the network, so it
// runs only with DBPROOF_TEST_NODE_TOOLS=1.
func TestCheckRunsPrismaAndDrizzle(t *testing.T) {
	if os.Getenv("DBPROOF_TEST_NODE_TOOLS") == "" {
		t.Skip("set DBPROOF_TEST_NODE_TOOLS=1 to run the real Prisma and Drizzle Kit")
	}
	for _, nt := range []nodeTool{prisma, drizzle} {
		t.Run(string(nt.tool), func(t *testing.T) {
			project := t.TempDir()
			for name, text := range nt.config {
				write(t, filepath.Join(project, name), text)
			}
			install := exec.CommandContext(t.Context(), "npm", append([]string{"install", "--no-audit", "--no-fund", "--silent"}, nt.packages...)...)
			install.Dir = project
			if out, err := install.CombinedOutput(); err != nil {
				t.Fatalf("npm install: %v\n%s", err, out)
			}
			dir := filepath.Join(project, nt.dir)
			for _, server := range pgtest.Servers(t) {
				t.Run(server.Name, func(t *testing.T) {
					checkNodeTool(t, nt, server, project, dir)
				})
			}
		})
	}
}

func checkNodeTool(t *testing.T, nt nodeTool, server pgtest.Server, project, dir string) {
	t.Helper()
	ctx := context.Background()
	command := "cd " + project + " && " + nt.command
	_ = os.RemoveAll(dir)
	if nt.tool == snapshot.ToolPrisma {
		write(t, filepath.Join(dir, "migration_lock.toml"), nt.config["prisma/migrations/migration_lock.toml"])
	}

	// Production has run the first two.
	nt.write(t, dir, 2)
	prod := pgtest.NewDB(t, server)
	if out, err := (check.CommandMigrator{Command: command, Tool: nt.tool, DSN: prod}).Migrate(ctx, ""); err != nil {
		t.Fatalf("migrate production: %v\n%s", err, out)
	}
	pgtest.Exec(t, prod, `CREATE SCHEMA dbproof;
		CREATE FUNCTION dbproof.table_stats() RETURNS TABLE (schema_name text, table_name text, n_rows real, column_name text, null_frac real, n_distinct real, avg_width integer)
		  LANGUAGE sql AS $$ SELECT NULL::text, NULL::text, NULL::real, NULL::text, NULL::real, NULL::real, NULL::integer WHERE false $$;`)
	snap, err := capture.Run(ctx, pgtest.Connect(t, prod), capture.Config{Kind: snapshot.KindScheduled, Tool: nt.tool})
	if err != nil {
		t.Fatal(err)
	}
	if got := capture.Applied(snap.History); len(got) != 2 {
		t.Fatalf("production applied = %v, want two", got)
	}

	// Main merged the third; the pull request adds the fourth.
	nt.write(t, dir, 4)
	files, err := check.ListFiles(nt.tool, dir)
	if err != nil || len(files) != 4 {
		t.Fatalf("files = %+v, %v", files, err)
	}
	before := tree(t, dir)
	conn := pgtest.Connect(t, pgtest.NewDB(t, server))
	migrator := check.Staged{Migrator: check.CommandMigrator{Command: command, Tool: nt.tool, DSN: conn.Config().ConnString()}, Tool: nt.tool, Dir: dir, Files: files}
	r, err := check.Run(ctx, check.Config{
		Conn: conn, Snapshot: snap, SnapshotLabel: "S-1", Tool: nt.tool,
		Files: files, PullRequestFiles: []string{files[3].Path}, Migrator: migrator, MigratorName: nt.command,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.SetupProblem != "" {
		t.Fatalf("setup problem: %s", r.SetupProblem)
	}
	if len(r.Migrations) != 2 || !r.Migrations[0].Applied || !r.Migrations[1].Applied || r.Migrations[0].FromPullRequest || !r.Migrations[1].FromPullRequest {
		t.Fatalf("migrations = %+v", r.Migrations)
	}
	// Each migration's changes are its own, so the tool stopped after the
	// third before the fourth ran.
	if c := r.Migrations[0].Changes; len(c) != 1 || c[0].ID != "public.customers.tax_id" {
		t.Errorf("main's migration changed %+v, want only customers.tax_id", c)
	}
	if c := r.Migrations[1].Changes; len(c) != 1 || c[0].ID != "public.invoices.invoices_customer_idx" {
		t.Errorf("the pull request's migration changed %+v, want only the index", c)
	}
	if after := tree(t, dir); after != before {
		t.Errorf("the migrations folder wasn't put back:\nbefore %s\nafter  %s", before, after)
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// tree lists the files under dir with their contents, to compare.
func tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		text, err := os.ReadFile(path)
		b.WriteString(path + "=" + string(text) + ";")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
