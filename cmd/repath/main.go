// Command repath indexes a codebase into a structural graph and answers
// questions about it: what breaks, how two things connect, where the important
// code is.
package main

import (
	"os"

	"github.com/Goutam-11/repath/internal/cli"
	"github.com/Goutam-11/repath/internal/mcp"
)

func main() {
	// MCP is a long-lived stdio transport, so it owns stdin/stdout directly
	// instead of going through the one-shot CLI dispatcher.
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		os.Exit(mcp.Main(os.Args[2:], cli.Version(), os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
