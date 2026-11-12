package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/query"
)

func testsCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "tests needs exactly one symbol or file:line\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	answer, code, err := query.Tests(ctx, st.Store, positional[0], opts.depth, opts.minConfidence, opts.limit)
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
