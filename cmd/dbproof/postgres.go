package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// checkPostgres is a throwaway Postgres the check started in Docker.
type checkPostgres struct {
	id  string
	dsn string
}

// startPostgres runs image, with {major} replaced by production's major
// version, on a free local port, and waits until it accepts connections.
func startPostgres(ctx context.Context, image string, major int32) (*checkPostgres, error) {
	image = strings.ReplaceAll(image, "{major}", strconv.Itoa(int(major)))
	//nolint:gosec // the workflow's postgres-image input, passed to docker as one argument
	out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=postgres", "-p", "127.0.0.1::5432", image).Output()
	if err != nil {
		return nil, fmt.Errorf("start %s in Docker: %w", image, commandError(err))
	}
	pg := &checkPostgres{id: strings.TrimSpace(string(out))}
	port, err := exec.CommandContext(ctx, "docker", "port", pg.id, "5432/tcp").Output() //nolint:gosec // the container docker just started
	if err != nil {
		pg.stop()
		return nil, fmt.Errorf("find %s's port: %w", image, commandError(err))
	}
	// docker port prints 127.0.0.1:<port>.
	_, p, _ := strings.Cut(strings.TrimSpace(strings.Split(string(port), "\n")[0]), ":")
	pg.dsn = "postgres://postgres:postgres@127.0.0.1:" + p + "/postgres?sslmode=disable"
	// The image restarts Postgres once after initializing, so a single
	// successful connection isn't enough: wait for one that runs a query.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if ready(ctx, pg.dsn) {
			return pg, nil
		}
		if time.Now().After(deadline) {
			pg.stop()
			return nil, fmt.Errorf("%s didn't accept connections within 2 minutes", image)
		}
		select {
		case <-ctx.Done():
			pg.stop()
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func ready(ctx context.Context, dsn string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	return conn.Ping(ctx) == nil
}

// stop removes the container, with its data.
func (pg *checkPostgres) stop() {
	_ = exec.Command("docker", "rm", "-f", pg.id).Run() //nolint:noctx,gosec // the container docker started; runs after the check's context may be done
}

// commandError adds a failed command's stderr to its error.
func commandError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
	}
	return err
}
