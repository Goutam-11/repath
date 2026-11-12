package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/check"
	"github.com/Goutam-11/repath/internal/query"
)

func checkCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "check takes no arguments\n\n%s", usage)
		return 2
	}
	if !check.ValidFailOn(opts.failOn) {
		fmt.Fprintf(stderr, "--fail-on must be none, error, warning, or info, got %q\n\n%s", opts.failOn, usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	// Parse every SARIF input before the index is opened or refreshed: a bad
	// version or a missing file must be a clean error, never a half-run.
	var sarif *check.SARIFLoad
	if len(opts.sarif) > 0 {
		load, err := check.LoadSARIF(root, opts.sarif)
		if err != nil {
			fmt.Fprintf(stderr, "check: %v\n", err)
			switch {
			case errors.Is(err, check.ErrSARIFNotFound):
				return 3
			case errors.Is(err, check.ErrSARIFInvalid):
				return 2
			default:
				return 1
			}
		}
		sarif = &load
	}

	ctx := context.Background()
	st, err := query.Open(ctx, root, opts.noRefresh)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	if !opts.noRefresh {
		// check is a CI command: analyze the current tree, not a stale index.
		if _, err := query.Status(ctx, st.Store, root, true, opts.full); err != nil {
			fmt.Fprintf(stderr, "refresh: %v\n", err)
			return 1
		}
	}

	report, err := check.Run(ctx, st.Store, root, check.Options{
		MinConfidence: opts.minConfidence,
		MaxFindings:   opts.maxFindings,
		SARIF:         sarif,
	})
	if err != nil {
		fmt.Fprintf(stderr, "check: %v\n", err)
		return 1
	}

	if opts.json {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "encode check: %v\n", err)
			return 1
		}
	} else {
		printCheck(stdout, report)
	}
	return check.ExitCode(report, opts.failOn)
}

func printCheck(out io.Writer, report check.Report) {
	fmt.Fprintf(out, "repath check · %d files · %d nodes · %d edges · min-confidence %.2f\n",
		report.Files, report.Nodes, report.Edges, report.MinConfidence)
	if report.Diagnostics != nil {
		fmt.Fprintf(out, "SARIF: %d source(s) · %d results · %d attached · %d skipped · %d duplicate(s)\n",
			report.Diagnostics.Sources, report.Diagnostics.Results,
			report.Diagnostics.Attached, report.Diagnostics.Skipped,
			report.Diagnostics.Duplicates)
	}

	total := report.Counts.Error + report.Counts.Warning + report.Counts.Info
	if total == 0 {
		fmt.Fprintln(out, "\nno findings")
		return
	}
	fmt.Fprintf(out, "%d error · %d warning · %d info\n", report.Counts.Error, report.Counts.Warning, report.Counts.Info)

	current := ""
	for _, finding := range report.Findings {
		if finding.Severity != current {
			current = finding.Severity
			fmt.Fprintf(out, "\n%s\n", current)
		}
		location := ""
		switch {
		case finding.File != "":
			location = fmt.Sprintf("  %s:%d", finding.File, finding.Line)
		case finding.Module != "":
			location = "  module " + finding.Module
		}
		message := finding.Message
		if finding.Kind == "diagnostic" && finding.QName != "" {
			// The symbol is the attachment point, so make it visible.
			message = finding.QName + ": " + message
		}
		fmt.Fprintf(out, "  %-18s %s%s\n", finding.Kind, message, location)
	}
	if report.Truncated {
		fmt.Fprintf(out, "\n… truncated to %d findings; raise --max-findings to see more\n", report.MaxFindings)
	}
}
