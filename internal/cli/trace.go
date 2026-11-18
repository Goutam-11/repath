package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/trace"
)

// traceCmd imports an existing runtime CPU profile and reports the calls it
// observed against the static graph. It is read-only: no index row changes.
func traceCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 || opts.traceFile == "" {
		fmt.Fprintf(stderr, "trace needs --file PROFILE and no positional arguments\n\n%s", usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}
	ctx := context.Background()
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	report, err := trace.Import(ctx, st.Store, opts.traceFile, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "trace: %v\n", err)
		if errors.Is(err, trace.ErrNotFound) {
			return 3
		}
		if errors.Is(err, trace.ErrMalformed) {
			return 2
		}
		return 1
	}
	if opts.json {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "encode trace: %v\n", err)
			return 1
		}
		return 0
	}
	printTrace(stdout, report)
	return 0
}

func printTrace(out io.Writer, report trace.Report) {
	fmt.Fprintf(out, "trace %s (%s): %d frames · %d mapped · %d skipped\n",
		report.File, report.Format, report.Frames, report.Mapped, report.Skipped)
	if len(report.SkippedReasons) > 0 {
		reasons := make([]string, 0, len(report.SkippedReasons))
		for reason := range report.SkippedReasons {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		parts := make([]string, 0, len(reasons))
		for _, reason := range reasons {
			parts = append(parts, fmt.Sprintf("%s %d", reason, report.SkippedReasons[reason]))
		}
		fmt.Fprintf(out, "  skipped: %s\n", joinComma(parts))
	}
	fmt.Fprintf(out, "%d observed edges · %d confirmed by the static graph · %d observed only\n",
		report.ObservedEdges, report.Confirmed, report.ObservedOnly)
	if len(report.Edges) == 0 {
		fmt.Fprintln(out, "  no observed caller→callee edges")
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  %6s  %-40s %-10s %s\n", "count", "call", "rule", "evidence")
	for _, edge := range report.Edges {
		evidence := "observed only"
		if edge.Static {
			evidence = "static"
		}
		fmt.Fprintf(out, "  %6d  %-40s %-10s %s\n", edge.Count,
			edge.Caller.QName+" → "+edge.Callee.QName, edge.Rule, evidence)
	}
	if report.Truncated {
		fmt.Fprintln(out, "  … truncated at --limit")
	}
}

func joinComma(parts []string) string {
	out := ""
	for i, part := range parts {
		if i > 0 {
			out += ", "
		}
		out += part
	}
	return out
}
