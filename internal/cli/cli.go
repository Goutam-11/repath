// Package cli implements the repath command surface.
//
// Exit codes are part of the contract (PLAN.md §6):
// 0 ok, 1 internal, 2 usage, 3 not found, 4 ambiguous.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
)

// version is set at build time:
//
//	go build -ldflags "-X github.com/Goutam-11/repath/internal/cli.version=v0.1.0"
var version = "dev"

const usage = `repath — structural answers about a codebase

Usage:
  repath version
  repath status [--json]

Commands:
  version   print the build version
  status    report index state

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
	case "status":
		return status(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

type statusReport struct {
	Indexed       bool   `json:"indexed"`
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	Detail        string `json:"detail"`
}

func status(args []string, stdout, stderr io.Writer) int {
	asJSON := false
	for _, arg := range args {
		if arg != "--json" {
			fmt.Fprintf(stderr, "unknown flag %q for status\n", arg)
			return 2
		}
		asJSON = true
	}

	// M0 replaces this with a real read of .repath/index.db.
	report := statusReport{
		Indexed:       false,
		SchemaVersion: 0,
		Version:       version,
		Detail:        "no index: graph engine lands in M0",
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "encode status: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "repath %s\nno index — the graph engine lands in M0\n", version)
	return 0
}
