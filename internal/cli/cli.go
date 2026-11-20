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
  repath index   [--root DIR] [--full] [--json] [--artifact DIR] [--compress]
                 [--scope DIR ...]
  repath pull    <path|url> [--root DIR] [--json]
  repath status  [--root DIR] [--json] [--no-refresh]
  repath find    <symbol> [--root DIR] [--json] [--min-confidence N]
  repath callers <symbol> [--root DIR] [--json] [--min-confidence N]
  repath impact  <symbol|file:line> [--depth N] [--limit N] [--json]
  repath impact  --diff [base] [--depth N] [--limit N] [--json]
  repath tests   <symbol|file:line> [--depth N] [--limit N] [--json]
  repath tests   --diff [base] [--depth N] [--limit N] [--json]
  repath path    <from> <to> [--max-depth N] [--json]
  repath map     [--max-tokens N] [--json]
  repath map     --md [--out DIR] [--max-tokens N] [--artifact DIR|FILE]
  repath export  --graphml|--dot [--modules] [--out FILE] [--min-confidence N]
                 [--artifact DIR|FILE]
  repath flows   <entrypoint> [--depth N] [--limit N] [--json]
  repath check   [--root DIR] [--json] [--min-confidence N]
                 [--fail-on none|error|warning|info] [--max-findings N]
                 [--sarif FILE|DIR ...]
  repath adapter --scip FILE [--root DIR] [--json]
  repath adapter --lsp-facts FILE [--root DIR] [--json]
  repath trace   --file PROFILE [--root DIR] [--limit N] [--json]
  repath install   [--platform claude|codex|cursor|gemini|vscode] [--all] [--force] [--json]
  repath uninstall [--platform NAME|--all] [--purge] [--root DIR] [--json]
  repath mcp     [--root DIR]
  repath serve   [--addr 127.0.0.1:0] [--root DIR]
  repath version

Commands:
  index    build or update .repath/index.db (optionally export an artifact)
  pull     seed the local index from a repath artifact (path or URL)
  status   report index state, refreshing it first
  find     symbol card: location, kind, caller and callee counts
  callers  who calls this symbol
  impact   transitive blast radius of a change (--diff: union over a git change set)
  tests    test files that structurally reach a changed target
  path     how one symbol reaches another
  map      ranked overview: modules, cycles, hot symbols
           (--md writes a committable markdown bundle of the same rollup)
  export   write the graph as GraphML or DOT for Gephi, yEd, Obsidian, or dot
  flows    forward execution paths from a detected entrypoint
  check    graph-native checks: cycles, dead code, boundaries, hotspots
           (opt-in --sarif adds compiler/linter diagnostics; repath never runs one)
  adapter  merge compiler facts (SCIP or LSP facts JSON) into unresolved references
  trace    read-only runtime evidence from an existing V8 CPU profile
  install  copy the repath agent skill into a user-scope agent skill directory
  uninstall  remove the installed skill; --purge also deletes .repath/
  mcp      stdio JSON-RPC server for MCP clients (six answer tools)
  serve    read-only HTTP answers for dashboards and shell-less clients
  version  print the build version

Exit codes:
  0 ok   1 internal   2 usage   3 not found   4 ambiguous   5 check findings
