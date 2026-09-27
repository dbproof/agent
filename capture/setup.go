package capture

import (
	_ "embed"
	"strings"
	"text/template"

	"github.com/stratum-dev/agent/schema"
	"github.com/stratum-dev/agent/snapshot"
)

// Role is the name of the capture role the setup script creates.
const Role = "stratum_capture"

//go:embed setup.sql.tmpl
var setupSQL string

var setupTemplate = template.Must(template.New("setup").Parse(setupSQL))

// SetupSQL returns the script a DBA runs once to create the capture role and
// the statistics function. historyTable is "schema.table"; empty means the
// tool's default.
func SetupSQL(tool snapshot.Tool, historyTable string) (string, error) {
	if historyTable == "" {
		historyTable = DefaultHistoryTable(tool)
	}
	ident, err := quoteTable(historyTable)
	if err != nil {
		return "", err
	}
	schemaName, _, _ := strings.Cut(historyTable, ".")
	names := map[snapshot.Tool]string{snapshot.ToolFlyway: "Flyway", snapshot.ToolAtlas: "Atlas"}
	var b strings.Builder
	err = setupTemplate.Execute(&b, map[string]string{
		"Tool":          names[tool],
		"Role":          Role,
		"HistorySchema": schema.QuoteIdent(schemaName),
		"HistoryTable":  ident,
	})
	return b.String(), err
}
