package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// statusReport is the `status --json` contract.
type statusReport struct {
	Root          string         `json:"root"`
	Indexed       bool           `json:"indexed"`
	SchemaVersion int            `json:"schema_version"`
	Version       string         `json:"version"`
	Files         int            `json:"files"`
	Nodes         int            `json:"nodes"`
	NodesByKind   map[string]int `json:"nodes_by_kind"`
	ParseFailures int            `json:"parse_failures"`
	LastIndexed   string         `json:"last_indexed,omitempty"`
	GitHead       string         `json:"git_head,omitempty"`
	Refresh       refreshReport  `json:"refresh"`
}

type refreshReport struct {
	FilesIndexed int   `json:"files_indexed"`
	FilesSkipped int   `json:"files_skipped"`
	FilesDeleted int   `json:"files_deleted"`
	DurationMS   int64 `json:"duration_ms"`
}

func statusCmd(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}

	root, err := resolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, err := openIndex(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	var refresh index.Result
	if !opts.noRefresh {
		refresh, err = index.Run(ctx, st, root, index.Options{Full: opts.full})
		if err != nil {
			fmt.Fprintf(stderr, "index: %v\n", err)
			return 1
		}
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "read index: %v\n", err)
		return 1
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "read index: %v\n", err)
		return 1
	}

	report := statusReport{
		Root:          root,
		Indexed:       counts.Files > 0,
		SchemaVersion: store.SchemaVersion,
		Version:       version,
		Files:         counts.Files,
		Nodes:         counts.Nodes,
		NodesByKind:   counts.ByKind,
		ParseFailures: counts.ParseFailures,
		LastIndexed:   meta["indexed_at"],
		GitHead:       meta["git_head"],
		Refresh: refreshReport{
			FilesIndexed: refresh.Indexed,
			FilesSkipped: refresh.Unchanged,
			FilesDeleted: refresh.Deleted,
			DurationMS:   refresh.Duration.Milliseconds(),
		},
	}

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

	if r.LastIndexed != "" {
		fmt.Fprintf(out, "indexed %s · +%d ~%d -%d in %dms\n",
			r.LastIndexed, r.Refresh.FilesIndexed, r.Refresh.FilesSkipped,
			r.Refresh.FilesDeleted, r.Refresh.DurationMS)
	}
}

func indexCmd(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}

	root, err := resolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, err := openIndex(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	result, err := index.Run(ctx, st, root, index.Options{Full: opts.full})
	if err != nil {
		fmt.Fprintf(stderr, "index: %v\n", err)
		return 1
	}

	if opts.json {
		payload := struct {
			Root          string `json:"root"`
			Files         int    `json:"files"`
			FilesIndexed  int    `json:"files_indexed"`
			FilesSkipped  int    `json:"files_skipped"`
			FilesDeleted  int    `json:"files_deleted"`
			Nodes         int    `json:"nodes"`
			ParseFailures int    `json:"parse_failures"`
			DurationMS    int64  `json:"duration_ms"`
		}{
			Root:          root,
			Files:         result.Files,
			FilesIndexed:  result.Indexed,
			FilesSkipped:  result.Unchanged,
			FilesDeleted:  result.Deleted,
			Nodes:         result.Nodes,
			ParseFailures: result.ParseFailures,
			DurationMS:    result.Duration.Milliseconds(),
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
	return 0
}

func openIndex(ctx context.Context, root string) (*store.Store, error) {
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, fmt.Errorf("migrate index: %w", err)
	}
	return st, nil
}

// resolveRoot prefers the explicit flag, then the git work-tree root, then the
// current directory.
func resolveRoot(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}

	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root, nil
		}
	}

	return os.Getwd()
}

func writeJSON(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}
