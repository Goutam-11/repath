// Command repath indexes a codebase into a structural graph and answers
// questions about it: what breaks, how two things connect, where the important
// code is.
package main

import (
	"os"

	"github.com/Goutam-11/repath/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
