package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
)

// statusReport is the `status --json` contract, owned by the shared query layer.
type statusReport = query.StatusReport
type refreshReport = query.RefreshReport

func statusCmd(args []string, stdout, stderr io.Writer) int {
	opts, _, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, err := query.Open(ctx, root, true)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	report, err := query.Status(ctx, st.Store, root, !opts.noRefresh, opts.full)
	if err != nil {
		fmt.Fprintf(stderr, "status: %v\n", err)
		return 1
	}
	report.Version = version

	if opts.json {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "encode status: %v\n", err)
			return 1
		}
		return 0
	}

	printStatus(stdout, report)
	return 0
}

func printStatus(out io.Writer, r statusReport) {
	fmt.Fprintf(out, "repath %s · %s\n", r.Version, r.Root)
	fmt.Fprintf(out, "%d files · %d nodes · %d parse failures\n", r.Files, r.Nodes, r.ParseFailures)

	kinds := make([]string, 0, len(r.NodesByKind))
	for kind := range r.NodesByKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(out, "  %-10s %d\n", kind, r.NodesByKind[kind])
	}

	if len(r.Edges) > 0 {
		kinds := make([]string, 0, len(r.Edges))
		for kind := range r.Edges {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		parts := make([]string, 0, len(kinds))
		for _, kind := range kinds {
			parts = append(parts, fmt.Sprintf("%s %d", kind, r.Edges[kind]))
		}
		fmt.Fprintf(out, "edges %s · ambiguous %d (%.1f%%) · unresolved %d (%.1f%%)\n",
			strings.Join(parts, " · "), r.Ambiguous, r.AmbiguityRate*100, r.Unresolved, r.UnresolvedRate*100)
	}

	if r.LastIndexed != "" {
		fmt.Fprintf(out, "indexed %s · +%d ~%d -%d in %dms\n",
			r.LastIndexed, r.Refresh.FilesIndexed, r.Refresh.FilesSkipped,
			r.Refresh.FilesDeleted, r.Refresh.DurationMS)
	}
	if r.Stale {
		fmt.Fprintf(out, "stale: index at %s, HEAD at %s\n", shortSHA(r.GitHead), shortSHA(r.CurrentHead))
	}
	if r.DirtyFiles > 0 {
		fmt.Fprintf(out, "dirty: %d changed or untracked files\n", r.DirtyFiles)
	}
}

func indexCmd(args []string, stdout, stderr io.Writer) int {
	opts, _, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, err := query.Open(ctx, root, true)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	result, err := index.Run(ctx, st.Store, root, index.Options{Full: opts.full})
	if err != nil {
		fmt.Fprintf(stderr, "index: %v\n", err)
		return 1
	}
	_ = index.StoreCommit(ctx, root, st.Store)

	var exported *artifact.ExportResult
	if opts.artifact != "" {
		out, err := artifact.Export(ctx, st.Store, root, opts.artifact, version)
		if err != nil {
			fmt.Fprintf(stderr, "artifact: %v\n", err)
			return 1
		}
		exported = &out
	}

	if opts.json {
		payload := struct {
			Root          string                 `json:"root"`
			Files         int                    `json:"files"`
			FilesIndexed  int                    `json:"files_indexed"`
			FilesSkipped  int                    `json:"files_skipped"`
			FilesDeleted  int                    `json:"files_deleted"`
			Nodes         int                    `json:"nodes"`
			ParseFailures int                    `json:"parse_failures"`
			DurationMS    int64                  `json:"duration_ms"`
			Artifact      *artifact.ExportResult `json:"artifact,omitempty"`
		}{
			Root:          root,
			Files:         result.Files,
			FilesIndexed:  result.Indexed,
			FilesSkipped:  result.Unchanged,
			FilesDeleted:  result.Deleted,
			Nodes:         result.Nodes,
			ParseFailures: result.ParseFailures,
			DurationMS:    result.Duration.Milliseconds(),
			Artifact:      exported,
		}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode result: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "indexed %d files (+%d ~%d -%d), %d nodes, %d parse failures in %dms\n",
		result.Files, result.Indexed, result.Unchanged, result.Deleted,
		result.Nodes, result.ParseFailures, result.Duration.Milliseconds())
	if exported != nil {
		fmt.Fprintf(stdout, "artifact %s + %s (schema v%d · %d files · %d nodes · %d edges)\n",
			exported.DBPath, exported.ManifestPath, exported.Manifest.SchemaVersion,
			exported.Manifest.FileCount, exported.Manifest.NodeCount, exported.Manifest.EdgeCount)
	}
	return 0
}

func writeJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
