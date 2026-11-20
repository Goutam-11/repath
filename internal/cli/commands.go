package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
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
	if len(r.Scope) > 0 {
		fmt.Fprintf(out, "scope %s (%d files)\n", strings.Join(r.Scope, ", "), r.ScopeFiles)
	}

	kinds := make([]string, 0, len(r.NodesByKind))
	for kind := range r.NodesByKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		fmt.Fprintf(out, "  %-10s %d\n", kind, r.NodesByKind[kind])
	}

	if r.Entrypoints > 0 {
		fmt.Fprintf(out, "%d entrypoints", r.Entrypoints)
		if len(r.EntrypointRules) > 0 {
			rules := make([]string, 0, len(r.EntrypointRules))
			for rule := range r.EntrypointRules {
				rules = append(rules, rule)
			}
			sort.Strings(rules)
			parts := make([]string, 0, len(rules))
			for _, rule := range rules {
				parts = append(parts, fmt.Sprintf("%s %d", rule, r.EntrypointRules[rule]))
			}
			fmt.Fprintf(out, " (%s)", strings.Join(parts, " · "))
		}
		fmt.Fprintln(out)
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
	if r.Unresolved > 0 && len(r.UnresolvedByReason) > 0 {
		reasons := make([]string, 0, len(store.PendingReasons))
		for _, reason := range store.PendingReasons {
			reasons = append(reasons, fmt.Sprintf("%s %d", reason, r.UnresolvedByReason[reason]))
		}
		fmt.Fprintf(out, "unresolved reasons: %s\n", strings.Join(reasons, " · "))
		if len(r.UnresolvedTopNames) > 0 {
			names := make([]string, 0, len(r.UnresolvedTopNames))
			for _, name := range r.UnresolvedTopNames {
				names = append(names, fmt.Sprintf("%s (%d)", name.Name, name.Count))
			}
			fmt.Fprintf(out, "unresolved top names: %s\n", strings.Join(names, " · "))
		}
	}

	if r.LastIndexed != "" {
		fmt.Fprintf(out, "indexed %s · +%d ~%d -%d in %dms\n",
			r.LastIndexed, r.Refresh.FilesIndexed, r.Refresh.FilesSkipped,
			r.Refresh.FilesDeleted, r.Refresh.DurationMS)
	}
	if len(r.Adapters) > 0 {
		for _, a := range r.Adapters {
			fmt.Fprintf(out, "adapter %s: %d definitions · %d refs · %d matched · %d edges · %d conflicts · %d skipped\n",
				a.Adapter, a.Definitions, a.References, a.Matched, a.Edges, a.Conflicts, a.Skipped)
		}
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

	if opts.compress && opts.artifact == "" {
		fmt.Fprintf(stderr, "--compress requires --artifact DIR\n\n%s", usage)
		return 2
	}
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	scope, err := index.CleanScope(opts.scope)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	var scoped []string
	if len(scope) > 0 {
		for _, item := range scope {
			info, statErr := os.Stat(filepath.Join(root, filepath.FromSlash(item)))
			if statErr != nil || !info.IsDir() {
				fmt.Fprintf(stderr, "--scope %q is not a directory in this repository\n\n%s", item, usage)
				return 2
			}
		}
		scoped = scope
	}

	ctx := context.Background()
	st, err := query.OpenScoped(ctx, root, true, scoped)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	// Without --scope the command asks for the whole repository: an index that
	// was built with a scope is rebuilt rather than served under a new label.
	result, err := index.Run(ctx, st.Store, root, index.Options{
		Full:      opts.full,
		Scope:     scoped,
		WholeRepo: len(scoped) == 0,
	})
	if err != nil {
		fmt.Fprintf(stderr, "index: %v\n", err)
		return 1
	}
	_ = index.StoreCommit(ctx, root, st.Store)

	var exported *artifact.ExportResult
	if opts.artifact != "" {
		out, err := artifact.ExportWithOptions(ctx, st.Store, root, opts.artifact, version, artifact.Options{Compress: opts.compress})
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
			Scope         []string               `json:"scope,omitempty"`
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
			Scope:         scoped,
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
	if len(scoped) > 0 {
		fmt.Fprintf(stdout, "scope %s (%d files)\n", strings.Join(scoped, ", "), result.Files)
	}
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
