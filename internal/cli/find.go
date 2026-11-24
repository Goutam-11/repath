package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Goutam-11/repath/internal/query"
)

func findCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "find needs exactly one symbol name\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, done, _, code := openAnswerStore(ctx, opts, false, stderr)
	if code != 0 {
		return code
	}
	defer done()

	answer, code, err := query.Find(ctx, st, positional[0], opts.module, opts.minConfidence)
	if err != nil {
		fmt.Fprintf(stderr, "search: %v\n", err)
		return 1
	}
	if code != 0 {
		return reportLookup(stdout, stderr, opts.json, positional[0], code, answer.Target)
	}

	result := answer.Result
	if opts.json {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	exported := ""
	if result.Symbol.Exported {
		exported = " · exported"
	}
	fmt.Fprintf(stdout, "%s  %s  %s:%d%s\n",
		result.Symbol.QName, result.Symbol.Kind, result.Symbol.File, result.Symbol.Line, exported)
	fmt.Fprintf(stdout, "callers %d · callees %d · min-confidence %.2f\n",
		result.Callers, result.Callees, result.MinConfidence)
	return 0
}

func callersCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "callers needs exactly one symbol name\n\n%s", usage)
		return 2
	}
	depth := opts.depth
	if depth == 0 {
		depth = 1
	}
	if depth != 1 {
		fmt.Fprintln(stderr, "callers supports --depth 1; use impact for transitive reach")
		return 2
	}

	ctx := context.Background()
	st, done, _, code := openAnswerStore(ctx, opts, false, stderr)
	if code != 0 {
		return code
	}
	defer done()

	answer, code, err := query.Callers(ctx, st, positional[0], opts.minConfidence, 0)
	if err != nil {
		fmt.Fprintf(stderr, "read callers: %v\n", err)
		return 1
	}
	if code != 0 {
		return reportLookup(stdout, stderr, opts.json, positional[0], code, answer.Target)
	}

	result := answer.Result
	if opts.json {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "callers of %s  (%s:%d)  min-confidence %.2f\n",
		result.Symbol.QName, result.Symbol.File, result.Symbol.Line, result.MinConfidence)
	if len(result.Callers) == 0 {
		fmt.Fprintln(stdout, "  none above the threshold")
		return 0
	}
	for _, e := range result.Callers {
		fmt.Fprintf(stdout, "  %-40s %s:%d  %.2f  %s\n",
			e.QName, e.File, e.Line, e.Confidence, strings.ToLower(e.Kind))
	}
	if result.Truncated {
		fmt.Fprintln(stdout, "  … truncated")
	}
	return 0
}

// reportLookup renders a not-found (3) or ambiguous (4) result in the command's
// chosen format and returns the exit code.
func reportLookup(stdout, stderr io.Writer, jsonOut bool, name string, code int, target query.Target) int {
	if jsonOut {
		type candidate struct {
			QName string `json:"qname"`
			Kind  string `json:"kind"`
		}
		payload := struct {
			Error      string      `json:"error"`
			Name       string      `json:"name"`
			Candidates []candidate `json:"candidates,omitempty"`
		}{Name: name}
		if code == 3 {
			payload.Error = "not found"
		} else {
			payload.Error = "ambiguous"
		}
		for _, m := range target.Candidates {
			payload.Candidates = append(payload.Candidates, candidate{QName: m.QName, Kind: m.Kind})
		}
		_ = writeJSON(stdout, payload)
		return code
	}

	reportUnresolved(stderr, name, code, target)
	return code
}

func reportUnresolved(out io.Writer, name string, code int, target query.Target) {
	if code == 3 {
		fmt.Fprintf(out, "not found: %s\n", name)
		return
	}
	fmt.Fprintf(out, "ambiguous: %s matches %d symbols\n", name, len(target.Candidates))
	for _, m := range target.Candidates {
		fmt.Fprintf(out, "  %s  %s\n", m.QName, m.Kind)
	}
	fmt.Fprintln(out, "use a qualified name to disambiguate")
}
