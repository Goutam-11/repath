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
	paths, err := listPaths(ctx, root)
	if err != nil {
		return nil, err
	}

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
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() > maxFileSize {
			continue
		}

		seen[rel] = true
		out = append(out, Source{Path: rel, Abs: abs, Size: info.Size(), MTime: info.ModTime().Unix()})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// listPaths uses git when root is inside a work tree, so tracked and new-but-not
// ignored files are both indexed.
func listPaths(ctx context.Context, root string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root,
		"ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err == nil {
		paths := make([]string, 0, 64)
		for _, p := range strings.Split(string(out), "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return paths, nil
		}
	}

	return walkPaths(root)
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