`

// Version reports the build version.
func Version() string { return version }

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
	case "pull":
		return pullCmd(args[1:], stdout, stderr)
	case "status":
		return statusCmd(args[1:], stdout, stderr)
	case "find":
		return findCmd(args[1:], stdout, stderr)
	case "callers":
		return callersCmd(args[1:], stdout, stderr)
	case "impact":
		return impactCmd(args[1:], stdout, stderr)
	case "tests":
		return testsCmd(args[1:], stdout, stderr)
	case "path":
		return pathCmd(args[1:], stdout, stderr)
	case "map":
		return mapCmd(args[1:], stdout, stderr)
	case "export":
		return exportCmd(args[1:], stdout, stderr)
	case "flows":
		return flowsCmd(args[1:], stdout, stderr)
	case "check":
		return checkCmd(args[1:], stdout, stderr)
	case "adapter":
		return adapterCmd(args[1:], stdout, stderr)
	case "trace":
		return traceCmd(args[1:], stdout, stderr)
	case "install":
		return installCmd(args[1:], stdout, stderr)
	case "uninstall":
		return uninstallCmd(args[1:], stdout, stderr)
	case "serve":
		return serveCmd(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// options are the flags shared by the commands.
type options struct {
	root          string
	json          bool
	full          bool
	noRefresh     bool
	minConfidence float64
	depth         int
	limit         int
	maxTokens     int
	module        string
	artifact      string
	compress      bool
	scip          string
	lspFacts      string
	traceFile     string
	sarif         []string
	diff          bool
	diffBase      string
	addr          string
	failOn        string
	maxFindings   int
	platform      string
	scope         []string
	md            bool
	out           string
	modules       bool
	graphml       bool
	dot           bool
	all           bool
	force         bool
	purge         bool
}

// parseOptions returns the flags and the positional arguments.
func parseOptions(args []string) (options, []string, error) {
	o := options{minConfidence: 0.5, limit: 50, maxTokens: 2000, failOn: "error", maxFindings: 200}
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--json":
			o.json = true
		case arg == "--full":
			o.full = true
		case arg == "--compress":
			o.compress = true
		case arg == "--no-refresh":
			o.noRefresh = true
		case arg == "--root":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--root needs a directory")
			}
			i++
			o.root = args[i]
		case strings.HasPrefix(arg, "--root="):
			o.root = strings.TrimPrefix(arg, "--root=")
		case arg == "--min-confidence":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--min-confidence needs a number")
			}
			i++
			if err := setConfidence(&o, args[i]); err != nil {
				return o, nil, err
			}
		case strings.HasPrefix(arg, "--min-confidence="):
			if err := setConfidence(&o, strings.TrimPrefix(arg, "--min-confidence=")); err != nil {
				return o, nil, err
			}
		case arg == "--depth", arg == "--max-depth":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("%s needs a number", arg)
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.depth); err != nil {
				return o, nil, fmt.Errorf("%s %q is not a number", arg, args[i])
			}
		case arg == "--artifact":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--artifact needs a directory")
			}
			i++
			o.artifact = args[i]
		case strings.HasPrefix(arg, "--artifact="):
			o.artifact = strings.TrimPrefix(arg, "--artifact=")
		case arg == "--scip":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--scip needs a SCIP file")
			}
			i++
			o.scip = args[i]
		case strings.HasPrefix(arg, "--scip="):
			o.scip = strings.TrimPrefix(arg, "--scip=")
		case arg == "--lsp-facts":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--lsp-facts needs a JSON file")
			}
			i++
			o.lspFacts = args[i]
		case strings.HasPrefix(arg, "--lsp-facts="):
			o.lspFacts = strings.TrimPrefix(arg, "--lsp-facts=")
		case arg == "--file":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--file needs a profile path")
			}
			i++
			o.traceFile = args[i]
		case strings.HasPrefix(arg, "--file="):
			o.traceFile = strings.TrimPrefix(arg, "--file=")
		case arg == "--addr":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--addr needs host:port")
			}
			i++
			o.addr = args[i]
		case strings.HasPrefix(arg, "--addr="):
			o.addr = strings.TrimPrefix(arg, "--addr=")
		case arg == "--sarif":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--sarif needs a SARIF file or directory")
			}
			i++
			o.sarif = append(o.sarif, args[i])
		case strings.HasPrefix(arg, "--sarif="):
			o.sarif = append(o.sarif, strings.TrimPrefix(arg, "--sarif="))
		case arg == "--diff":
			o.diff = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				o.diffBase = args[i]
			}
		case strings.HasPrefix(arg, "--diff="):
			o.diff = true
			o.diffBase = strings.TrimPrefix(arg, "--diff=")
		case arg == "--fail-on":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--fail-on needs none, error, warning, or info")
			}
			i++
			o.failOn = args[i]
		case strings.HasPrefix(arg, "--fail-on="):
			o.failOn = strings.TrimPrefix(arg, "--fail-on=")
		case arg == "--platform":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--platform needs a name")
			}
			i++
			o.platform = args[i]
		case strings.HasPrefix(arg, "--platform="):
			o.platform = strings.TrimPrefix(arg, "--platform=")
		case arg == "--scope":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--scope needs a directory")
			}
			i++
			o.scope = append(o.scope, args[i])
		case strings.HasPrefix(arg, "--scope="):
			o.scope = append(o.scope, strings.TrimPrefix(arg, "--scope="))
		case arg == "--md":
			o.md = true
		case arg == "--modules":
			o.modules = true
		case arg == "--graphml":
			o.graphml = true
		case arg == "--dot":
			o.dot = true
		case arg == "--out":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--out needs a path")
			}
			i++
			o.out = args[i]
		case strings.HasPrefix(arg, "--out="):
			o.out = strings.TrimPrefix(arg, "--out=")
		case arg == "--all":
			o.all = true
		case arg == "--force":
			o.force = true
		case arg == "--purge":
			o.purge = true
		case arg == "--max-findings":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--max-findings needs a number")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.maxFindings); err != nil || o.maxFindings <= 0 {
				return o, nil, fmt.Errorf("--max-findings %q is not a positive number", args[i])
			}
		case strings.HasPrefix(arg, "--max-findings="):
			raw := strings.TrimPrefix(arg, "--max-findings=")
			if _, err := fmt.Sscanf(raw, "%d", &o.maxFindings); err != nil || o.maxFindings <= 0 {
				return o, nil, fmt.Errorf("--max-findings %q is not a positive number", raw)
			}
		case arg == "--module":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--module needs a name")
			}
			i++
			o.module = args[i]
		case strings.HasPrefix(arg, "--module="):
			o.module = strings.TrimPrefix(arg, "--module=")
		case arg == "--limit":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--limit needs a number")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.limit); err != nil {
				return o, nil, fmt.Errorf("--limit %q is not a number", args[i])
			}
		case arg == "--max-tokens":
			if i+1 >= len(args) {
				return o, nil, fmt.Errorf("--max-tokens needs a number")
			}
			i++
			if _, err := fmt.Sscanf(args[i], "%d", &o.maxTokens); err != nil {
				return o, nil, fmt.Errorf("--max-tokens %q is not a number", args[i])
			}
		case strings.HasPrefix(arg, "--"):
			return o, nil, fmt.Errorf("unknown argument %q", arg)
		default:
			positional = append(positional, arg)
		}
	}

	return o, positional, nil
}

func setConfidence(o *options, raw string) error {
	var v float64
	if _, err := fmt.Sscanf(raw, "%f", &v); err != nil {
		return fmt.Errorf("--min-confidence %q is not a number", raw)
	}
	if v < 0 || v > 1 {
		return fmt.Errorf("--min-confidence must be between 0 and 1, got %q", raw)
	}
	o.minConfidence = v
	return nil
}
