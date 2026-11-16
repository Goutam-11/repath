package resolve

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// workspacePackage is one local package declared by the root package.json
// `workspaces` field. dir is repo-relative; entry is the file id of the source
// module the package exposes, or 0 when no entry is indexed.
type workspacePackage struct {
	dir   string
	entry int64
}

// maxWorkspaceDirs bounds manifest discovery. A workspaces glob is a local
// manifest read, never a tree walk, but a pathological pattern must still not
// turn one index run into an unbounded filesystem scan.
const maxWorkspaceDirs = 10000

// workspaceTarget maps a bare specifier to a local workspace package's source
// entry. `@scope/pkg` resolves to the entry module, `@scope/pkg/sub` to the
// subpath inside the package directory. Zero means the specifier is a
// dependency, so the caller classifies it external.
func (ix *Index) workspaceTarget(spec string) (int64, bool) {
	ix.loadWorkspaces()
	if len(ix.workspaces) == 0 {
		return 0, false
	}

	// Longest package name wins, which is what makes `@scope/pkg` and
	// `@scope/pkg-extra` distinct. Names are unique in the map, so the match
	// cannot be ambiguous.
	var bestName string
	var best workspacePackage
	for name, pkg := range ix.workspaces {
		if spec != name && !strings.HasPrefix(spec, name+"/") {
			continue
		}
		if len(name) > len(bestName) {
			bestName, best = name, pkg
		}
	}
	if bestName == "" {
		return 0, false
	}
	if spec == bestName {
		if best.entry == 0 {
			return 0, false
		}
		return best.entry, true
	}
	id := ix.resolveFile(path.Join(best.dir, spec[len(bestName)+1:]))
	if id == 0 {
		return 0, false
	}
	return id, true
}

// loadWorkspaces reads the root manifest once, on the first bare specifier a
// TypeScript file resolves. A monorepo without workspaces pays one failed read;
// a run whose imports are all relative pays nothing.
func (ix *Index) loadWorkspaces() {
	if ix.workspacesLoaded {
		return
	}
	ix.workspacesLoaded = true
	ix.workspaces = ix.readWorkspacePackages()
}

// readWorkspacePackages expands package.json `workspaces` into name -> package.
// Only local manifests are read: there is no registry lookup, no lockfile, and
// no dependency on a package manager. Directories are sorted and a duplicate
// package name keeps the first one, so the mapping is deterministic.
func (ix *Index) readWorkspacePackages() map[string]workspacePackage {
	root := ix.root
	if root == "" {
		return nil
	}
	patterns := workspacePatterns(filepath.Join(root, "package.json"))
	if len(patterns) == 0 {
		return nil
	}

	dirs := make(map[string]bool, len(patterns))
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "!") {
			// Negated workspace patterns are rare; ignoring the exclusion can
			// only add a local package, never a wrong external one.
			continue
		}
		if len(dirs) >= maxWorkspaceDirs {
			break
		}
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			continue
		}
		for _, match := range matches {
			if len(dirs) >= maxWorkspaceDirs {
				break
			}
			info, err := os.Stat(match)
			if err != nil || !info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(root, match)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			dirs[filepath.ToSlash(rel)] = true
		}
	}
	if len(dirs) == 0 {
		return nil
	}

	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Strings(ordered)

	packages := make(map[string]workspacePackage, len(ordered))
	for _, dir := range ordered {
		manifest := readPackageManifest(filepath.Join(root, filepath.FromSlash(dir), "package.json"))
		if manifest.Name == "" {
			continue
		}
		if _, seen := packages[manifest.Name]; seen {
			continue
		}
		packages[manifest.Name] = workspacePackage{dir: dir, entry: ix.workspaceEntry(dir, manifest)}
	}
	return packages
}

// workspaceEntry picks the package's source module. The order follows what the
// package tells a resolver, then the common TypeScript convention: an entry
// that is not in the index (build output under dist/) falls through to the
// source tree instead of resolving to a file repath cannot see.
func (ix *Index) workspaceEntry(dir string, manifest packageManifest) int64 {
	candidates := []string{}
	for _, entry := range []string{
		exportsEntry(manifest.Exports),
		manifest.Source,
		manifest.Types,
		manifest.Typings,
		manifest.Module,
		manifest.Main,
	} {
		if entry != "" {
			candidates = append(candidates, entry)
		}
	}
	candidates = append(candidates, "src/index", "index", "src/main")

	for _, candidate := range candidates {
		clean := path.Clean(strings.TrimPrefix(strings.TrimSpace(candidate), "./"))
		if clean == "." || strings.HasPrefix(clean, "../") {
			continue
		}
		if id := ix.resolveFile(path.Join(dir, clean)); id != 0 {
			return id
		}
	}
	return 0
}

type packageManifest struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Types   string          `json:"types"`
	Typings string          `json:"typings"`
	Module  string          `json:"module"`
	Main    string          `json:"main"`
	Exports json.RawMessage `json:"exports"`
}

func readPackageManifest(file string) packageManifest {
	var manifest packageManifest
	data, err := os.ReadFile(file)
	if err != nil {
		return manifest
	}
	// package.json is JSON with no comments; a manifest that does not parse is
	// skipped rather than guessed at.
	_ = json.Unmarshal(data, &manifest)
	return manifest
}

// workspacePatterns returns the root manifest's workspaces list. Both accepted
// shapes are supported: an array of patterns, and the object form whose
// `packages` key holds them.
func workspacePatterns(manifestPath string) []string {
	var manifest struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Workspaces) == 0 {
		return nil
	}

	var list []string
	if err := json.Unmarshal(manifest.Workspaces, &list); err == nil {
		return cleanPatterns(list)
	}
	var object struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(manifest.Workspaces, &object); err == nil {
		return cleanPatterns(object.Packages)
	}
	return nil
}

func cleanPatterns(list []string) []string {
	out := make([]string, 0, len(list))
	for _, pattern := range list {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		out = append(out, pattern)
	}
	return out
}

// exportsEntry reduces a package.json `exports` value to a module path: a plain
// string, the "." condition, or the first condition that names a file. It never
// invents a path; an unrecognised shape returns "".
func exportsEntry(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, item := range list {
			if entry := exportsEntry(item); entry != "" {
				return entry
			}
		}
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	if dot, ok := object["."]; ok {
		if entry := exportsEntry(dot); entry != "" {
			return entry
		}
	}
	if _, hasDot := object["."]; hasDot {
		return ""
	}
	for _, condition := range []string{"import", "types", "require", "default", "node"} {
		if value, ok := object[condition]; ok {
			if entry := exportsEntry(value); entry != "" {
				return entry
			}
		}
	}
	return ""
}
