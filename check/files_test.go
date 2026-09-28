package check_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stratum-dev/agent/check"
	"github.com/stratum-dev/agent/snapshot"
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
