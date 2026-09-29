// Package snapshot defines the versioned format the agent uploads to Stratum:
// a schema, planner statistics and the migration history table's rows. It
// never contains values from application tables.
package snapshot

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/borovikovd/stratum-agent/schema"
)

// FormatVersion is the snapshot format this agent writes. Readers accept any
// version up to their own; fields are only ever added within a version.
const FormatVersion = 1

// Kind says when a capture ran relative to deploys.
type Kind string

const (
	KindPreDeploy  Kind = "pre"
	KindPostDeploy Kind = "post"
	KindScheduled  Kind = "scheduled"
)

// Snapshot is one capture of a production database.
type Snapshot struct {
	FormatVersion int32          `json:"format_version"`
	CapturedAt    time.Time      `json:"captured_at"`
	Kind          Kind           `json:"kind"`
	AgentVersion  string         `json:"agent_version"`
	Schema        *schema.Schema `json:"schema"`
	Stats         []TableStats   `json:"stats,omitempty"`
	History       *History       `json:"history,omitempty"`
	// Exclusions are the "schema.*" and "schema.table" patterns left out of
	// the schema and statistics.
	Exclusions []string `json:"exclusions,omitempty"`
}

// TableStats holds planner estimates for one table: no values, only counts
// and fractions.
type TableStats struct {
	Table string `json:"table"`
	// Rows is pg_class.reltuples: an estimate, or -1 if never analyzed.
	Rows    float64       `json:"rows"`
	Columns []ColumnStats `json:"columns,omitempty"`
}

// ColumnStats holds pg_stats' null_frac, n_distinct and avg_width.
type ColumnStats struct {
	Column    string  `json:"column"`
	NullFrac  float64 `json:"null_frac"`
	NDistinct float64 `json:"n_distinct"`
	AvgWidth  int     `json:"avg_width"`
}

// Tool is a migration tool whose history table the agent reads.
type Tool string

const (
	ToolFlyway Tool = "flyway"
	ToolAtlas  Tool = "atlas"
)

// History is a migration tool's history table, row by row, as text, so a
// check can insert it back exactly.
type History struct {
	Tool Tool `json:"tool"`
	// Table is the history table's identity, e.g. public.flyway_schema_history.
	Table   string      `json:"table"`
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

// Value returns a row's value for the named column, or nil.
func (h *History) Value(row []*string, column string) *string {
	for i, c := range h.Columns {
		if c == column && i < len(row) {
			return row[i]
		}
	}
	return nil
}

// Encode writes the snapshot as gzipped JSON.
func Encode(s *Snapshot) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(s); err != nil {
		return nil, fmt.Errorf("encode snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("compress snapshot: %w", err)
	}
	return buf.Bytes(), nil
}

// MaxDecodedSize bounds a snapshot's size once decompressed. Uploads are
// capped compressed, and gzip inflates zeros a thousandfold, so without it a
// small upload could fill the server's memory. A schema with tens of
// thousands of objects stays well under it.
const MaxDecodedSize = 256 << 20

// Decode reads a snapshot written by Encode, rejecting formats newer than
// this code understands and snapshots over MaxDecodedSize.
func Decode(b []byte) (*Snapshot, error) {
	return decode(b, MaxDecodedSize)
}

func decode(b []byte, maxSize int64) (*Snapshot, error) {
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("decompress snapshot: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("decompress snapshot: %w", err)
	}
	if int64(len(raw)) > maxSize {
		return nil, fmt.Errorf("snapshot is over %d MiB decompressed", maxSize>>20)
	}
	var s Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	if s.FormatVersion < 1 || s.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("snapshot format version %d is not supported (this build reads up to %d)", s.FormatVersion, FormatVersion)
	}
	if s.Schema == nil {
		return nil, fmt.Errorf("snapshot has no schema")
	}
	return &s, nil
}
