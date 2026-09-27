# Stratum agent. Tests need the Postgres servers from compose.yaml.

golangci_version := "v2.14.0"

export PATH := justfile_directory() / "bin" + ":" + env_var("PATH")

# Install pinned tools into ./bin.
tools:
    test -x bin/golangci-lint && bin/golangci-lint version | grep -q "{{ trim_start_match(golangci_version, 'v') }}" || \
      curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b bin {{ golangci_version }}

# Lint and run every test, starting Postgres if needed.
check: tools
    docker compose up -d --wait
    golangci-lint run ./...
    go test ./...
