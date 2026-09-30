package check

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/borovikovd/dbproof-agent/snapshot"
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
// out.
func ListFiles(tool snapshot.Tool, dir string) ([]File, error) {
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
	for i := 0; i < max(len(pa), len(pb)); i++ {
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

// targetArgs tells the tool to stop after version: Flyway's -target and
// Atlas's --to-version.
func targetArgs(tool snapshot.Tool, version string) string {
	if tool == snapshot.ToolAtlas {
		return " --to-version " + version
	}
	return " -target=" + version
}
