package main

import (
	"strings"
	"testing"

	agentv1 "github.com/dbproof/agent/gen/dbproof/agent/v1"
)

// GitHub decodes %2C and %3A only in a workflow command's properties, such
// as the title, so a message escapes just %, CR and LF, or it shows "%2C".
func TestAnnotationsEscapeOnlyWhatEachPartNeeds(t *testing.T) {
	var b strings.Builder
	annotate(&b, &agentv1.GetCheckVerdictResponse{
		SetupProblem: "Message : ERROR: relation, missing",
		Findings: []*agentv1.Finding{{
			Severity: agentv1.Severity_SEVERITY_ERROR,
			File:     "prisma/migrations/1_require/migration.sql",
			Line:     8,
			Title:    "NOT NULL on a column with nulls: 1, 2",
			Detail:   `Production statistics show nulls in "Invoice"."dueDate". 100% sure.`,
			Fix:      "Backfill the nulls, then enforce NOT NULL: in two steps.",
		}},
	})
	got := b.String()
	for _, want := range []string{
		"::warning title=DbProof setup problem::Message : ERROR: relation, missing\n",
		`title=NOT NULL on a column with nulls%3A 1%2C 2::Production statistics show nulls in "Invoice"."dueDate". 100%25 sure. Backfill the nulls, then enforce NOT NULL: in two steps.` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("annotations are\n%s\nwant them to contain\n%s", got, want)
		}
	}
}
