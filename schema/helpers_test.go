package schema_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dbproof/agent/internal/pgtest"
	"github.com/dbproof/agent/schema"
)

func hasView(s *schema.Schema, id string) bool {
	for _, v := range s.Views {
		if v.ID() == id {
			return true
		}
	}
	return false
}

func inspect(t *testing.T, dbURL string) *schema.Schema {
	t.Helper()
	conn := pgtest.Connect(t, dbURL)
	s, err := schema.Inspect(context.Background(), conn, schema.Options{IgnoreGrantees: []string{pgtest.CaptureRole}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func restore(t *testing.T, dbURL string, stmts []string) {
	t.Helper()
	conn := pgtest.Connect(t, dbURL)
	for _, stmt := range stmts {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("restore statement failed: %v\n%s", err, stmt)
		}
	}
}

func describe(changes []schema.Change) string {
	var b strings.Builder
	for _, c := range changes {
		b.WriteString(string(c.Op) + " " + string(c.Kind) + " " + c.ID)
		if len(c.Fields) > 0 {
			b.WriteString(" (" + strings.Join(c.Fields, ", ") + ")")
			before, err := json.Marshal(c.Before)
			if err != nil {
				panic(err)
			}
			after, err := json.Marshal(c.After)
			if err != nil {
				panic(err)
			}
			b.WriteString("\n    before: " + string(before) + "\n    after:  " + string(after))
		}
		b.WriteString("\n")
	}
	return b.String()
}
