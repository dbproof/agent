package check

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/dbproof/agent/snapshot"
)

// CommandMigrator runs the customer's own migrate command, such as
// `flyway migrate` or `atlas migrate apply`, through the shell. The target
// flag for each version is appended, and the command finds the throwaway
// database in DBPROOF_CHECK_* variables.
type CommandMigrator struct {
	Command string
	Tool    snapshot.Tool
	// DSN is the throwaway database's postgres:// URL.
	DSN string
}

// Env returns the variables the command sees, so a command can say
// -url=$DBPROOF_CHECK_JDBC_URL or --url "$DBPROOF_CHECK_DSN".
func (m CommandMigrator) Env() ([]string, error) {
	u, err := url.Parse(m.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse check database URL: %w", err)
	}
	password, _ := u.User.Password()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	db := strings.TrimPrefix(u.Path, "/")
	return []string{
		"DBPROOF_CHECK_DSN=" + m.DSN,
		"DBPROOF_CHECK_JDBC_URL=jdbc:postgresql://" + u.Hostname() + ":" + port + "/" + db,
		"DBPROOF_CHECK_HOST=" + u.Hostname(),
		"DBPROOF_CHECK_PORT=" + port,
		"DBPROOF_CHECK_DB=" + db,
		"DBPROOF_CHECK_USER=" + u.User.Username(),
		"DBPROOF_CHECK_PASSWORD=" + password,
		// Flyway reads these when the command doesn't say where to migrate,
		// ahead of a flyway.conf that could name another database.
		"FLYWAY_URL=jdbc:postgresql://" + u.Hostname() + ":" + port + "/" + db,
		"FLYWAY_USER=" + u.User.Username(),
		"FLYWAY_PASSWORD=" + password,
	}, nil
}

// Migrate runs the command up to target and returns its combined output.
func (m CommandMigrator) Migrate(ctx context.Context, target string) (string, error) {
	env, err := m.Env()
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", m.Command+targetArgs(m.Tool, target)) //nolint:gosec // the workflow's own migrate command, run as it asks
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err = cmd.Run()
	return out.String(), err
}
