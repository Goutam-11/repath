// Package index builds the graph: discovery, hashing, parsing, and the
// incremental write pipeline.
package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/lang"
)

// maxFileSize skips generated bundles and vendored blobs.
const maxFileSize = 1 << 20 // 1 MiB

// Source is one candidate file, relative to the repo root.
type Source struct {
	Path  string // repo-relative, slash separated
	Abs   string
	Size  int64
	MTime int64
	// Unchanged marks a file git reports clean since the last index. It is
	// skipped without a stat or a hash read.
	Unchanged bool
}

var skippedDirs = map[string]bool{
	".git": true, ".repath": true, ".cache": true,
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true,
	"target": true, "dist": true, "build": true, "vendor": true,
	".next": true, ".turbo": true, ".yarn": true,
}

// Discover lists the files repath can parse. It prefers git so .gitignore rules
// apply for free, and falls back to a directory walk outside a work tree.
func Discover(ctx context.Context, root string) ([]Source, error) {
	return discoverSources(ctx, root, nil)
}

// discoverSources lists source files and stats them, except for paths that
// unchanged reports as clean since the last index. Those are recorded without
// touching the filesystem: the incremental run is then proportional to the
// changed files rather than the whole tree.
func discoverSources(ctx context.Context, root string, unchanged func(rel string) bool) ([]Source, error) {
	paths, _, err := listPathSets(ctx, root, true)
	if err != nil {
		return nil, err
	}
	return sourcesFromPaths(root, paths, unchanged), nil
}

func sourcesFromPaths(root string, paths []string, unchanged func(rel string) bool) []Source {
	seen := make(map[string]bool, len(paths))
	out := make([]Source, 0, len(paths))
	for _, rel := range paths {
		if !filepath.IsLocal(rel) {
			continue
		}
		rel = filepath.ToSlash(rel)
		if seen[rel] || strings.HasPrefix(rel, ".repath/") {
			continue
		}
		if _, _, ok := lang.For(rel); !ok {
			continue
		}

		abs := filepath.Join(root, filepath.FromSlash(rel))
		if unchanged != nil && unchanged(rel) {
			seen[rel] = true
			out = append(out, Source{Path: rel, Abs: abs, Unchanged: true})
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() > maxFileSize {
			continue
		}

		seen[rel] = true
		out = append(out, Source{Path: rel, Abs: abs, Size: info.Size(), MTime: info.ModTime().Unix()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// gitTrackedChanges asks git which tracked paths differ from the index and
// HEAD (staged or unstaged, including deletes and renames). Untracked paths are
// handled separately because git keeps no stat cache for them. The bool is
// false when git cannot answer, in which case the caller falls back to hashing
// every file.
func gitTrackedChanges(ctx context.Context, root string) (map[string]bool, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "--no-optional-locks",
		"status", "--porcelain=v1", "-z", "--untracked-files=no").Output()
	if err != nil {
		return nil, false
	}
	return parseStatusPaths(out), true
}

func parseStatusPaths(out []byte) map[string]bool {
	changed := make(map[string]bool)
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if len(field) < 4 {
			continue
		}
		status := field[:2]
		changed[filepath.ToSlash(field[3:])] = true
		// In -z format a rename/copy carries the original path as the next
		// NUL-terminated token. Marking it changed is safe: if it is gone the
		// delete pass removes it, and if it is still there it is re-hashed.
		if strings.ContainsAny(status, "RC") && i+1 < len(fields) {
			i++
			if fields[i] != "" {
				changed[filepath.ToSlash(fields[i])] = true
			}
		}
	}
	return changed
}

// listPathSets lists tracked and untracked paths from one git process. With
// -t, git tags every entry: "S" is skip-worktree (a path a sparse checkout
// leaves out) and "?" is untracked. includeSkip keeps the skip-worktree paths
// for a full discovery, which stats them and drops the absent ones; the
// incremental path omits them outright.
func listPathSets(ctx context.Context, root string, includeSkip bool) ([]string, map[string]bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "ls-files", "-t", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		paths, walkErr := walkPaths(root)
		return paths, nil, walkErr
	}

	var paths []string
	untracked := make(map[string]bool)
	for _, field := range strings.Split(string(out), "\x00") {
		if len(field) < 3 {
			continue
		}
		tag := field[0]
		if tag == 'S' && !includeSkip {
			continue
		}
		path := field[2:]
		if tag == '?' {
			untracked[path] = true
		}
		paths = append(paths, path)
	}
	return paths, untracked, nil
}

func walkPaths(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// An unreadable entry must not abort the whole index.
			return nil
		}
		if d.IsDir() {
			if path != root && skippedDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
