package query

import (
	"context"
	"os/exec"
	"strings"

	"github.com/Goutam-11/repath/internal/adapter"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// RefreshReport is what one status refresh changed.
type RefreshReport struct {
	FilesIndexed int   `json:"files_indexed"`
	FilesSkipped int   `json:"files_skipped"`
	FilesDeleted int   `json:"files_deleted"`
	DurationMS   int64 `json:"duration_ms"`
}

// StatusReport is the `status --json` contract, shared by the CLI and MCP.
type StatusReport struct {
	Root            string         `json:"root"`
	Indexed         bool           `json:"indexed"`
	SchemaVersion   int            `json:"schema_version"`
	Version         string         `json:"version,omitempty"`
	Files           int            `json:"files"`
	Nodes           int            `json:"nodes"`
	NodesByKind     map[string]int `json:"nodes_by_kind"`
	ParseFailures   int            `json:"parse_failures"`
	Entrypoints     int            `json:"entrypoints"`
	EntrypointRules map[string]int `json:"entrypoint_rules,omitempty"`
	Edges           map[string]int `json:"edges"`
	Ambiguous       int            `json:"ambiguous"`
	AmbiguityRate   float64        `json:"ambiguity_rate"`
	Unresolved      int            `json:"unresolved"`
	UnresolvedRate  float64        `json:"unresolved_rate"`
	// UnresolvedByReason explains the pending count with the store classifier;
	// the buckets always sum to Unresolved. The top lists are bounded and the
	// reason totals are not.
	UnresolvedByReason   map[string]int        `json:"unresolved_by_reason,omitempty"`
	UnresolvedByLang     map[string]int        `json:"unresolved_by_lang,omitempty"`
	UnresolvedTopNames   []store.PendingBucket `json:"unresolved_top_names,omitempty"`
	UnresolvedTopModules []store.PendingBucket `json:"unresolved_top_modules,omitempty"`
	LastIndexed          string                `json:"last_indexed,omitempty"`
	GitHead              string                `json:"git_head,omitempty"`
	CurrentHead          string                `json:"current_head,omitempty"`
	Stale                bool                  `json:"stale"`
	DirtyFiles           int                   `json:"dirty_files"`
	Adapters             []adapter.Stats       `json:"adapters,omitempty"`
	Refresh              RefreshReport         `json:"refresh"`
}

// Status reads the index counters and, unless refresh is false, runs the normal
// incremental pass first so the answer describes the current tree.
func Status(ctx context.Context, st *store.Store, root string, refresh, full bool) (StatusReport, error) {
	var refreshed index.Result
	if refresh {
		result, err := index.Run(ctx, st, root, index.Options{Full: full})
		if err != nil {
			return StatusReport{}, err
		}
		refreshed = result
		// Keep the commit cache warm after an explicit refresh. Cache writes
		// are best-effort and never fail the status answer.
		_ = index.StoreCommit(ctx, root, st)
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	edges, err := st.EdgeCounts(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	unresolvedSummary, err := st.UnresolvedSummary(ctx, 10)
	if err != nil {
		return StatusReport{}, err
	}
	unresolved := unresolvedSummary.Total
	ambiguous, err := st.AmbiguousEdges(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	entrypointRules, err := st.EntrypointCounts(ctx)
	if err != nil {
		return StatusReport{}, err
	}
	entrypoints := 0
	for _, count := range entrypointRules {
		entrypoints += count
	}
	adapters, err := adapter.SavedStats(ctx, st)
	if err != nil {
		return StatusReport{}, err
	}

	totalEdges := 0
	for _, n := range edges {
		totalEdges += n
	}
	ambiguityRate := 0.0
	if totalEdges > 0 {
		ambiguityRate = float64(ambiguous) / float64(totalEdges)
	}
	unresolvedRate := 0.0
	if totalEdges+unresolved > 0 {
		unresolvedRate = float64(unresolved) / float64(totalEdges+unresolved)
	}

	currentHead := gitHead(ctx, root)
	gitHead := meta["git_head"]
	return StatusReport{
		Root:                 root,
		Indexed:              counts.Files > 0,
		SchemaVersion:        store.SchemaVersion,
		Files:                counts.Files,
		Nodes:                counts.Nodes,
		NodesByKind:          counts.ByKind,
		ParseFailures:        counts.ParseFailures,
		Entrypoints:          entrypoints,
		EntrypointRules:      entrypointRules,
		Edges:                edges,
		Ambiguous:            ambiguous,
		AmbiguityRate:        ambiguityRate,
		Unresolved:           unresolved,
		UnresolvedRate:       unresolvedRate,
		UnresolvedByReason:   unresolvedSummary.ByReason,
		UnresolvedByLang:     unresolvedSummary.ByLang,
		UnresolvedTopNames:   unresolvedSummary.TopNames,
		UnresolvedTopModules: unresolvedSummary.TopModules,
		LastIndexed:          meta["indexed_at"],
		GitHead:              gitHead,
		CurrentHead:          currentHead,
		Stale:                gitHead != "" && currentHead != "" && gitHead != currentHead,
		DirtyFiles:           gitDirtyCount(ctx, root),
		Adapters:             adapters,
		Refresh: RefreshReport{
			FilesIndexed: refreshed.Indexed,
			FilesSkipped: refreshed.Unchanged,
			FilesDeleted: refreshed.Deleted,
			DurationMS:   refreshed.Duration.Milliseconds(),
		},
	}, nil
}

func gitHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitDirtyCount returns the number of changed or untracked paths, or 0 when git
// is unavailable (a plain directory is never reported as dirty).
func gitDirtyCount(ctx context.Context, root string) int {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}
