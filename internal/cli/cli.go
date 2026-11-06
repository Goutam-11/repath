// Package cli implements the repath command surface.
//
// Exit codes are part of the contract (PLAN.md §6):
// 0 ok, 1 internal, 2 usage, 3 not found, 4 ambiguous.
package cli

import (
	"fmt"
	"io"
	"strings"
)

// version is set at build time:
//
//	go build -ldflags "-X github.com/Goutam-11/repath/internal/cli.version=v0.1.0"
var version = "dev"

const usage = `repath — structural answers about a codebase

Usage:
  repath index  [--root DIR] [--full] [--json]
  repath status [--root DIR] [--json] [--no-refresh]
  repath version

Commands:
  index    build or update .repath/index.db
  status   report index state, refreshing it first
  version  print the build version

Exit codes:
  0 ok   1 internal   2 usage   3 not found   4 ambiguous
`

// Run executes one command against the given streams and returns the process
// exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "index":
		return indexCmd(args[1:], stdout, stderr)
	case "status":
		return statusCmd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// options are the flags shared by index and status.
type options struct {
	root      string
	json      bool
	full      bool
	noRefresh bool
}

func parseOptions(args []string) (options, error) {
	var o options

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			o.json = true
		case arg == "--full":
			o.full = true
		case arg == "--no-refresh":
			o.noRefresh = true
		case arg == "--root":
			if i+1 >= len(args) {
				return o, fmt.Errorf("--root needs a directory")
			}
			i++
			o.root = args[i]
		case strings.HasPrefix(arg, "--root="):
			o.root = strings.TrimPrefix(arg, "--root=")
		default:
			return o, fmt.Errorf("unknown argument %q", arg)
		}
	}

	return o, nil
}
