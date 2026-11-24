package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
)

func testsCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if opts.diff {
		if len(positional) != 0 {
			fmt.Fprintf(stderr, "tests --diff takes no target; pass a base ref or nothing\n\n%s", usage)
			return 2
		}
		return testsDiffCmd(opts, stdout, stderr)
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "tests needs exactly one symbol or file:line (or --diff [base])\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, done, root, code := openAnswerStore(ctx, opts, false, stderr)
	if code != 0 {
		return code
	}
	defer done()

	answer, code, err := query.Tests(ctx, st, positional[0], opts.depth, opts.minConfidence, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		reportUnresolved(stderr, positional[0], code, answer.Target)
		return code
	}

	if opts.json {
		payload := struct {
			query.TestsResult
			Root string `json:"root"`
		}{answer.Result, root}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	printTests(stdout, answer)
	return 0
}

func testsDiffCmd(opts options, stdout, stderr io.Writer) int {
	ctx := context.Background()
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	base, err := index.ResolveDiffBase(ctx, root, opts.diffBase)
	if err != nil {
		fmt.Fprintf(stderr, "tests --diff: %v\n", err)
		return diffExitCode(err)
	}
	changed, err := index.ChangedFiles(ctx, root, base)
	if err != nil {
		fmt.Fprintf(stderr, "tests --diff: %v\n", err)
		return diffExitCode(err)
	}

	// A clean diff needs no index: answer explicitly instead of building one.
	if len(changed) == 0 {
		result := query.EmptyTestsDiff(base, opts.depth, opts.minConfidence)
		return writeTestsDiff(stdout, stderr, root, result, opts.json)
	}

	st, done, code := openDiffStore(ctx, opts, root, stderr)
	if code != 0 {
		return code
	}
	defer done()

	result, err := query.TestsDiff(ctx, st, base, changed, opts.depth, opts.minConfidence, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "tests --diff: %v\n", err)
		return 1
	}
	return writeTestsDiff(stdout, stderr, root, result, opts.json)
}

// diffExitCode keeps bad bases and non-git directories in the documented
// not-found bucket; 5 stays check-only.
func diffExitCode(err error) int {
	switch {
	case errors.Is(err, index.ErrNotGitRepo), errors.Is(err, index.ErrDiffBaseNotFound):
		return 3
	default:
		return 1
	}
}

func writeTestsDiff(stdout, stderr io.Writer, root string, result query.TestsResult, asJSON bool) int {
	if asJSON {
		payload := struct {
			query.TestsResult
			Root string `json:"root"`
		}{result, root}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	printTestsDiff(stdout, result)
	return 0
}

func printTests(out io.Writer, answer query.TestsAnswer) {
	result := answer.Result
	target := result.Targets[0]
	fmt.Fprintf(out, "%s  %s  %s:%d\n", target.QName, target.Kind, target.File, target.Line)
	fmt.Fprintf(out, "%d test files (structural reachability, not runtime coverage)\n", len(result.Tests))
	if len(result.Tests) == 0 {
		fmt.Fprintf(out, "  no test file reaches this target at or above confidence %.2f\n", result.MinConfidence)
		return
	}
	for _, file := range result.Tests {
		fmt.Fprintf(out, "\n%s\n", file.File)
		for _, link := range file.Links {
			symbol := link.TestSymbol
			if symbol == "" {
				symbol = "(file-level evidence)"
			}
			fmt.Fprintf(out, "  %-30s depth %d  %.2f  %-20s via %s\n",
				symbol, link.Depth, link.Confidence, link.Rule, link.AffectedSymbol)
		}
	}
	if result.Truncated {
		fmt.Fprintln(out, "\n… truncated by the reverse scan or test-file limit")
	}
}

func printTestsDiff(out io.Writer, result query.TestsResult) {
	diff := result.Diff
	fmt.Fprintf(out, "diff vs %s · %d changed file(s) · %d test file(s) (structural reachability, not runtime coverage)\n",
		diff.Base, len(diff.Changed), len(result.Tests))
	if len(diff.Changed) == 0 {
		fmt.Fprintln(out, "\nno changed files")
		return
	}
	for _, file := range diff.Changed {
		fmt.Fprintf(out, "  %-12s %s\n", file.Status, file.Path)
	}
	if len(diff.Skipped) > 0 {
		fmt.Fprintln(out, "\nskipped (no current symbols):")
		for _, file := range diff.Skipped {
			fmt.Fprintf(out, "  %-12s %s\n", file.Status, file.Path)
		}
	}
	if len(result.Tests) == 0 {
		fmt.Fprintf(out, "\nno test file reaches the changed files at or above confidence %.2f\n", result.MinConfidence)
		return
	}
	for _, file := range result.Tests {
		fmt.Fprintf(out, "\n%s\n", file.File)
		for _, link := range file.Links {
			symbol := link.TestSymbol
			if symbol == "" {
				symbol = "(file-level evidence)"
			}
			via := link.AffectedSymbol
			if via == "" {
				via = link.AffectedFile
			}
			if link.ChangedFile != "" && link.ChangedFile != via {
				via += " ← " + link.ChangedFile
			}
			fmt.Fprintf(out, "  %-30s depth %d  %.2f  %-20s via %s\n",
				symbol, link.Depth, link.Confidence, link.Rule, via)
		}
	}
	if result.Truncated {
		fmt.Fprintln(out, "\n… truncated by the reverse scan or test-file limit")
	}
}
