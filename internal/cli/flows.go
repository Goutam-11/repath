package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/query"
)

func flowsCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "flows needs exactly one entrypoint\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, done, root, code := openAnswerStore(ctx, opts, false, stderr)
	if code != 0 {
		return code
	}
	defer done()

	depth := opts.depth
	if depth <= 0 {
		depth = 6
	}
	limit := opts.limit
	if limit <= 0 {
		limit = 20
	}
	answer, code, err := query.Flows(ctx, st, positional[0], depth, opts.minConfidence, limit)
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
			graph.FlowResult
			Root string `json:"root"`
		}{answer.Result, root}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	printFlows(stdout, answer)
	return 0
}

func printFlows(out io.Writer, answer query.FlowsAnswer) {
	result := answer.Result
	fmt.Fprintf(out, "%s  %s  %s:%d  rule %s\n",
		result.Entrypoint.QName, result.Entrypoint.Kind, result.Entrypoint.File,
		result.Entrypoint.Line, result.EntrypointRule)
	if len(result.Paths) == 0 {
		fmt.Fprintf(out, "no outgoing edges at or above confidence %.2f; frontier %s\n",
			result.MinConfidence, result.Entrypoint.QName)
		return
	}
	for i, path := range result.Paths {
		fmt.Fprintf(out, "\npath %d\n", i+1)
		for _, step := range path {
			fmt.Fprintf(out, "  depth %d  %-28s %s:%d  %.2f  %s → %s\n",
				step.Depth, step.QName, step.File, step.Line, step.Confidence,
				step.Rule, step.EdgeKind)
		}
	}
	if result.Truncated {
		fmt.Fprintln(out, "\n… truncated by depth or path limit")
	}
}
