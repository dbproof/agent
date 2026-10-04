# DbProof agent. Tests need the Postgres servers from compose.yaml.

golangci_version := "v2.14.0"
atlas_version := "v1.3.0"

export PATH := justfile_directory() / "bin" + ":" + env_var("PATH")

# Install pinned tools into ./bin: buf, protoc plugins and gomsort from
# tools/go.mod, golangci-lint, and the official Atlas build setup-atlas gives
# customers, which check tests run.
tools:
    #!/usr/bin/env bash
    set -euo pipefail
    mkdir -p bin
    GOWORK=off go -C tools build -o ../bin/ tool
    if ! bin/golangci-lint version 2>/dev/null | grep -q "{{ trim_start_match(golangci_version, 'v') }}"; then
      curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b bin {{ golangci_version }}
    fi
    if ! bin/atlas version 2>/dev/null | grep -q "{{ atlas_version }}"; then
      os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
      curl -fsSL -o bin/atlas "https://atlasbinaries.com/atlas/atlas-$os-$arch-{{ atlas_version }}"
      chmod +x bin/atlas
    fi

# Regenerate Go code from proto/.
gen:
    buf generate

# Lint and run every test, starting Postgres if needed.
check: tools gen
    docker compose up -d --wait
    buf lint
    golangci-lint run ./...
    @just sorted
    go test ./...

# Fail if any type's methods aren't laid out as gomsort lays them out, after
# the Uber Go style guide: grouped by type, exported first, then the rest in
# call order. Run `bin/gomsort .` to sort them.
sorted:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(gomsort -n .)
    if [ -n "$out" ]; then
      echo "$out" >&2
      echo "Methods are out of order; run bin/gomsort . to sort them." >&2
      exit 1
    fi
