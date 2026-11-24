package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
)

// impactDiffCmd unions the per-file impact answers for a git change set. It
// reuses index.ChangedFiles and query.ImpactDiff; there is no second diff
// engine.
func impactDiffCmd(opts options, stdout, stderr io.Writer) int {
	ctx := context.Background()
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	base, err := index.ResolveDiffBase(ctx, root, opts.diffBase)
	if err != nil {
		fmt.Fprintf(stderr, "impact --diff: %v\n", err)
		return diffExitCode(err)
	}
	changed, err := index.ChangedFiles(ctx, root, base)
	if err != nil {
		fmt.Fprintf(stderr, "impact --diff: %v\n", err)
		return diffExitCode(err)
	}

	// A clean diff needs no index: answer explicitly instead of building one.
	if len(changed) == 0 {
		result := query.EmptyImpactDiff(base, opts.depth, opts.minConfidence)
		return writeImpactDiff(stdout, stderr, root, result, opts.json)
	}

	st, done, code := openDiffStore(ctx, opts, root, stderr)
	if code != 0 {
		return code
	}
	defer done()

	result, err := query.ImpactDiff(ctx, st, base, changed, opts.depth, opts.minConfidence, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "impact --diff: %v\n", err)
		return 1
	}
	return writeImpactDiff(stdout, stderr, root, result, opts.json)
}

func writeImpactDiff(stdout, stderr io.Writer, root string, result query.ImpactDiffResult, asJSON bool) int {
	if asJSON {
		payload := struct {
			query.ImpactDiffResult
			Root string `json:"root"`
		}{result, root}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}
	printImpactDiff(stdout, result)
	return 0
}

func printImpactDiff(out io.Writer, result query.ImpactDiffResult) {
	diff := result.Diff
	fmt.Fprintf(out, "diff vs %s · %d changed file(s) · %d dependents · %d files · %d modules · min-confidence %.2f · depth %d\n",
		diff.Base, len(diff.Changed), len(result.Dependents), len(result.Files), len(result.Modules),
		result.MinConfidence, result.Depth)
	for _, file := range diff.Changed {
		fmt.Fprintf(out, "  %-12s %s\n", file.Status, file.Path)
	}
	if len(diff.Skipped) > 0 {
		fmt.Fprintln(out, "\nskipped (deleted or no current symbols):")
		for _, file := range diff.Skipped {
			fmt.Fprintf(out, "  %-12s %s\n", file.Status, file.Path)
		}
	}
	if len(diff.Changed) == 0 {
		fmt.Fprintln(out, "\nno changed files")
		return
	}
	if len(result.Dependents) == 0 {
		fmt.Fprintf(out, "\nnothing reaches the changed files at or above confidence %.2f\n", result.MinConfidence)
		return
	}

	byDepth := make(map[int][]query.ImpactDependent)
	maxDepth := 0
	for _, dependent := range result.Dependents {
		byDepth[dependent.Depth] = append(byDepth[dependent.Depth], dependent)
		if dependent.Depth > maxDepth {
			maxDepth = dependent.Depth
		}
	}

	lines := 0
	const budget = 34
	for depth := 1; depth <= maxDepth; depth++ {
		list := byDepth[depth]
		if len(list) == 0 {
			continue
		}
		if lines >= budget {
			fmt.Fprintf(out, "\n… truncated; use --json or --depth to see the rest\n")
			break
		}
		fmt.Fprintf(out, "\ndepth %d\n", depth)
		lines += 2
		for i, dependent := range list {
			if i >= 10 || lines >= budget {
				fmt.Fprintf(out, "  +%d more at depth %d\n", len(list)-i, depth)
				break
			}
			fmt.Fprintf(out, "  %-42s %s:%d  %.2f  %-12s ← %s\n",
				dependent.QName, dependent.File, dependent.Line, dependent.Confidence,
				strings.ToLower(dependent.EdgeKind), dependent.ChangedFile)
			lines++
		}
	}

	if len(result.Files) > 0 {
		fmt.Fprintln(out, "\naffected files")
		for i, file := range result.Files {
			if i >= 8 {
				fmt.Fprintf(out, "  +%d more\n", len(result.Files)-i)
				break
			}
			fmt.Fprintf(out, "  %-52s %d\n", file.Path, file.Symbols)
		}
	}
	if result.Truncated {
		fmt.Fprintln(out, "\n… truncated by the per-file scan or the overall limit")
	}
}
