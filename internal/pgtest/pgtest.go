// Package pgtest creates throwaway databases on the Postgres servers from
// compose.yaml for tests.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// CaptureRole is a login role with no privileges beyond the defaults, for
// tests of what capture sees without reading the application's tables.
const CaptureRole = "dbproof_capture"

// Server is one Postgres server to test against.
type Server struct {
	Name  string
	URL   string
	Major int
}

// Servers returns the servers listed in DBPROOF_TEST_PG (comma-separated
// admin URLs), defaulting to the compose.yaml servers. Tests that need them
// are skipped with -short.
func Servers(t testing.TB) []Server {
	t.Helper()
	if testing.Short() {
		t.Skip("needs Postgres; run without -short after docker compose up -d")
	}
	list := os.Getenv("DBPROOF_TEST_PG")
	if list == "" {
		list = "postgres://postgres:postgres@localhost:54313/postgres,postgres://postgres:postgres@localhost:54318/postgres"
	}
	var servers []Server
	for _, u := range strings.Split(list, ",") {
		conn := connect(t, u)
		var num int
		if err := conn.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
			t.Fatalf("read server version: %v", err)
		}
		_ = conn.Close(context.Background())
		servers = append(servers, Server{Name: "pg" + strconv.Itoa(num/10000), URL: u, Major: num / 10000})
	}
	return servers
}

// NewDB creates an empty database on the server, dropped when the test ends,
// and returns its admin URL.
func NewDB(t testing.TB, s Server) string {
	t.Helper()
	name := "t_" + randomHex(6)
	admin := connect(t, s.URL)
	defer func() { _ = admin.Close(context.Background()) }()
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c := connect(t, s.URL)
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	return withDatabase(s.URL, name)
}

// Exec runs a SQL script, which may hold many statements, against the
// database at dbURL.
func Exec(t testing.TB, dbURL, script string) {
	t.Helper()
	conn := connect(t, dbURL)
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.PgConn().Exec(context.Background(), script).ReadAll(); err != nil {
		t.Fatalf("run script: %v", err)
	}
}

// ExecFile runs the SQL script in path.
func ExecFile(t testing.TB, dbURL, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	Exec(t, dbURL, string(b))
}

// CaptureURL makes sure CaptureRole exists and returns a URL that connects to
// the same database as it.
func CaptureURL(t testing.TB, dbURL string) string {
	t.Helper()
	Exec(t, dbURL, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+CaptureRole+`') THEN
			CREATE ROLE `+CaptureRole+` LOGIN PASSWORD 'capture';
		END IF;
	END $$`)
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword(CaptureRole, "capture")
	return u.String()
}

// Connect opens a connection closed when the test ends.
func Connect(t testing.TB, dbURL string) *pgx.Conn {
	t.Helper()
	conn := connect(t, dbURL)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func connect(t testing.TB, dbURL string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to Postgres (is docker compose up?): %v", err)
	}
	return conn
}

func withDatabase(serverURL, name string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
