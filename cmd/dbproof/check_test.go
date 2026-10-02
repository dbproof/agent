package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Relative -pull-request-files resolve from the working directory, like
// -migrations-dir, so they match the migrations the agent lists even when
// the workflow checks the repository out into a subfolder of the workspace.
func TestPullRequestFilesResolveFromTheWorkingDirectory(t *testing.T) {
	// Resolve symlinks so paths compare: macOS's temp dir is under /var,
	// which is /private/var.
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(workspace, "dbproof")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	got, err := pullRequestFiles(t.Context(), checkFlags{prFiles: "db/V4__due_date.sql, db/V5__index.sql", repoRoot: workspace})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(repo, "db", "V4__due_date.sql"), filepath.Join(repo, "db", "V5__index.sql")}
	if !slices.Equal(got, want) {
		t.Fatalf("pull request files = %v, want %v", got, want)
	}
}
