# DbProof agent. Tests need the Postgres servers from compose.yaml.

golangci_version := "v2.14.0"

export PATH := justfile_directory() / "bin" + ":" + env_var("PATH")

# Install pinned tools into ./bin: buf and protoc plugins from tools/go.mod,
# and golangci-lint.
tools:
    GOWORK=off go -C tools build -o ../bin/ tool
    test -x bin/golangci-lint && bin/golangci-lint version | grep -q "{{ trim_start_match(golangci_version, 'v') }}" || \
      curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b bin {{ golangci_version }}

# Regenerate Go code from proto/.
gen:
    buf generate

# Lint and run every test, starting Postgres if needed.
check: tools gen
    docker compose up -d --wait
    buf lint
    golangci-lint run ./...
    go test ./...
