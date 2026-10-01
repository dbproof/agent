package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Without a database to use, the check starts production's Postgres major in
// Docker and removes it afterwards.
func TestStartPostgres(t *testing.T) {
	ctx := context.Background()
	pg, err := startPostgres(ctx, "postgres:{major}-alpine", 13)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, pg.dsn)
	if err != nil {
		pg.stop()
		t.Fatal(err)
	}
	var major int32
	err = conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int / 10000").Scan(&major)
	_ = conn.Close(ctx)
	pg.stop()
	if err != nil || major != 13 {
		t.Fatalf("server major = %d, %v; want 13", major, err)
	}
	out, err := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter", "id="+pg.id, "--format", "{{.ID}}").Output()
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Errorf("container still there after stop: %q, %v", out, err)
	}
}
