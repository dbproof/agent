package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/dbproof/agent/capture"
	"github.com/dbproof/agent/snapshot"
)

// File is one versioned migration in the repository.
type File struct {
	Version string
	// Path is where the file is on disk.
	Path string
}

var (
	// Flyway's versioned migrations: V1__init.sql, V2_1__more.sql.
	flywayFile = regexp.MustCompile(`^V([0-9][0-9._]*)__.*\.sql$`)
	// Atlas's: 20260927120000_add.sql or 20260927120000.sql.
	atlasFile = regexp.MustCompile(`^([0-9]+)(_.*)?\.sql$`)
)

// ListFiles returns the tool's versioned migrations in dir, in the order the
// tool applies them. Flyway scans subfolders too; Atlas reads one folder.
// Flyway's repeatable (R__) migrations and anything that isn't SQL are left
// out. Prisma keeps each migration in a folder of its own, and Drizzle lists
// its migrations in a journal.
func ListFiles(tool snapshot.Tool, dir string) ([]File, error) {
	switch tool {
	case snapshot.ToolPrisma:
		return prismaFiles(dir)
	case snapshot.ToolDrizzle:
		return drizzleFiles(dir)
	case snapshot.ToolFlyway, snapshot.ToolAtlas:
	}
	pattern := flywayFile
	if tool == snapshot.ToolAtlas {
		pattern = atlasFile
	}
	var files []File
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && tool == snapshot.ToolAtlas {
				return filepath.SkipDir
			}
			return nil
		}
		m := pattern.FindStringSubmatch(d.Name())
		if m == nil {
			return nil
		}
		version := m[1]
		if tool == snapshot.ToolFlyway {
			version = strings.ReplaceAll(version, "_", ".")
		}
		files = append(files, File{Version: version, Path: path})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(files, func(a, b File) int { return compareVersions(a.Version, b.Version) })
	return files, nil
}

// compareVersions orders dotted versions numerically: 1.10 comes after 1.9.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// prismaFiles returns Prisma's migrations: each folder in dir that holds a
// migration.sql, by folder name, which is the order Prisma applies them in.
// The folder's name is the migration's version, as Prisma records it.
func prismaFiles(dir string) ([]File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for _, e := range entries {
		path := filepath.Join(dir, e.Name(), "migration.sql")
		if _, err := os.Stat(path); e.IsDir() && err == nil {
			files = append(files, File{Version: e.Name(), Path: path})
		}
	}
	return files, nil
}

// drizzleJournal is the meta/_journal.json Drizzle Kit keeps next to its
// migrations, listing them in the order they apply.
const drizzleJournal = "meta/_journal.json"

// drizzleFiles returns Drizzle's migrations in its journal's order. A
// migration's version is its place in the journal, as capture counts the
// history's rows.
func drizzleFiles(dir string) ([]File, error) {
	raw, err := os.ReadFile(filepath.Join(dir, drizzleJournal)) //nolint:gosec // the workflow's migrations folder
	if err != nil {
		return nil, fmt.Errorf("read Drizzle's journal: %w", err)
	}
	var journal struct {
		Entries []struct {
			Idx int    `json:"idx"`
			Tag string `json:"tag"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &journal); err != nil {
		return nil, fmt.Errorf("read Drizzle's journal: %w", err)
	}
	files := make([]File, len(journal.Entries))
	for i, e := range journal.Entries {
		files[i] = File{Version: capture.DrizzleVersion(e.Idx), Path: filepath.Join(dir, e.Tag+".sql")}
	}
	slices.SortFunc(files, func(a, b File) int { return strings.Compare(a.Version, b.Version) })
	return files, nil
}

// targetArgs tells the tool to stop after version: Flyway's -target and
// Atlas's --to-version. Prisma and Drizzle have no such option; Staged stops
// them instead.
func targetArgs(tool snapshot.Tool, version string) string {
	switch tool {
	case snapshot.ToolAtlas:
		return " --to-version " + version
	case snapshot.ToolFlyway:
		return " -target=" + version
	case snapshot.ToolPrisma, snapshot.ToolDrizzle:
	}
	return ""
}

// Staged runs a migrator for a tool that applies every pending migration,
// with no option to stop at one: Prisma and Drizzle. For each run it leaves
// only the migrations up to the target where the tool looks, and puts the
// rest back after, whatever happened. Files are the migrations in Dir.
type Staged struct {
	Migrator
	Tool  snapshot.Tool
	Dir   string
	Files []File
}

func (s Staged) Migrate(ctx context.Context, target string) (string, error) {
	i := slices.IndexFunc(s.Files, func(f File) bool { return f.Version == target })
	if i < 0 {
		return "", fmt.Errorf("no migration %s in %s", target, s.Dir)
	}
	restore, err := hideAfter(s.Tool, s.Dir, s.Files, i)
	if err != nil {
		return "", fmt.Errorf("set aside the migrations after %s: %w", target, err)
	}
	out, err := s.Migrator.Migrate(ctx, target)
	if rerr := restore(); rerr != nil {
		return out, fmt.Errorf("put back the migrations after %s: %w", target, rerr)
	}
	return out, err
}

// hideAfter hides the migrations after files[i] from the tool, and returns
// how to bring them back: Prisma's folders move out of dir, and Drizzle's
// journal loses their entries.
func hideAfter(tool snapshot.Tool, dir string, files []File, i int) (func() error, error) {
	later := files[i+1:]
	switch tool {
	case snapshot.ToolPrisma:
		aside, err := os.MkdirTemp(filepath.Dir(dir), ".dbproof-later-")
		if err != nil {
			return nil, err
		}
		var moved []string
		restore := func() error {
			var errs []error
			for _, folder := range moved {
				errs = append(errs, os.Rename(filepath.Join(aside, filepath.Base(folder)), folder))
			}
			return errors.Join(append(errs, os.Remove(aside))...)
		}
		for _, f := range later {
			folder := filepath.Dir(f.Path)
			if err := os.Rename(folder, filepath.Join(aside, filepath.Base(folder))); err != nil {
				return nil, errors.Join(err, restore())
			}
			moved = append(moved, folder)
		}
		return restore, nil
	case snapshot.ToolDrizzle:
		path := filepath.Join(dir, drizzleJournal)
		original, err := os.ReadFile(path) //nolint:gosec // the workflow's migrations folder
		if err != nil {
			return nil, err
		}
		// Everything else in the journal stays as it is.
		var journal map[string]any
		if err := json.Unmarshal(original, &journal); err != nil {
			return nil, fmt.Errorf("read Drizzle's journal: %w", err)
		}
		entries, _ := journal["entries"].([]any)
		journal["entries"] = entries[:min(len(entries), len(files)-len(later))]
		staged, err := json.MarshalIndent(journal, "", "  ")
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, staged, 0o600); err != nil {
			return nil, err
		}
		return func() error { return os.WriteFile(path, original, 0o600) }, nil //nolint:gosec // the journal read above, put back as it was
	case snapshot.ToolFlyway, snapshot.ToolAtlas:
	}
	return func() error { return nil }, nil
}
