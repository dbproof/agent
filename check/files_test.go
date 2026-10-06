package check_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbproof/agent/check"
	"github.com/dbproof/agent/snapshot"
)

// Flyway finds migrations in subfolders of its location; Atlas reads one
// folder.
func TestListFilesFindsFlywaySubfolders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"V1__init.sql", "2026/V2__later.sql", "R__views.sql", "notes.txt"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("SELECT 1;"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := check.ListFiles(snapshot.ToolFlyway, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[1].Version != "2" || files[1].Path != filepath.Join(dir, "2026", "V2__later.sql") {
		t.Fatalf("files = %+v, want V1 and 2026/V2", files)
	}
}

// Prisma keeps each migration in a folder of its own, applied by name.
func TestListFilesFindsPrismaFolders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"20260102000000_email/migration.sql", "20260101000000_init/migration.sql", "migration_lock.toml", "drafts/notes.md"} {
		write(t, filepath.Join(dir, name), "SELECT 1;")
	}
	files, err := check.ListFiles(snapshot.ToolPrisma, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Version != "20260101000000_init" || files[1].Path != filepath.Join(dir, "20260102000000_email", "migration.sql") {
		t.Fatalf("files = %+v, want init then email", files)
	}
}

// Drizzle's journal names its migrations and their order.
func TestListFilesFollowsDrizzlesJournal(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "meta", "_journal.json"), `{"version":"7","dialect":"postgresql","entries":[
		{"idx":1,"tag":"0001_email","when":2},{"idx":0,"tag":"0000_init","when":1}]}`)
	files, err := check.ListFiles(snapshot.ToolDrizzle, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Version != "0000" || files[1].Path != filepath.Join(dir, "0001_email.sql") {
		t.Fatalf("files = %+v, want 0000 then 0001", files)
	}
}

// sees records what a tool would find when it runs.
type sees struct {
	dir  string
	seen *[]string
	err  error
}

func (s sees) Migrate(context.Context, string) (string, error) {
	var names []string
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if journal, err := os.ReadFile(filepath.Join(s.dir, "meta", "_journal.json")); err == nil {
		names = append(names, string(journal))
	}
	*s.seen = append(*s.seen, strings.Join(names, " "))
	return "", s.err
}

// For a tool with no way to stop at a migration, Staged shows it only the
// migrations up to the target, and puts the rest back even when it fails.
func TestStagedHidesLaterMigrations(t *testing.T) {
	t.Run("prisma", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "migrations")
		for _, name := range []string{"1_init", "2_email", "3_tax_id"} {
			write(t, filepath.Join(dir, name, "migration.sql"), "SELECT 1;")
		}
		files, _ := check.ListFiles(snapshot.ToolPrisma, dir)
		var seen []string
		staged := check.Staged{Migrator: sees{dir: dir, seen: &seen, err: errors.New("failed")}, Tool: snapshot.ToolPrisma, Dir: dir, Files: files}
		if _, err := staged.Migrate(context.Background(), "2_email"); err == nil {
			t.Fatal("the migrator's error was lost")
		}
		if len(seen) != 1 || seen[0] != "1_init 2_email" {
			t.Errorf("Prisma saw %q, want 1_init and 2_email only", seen)
		}
		if after, _ := check.ListFiles(snapshot.ToolPrisma, dir); len(after) != 3 {
			t.Errorf("after: %d migrations, want all 3 back", len(after))
		}
		if siblings, _ := os.ReadDir(filepath.Dir(dir)); len(siblings) != 1 {
			t.Errorf("left %d entries beside the migrations folder, want none", len(siblings)-1)
		}
	})
	t.Run("drizzle", func(t *testing.T) {
		dir := t.TempDir()
		journal := `{"version":"7","dialect":"postgresql","entries":[{"idx":0,"tag":"0000_init"},{"idx":1,"tag":"0001_email"},{"idx":2,"tag":"0002_tax_id"}]}`
		write(t, filepath.Join(dir, "meta", "_journal.json"), journal)
		files, _ := check.ListFiles(snapshot.ToolDrizzle, dir)
		var seen []string
		staged := check.Staged{Migrator: sees{dir: dir, seen: &seen}, Tool: snapshot.ToolDrizzle, Dir: dir, Files: files}
		if _, err := staged.Migrate(context.Background(), "0001"); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || !strings.Contains(seen[0], "0001_email") || strings.Contains(seen[0], "0002_tax_id") {
			t.Errorf("Drizzle saw %q, want the journal up to 0001", seen)
		}
		if after, _ := os.ReadFile(filepath.Join(dir, "meta", "_journal.json")); string(after) != journal {
			t.Errorf("journal after = %s, want it as it was", after)
		}
	})
}
