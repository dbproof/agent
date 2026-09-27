// Command stratum is Stratum's agent: it captures the production schema and
// checks pull request migrations against it. It never fails a deploy: when
// Stratum is unreachable it warns and exits successfully.
package main

import (
	"fmt"
	"os"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: stratum <command> [flags]

Commands:
  capture   capture the production schema, statistics and migration history
  check     test a pull request's migrations against the latest snapshot
  version   print the agent version

Run "stratum <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "capture":
		os.Exit(runCapture(os.Args[2:]))
	case "check":
		os.Exit(runCheck(os.Args[2:]))
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

// warn prints a warning GitHub Actions shows as an annotation; elsewhere it's
// an ordinary log line.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "::warning::"+format+"\n", args...)
}
