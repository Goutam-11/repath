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

// maxWorkspaceDirs bounds manifest discovery. A workspaces glob is usually a
// manifest read (one stat per pattern), but a `**` pattern needs a tree walk,
// so a pathological pattern must still not turn one index run into an unbounded
// filesystem scan.
const maxWorkspaceDirs = 10000

// maxWorkspacePatterns bounds the parsed pattern list itself, and
// maxWorkspaceWalk bounds the directory visits one `**` expansion may make.
// `**` is the only pattern shape that needs a tree walk; the visited cap keeps a
// pattern such as `**` proportional to the tree instead of unbounded.
const maxWorkspacePatterns = 20000
const maxWorkspaceWalk = 200000

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

// readWorkspacePackages expands the repo's workspace manifests into
// name -> package. Only local manifests are read: there is no registry lookup,
// no lockfile, and no dependency on a package manager. Directories are sorted
// and a duplicate package name keeps the first one, so the mapping is
// deterministic.
func (ix *Index) readWorkspacePackages() map[string]workspacePackage {
	root := ix.root
	if root == "" {
		return nil
	}
	ordered := ExpandWorkspaceDirs(root, WorkspacePatterns(root))
	if len(ordered) == 0 {
		return nil
	}

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

// WorkspacePatterns returns the repo's workspace package patterns. A root
// pnpm-workspace.yaml with a `packages:` list wins; otherwise the root
// package.json `workspaces` field is used (array form, or the object form whose
// `packages` key holds the list). That precedence mirrors pnpm itself: the
// workspace manifest is the manifest of record when one exists. A
// pnpm-workspace.yaml without a usable `packages:` list falls back instead of
// silencing a package.json declaration.
func WorkspacePatterns(root string) []string {
	if patterns := pnpmWorkspacePatterns(filepath.Join(root, "pnpm-workspace.yaml")); len(patterns) > 0 {
		return patterns
	}
	return workspacePatterns(filepath.Join(root, "package.json"))
}

// ExpandWorkspaceDirs expands workspace patterns into repo-relative package
// directories, sorted and deduplicated. Patterns support Go's `*`/`?`/`[...]`
// syntax and a recursive `**` segment; a leading `!` excludes the directories
// its pattern matches. Negation is applied after every include, so an excluded
// package stays external rather than being resolved by accident. Directories
// are never walked for a pattern without `**`, so a manifest-driven repo pays
// one stat per pattern, exactly as before.
func ExpandWorkspaceDirs(root string, patterns []string) []string {
	if root == "" || len(patterns) == 0 {
		return nil
	}
	include := make(map[string]bool, len(patterns))
	var exclude map[string]bool
	for i, pattern := range patterns {
		if i >= maxWorkspacePatterns {
			break
		}
		pattern = strings.TrimSpace(pattern)
		negated := strings.HasPrefix(pattern, "!")
		if negated {
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "!"))
		}
		if pattern == "" {
			continue
		}
		matches := expandWorkspacePattern(root, pattern)
		if negated {
			if exclude == nil {
				exclude = make(map[string]bool, len(matches))
			}
			for _, dir := range matches {
				exclude[dir] = true
			}
			continue
		}
		for _, dir := range matches {
			if len(include) >= maxWorkspaceDirs {
				break
			}
			include[dir] = true
		}
	}
	if len(include) == 0 {
		return nil
	}
	dirs := make([]string, 0, len(include))
	for dir := range include {
		if exclude[dir] {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// expandWorkspacePattern turns one pattern into directories. The path is
// cleaned and must stay local: a pattern that climbs out of the repo is
// ignored, never followed. Only a `**` segment needs a walk.
func expandWorkspacePattern(root, pattern string) []string {
	pattern = strings.TrimSpace(pattern)
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return nil
	}
	pattern = path.Clean(strings.TrimPrefix(pattern, "./"))
	if pattern == "." || !filepath.IsLocal(filepath.FromSlash(pattern)) {
		return nil
	}
	if strings.Contains(pattern, "**") {
		return expandDoubleStar(root, pattern)
	}
	matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
	if err != nil {
		return nil
	}
	dirs := make([]string, 0, len(matches))
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || !info.IsDir() {
			continue
		}
		rel, err := filepath.Rel(root, match)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		dirs = append(dirs, filepath.ToSlash(rel))
	}
	sort.Strings(dirs)
	return dirs
}

// expandDoubleStar walks the pattern's static prefix and matches the remaining
// segments, where a bare `**` segment consumes zero or more directories. The
// directory itself is not a match for `dir/**` (only its descendants are), and
// hidden directories, node_modules, and repath state are never descended into.
func expandDoubleStar(root, pattern string) []string {
	segments := strings.Split(pattern, "/")
	prefix := 0
	for prefix < len(segments) && !strings.ContainsAny(segments[prefix], "*?[") {
		prefix++
	}
	base := filepath.Join(root, filepath.FromSlash(strings.Join(segments[:prefix], "/")))
	info, err := os.Stat(base)
	if err != nil || !info.IsDir() {
		return nil
	}
	baseRel, err := filepath.Rel(root, base)
	if err != nil {
		return nil
	}
	baseRel = filepath.ToSlash(baseRel)

	var dirs []string
	visited := 0
	_ = filepath.WalkDir(base, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != base && skipWorkspaceDir(d.Name()) {
			return filepath.SkipDir
		}
		visited++
		if visited > maxWorkspaceWalk || len(dirs) >= maxWorkspaceDirs {
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == baseRel {
			return nil
		}
		var have []string
		if rel != "." {
			have = strings.Split(rel, "/")
		}
		if matchWorkspaceSegments(segments, have) {
			dirs = append(dirs, rel)
		}
		return nil
	})
	sort.Strings(dirs)
	return dirs
}

// matchWorkspaceSegments matches path segments against pattern segments. Only a
// whole `**` segment is recursive; a `*` inside a segment keeps Go's path.Match
// meaning. The comparison is deterministic and never backtracks exponentially
// because each `**` consumes a prefix and the rest is matched once.
func matchWorkspaceSegments(pattern, have []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(have); i++ {
				if matchWorkspaceSegments(pattern[1:], have[i:]) {
					return true
				}
			}
			return false
		}
		if len(have) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], have[0])
		if err != nil || !ok {
			return false
		}
		pattern, have = pattern[1:], have[1:]
	}
	return len(have) == 0
}

// skipWorkspaceDir keeps a `**` walk out of dependency trees and repath state.
// Hidden directories are skipped too: pnpm packages are declared in visible
// directories, and the node_modules/pnpm store live under a dot directory.
func skipWorkspaceDir(name string) bool {
	switch name {
	case "node_modules", ".git", ".repath", ".cache", ".pnpm-store":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// pnpmWorkspacePatterns reads the top-level `packages:` list from a
// pnpm-workspace.yaml. It parses the documented subset without a YAML
// dependency: block list items (`- 'glob'` or unquoted) under the top-level
// `packages:` key, a trailing `# comment` stripped, and a one-line flow list.
// Every other key is ignored, which is what keeps the `hoistPattern`,
// `catalog`, and `overrides` lists out of the workspace set.
func pnpmWorkspacePatterns(file string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	return parsePnpmPackages(data)
}

func parsePnpmPackages(data []byte) []string {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	var out []string
	inPackages := false
	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(line)
		if !inPackages {
			indent, key, value, ok := yamlKey(line)
			if !ok || indent != 0 || key != "packages" {
				continue
			}
			if strings.HasPrefix(value, "[") {
				return parsePnpmFlowList(value)
			}
			inPackages = true
			continue
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			// A new top-level key (or document marker) ends the block.
			break
		}
		if !strings.HasPrefix(trimmed, "-") {
			// A nested mapping under an item is not a pattern.
			continue
		}
		item := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if item == "" {
			continue
		}
		item = strings.TrimSpace(yamlScalar(item))
		if item == "" || item == "{}" || item == "[]" {
			continue
		}
		out = append(out, item)
		if len(out) >= maxWorkspacePatterns {
			break
		}
	}
	return out
}

// yamlKey splits one block-style YAML line into indent, key, and value. It
// finds the first colon outside quotes and ignores comments and document
// markers. Only scalars are understood; that is all a packages list needs.
func yamlKey(line string) (indent int, key, value string, ok bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
		return 0, "", "", false
	}
	indent = len(line) - len(strings.TrimLeft(line, " \t"))
	rest := line[indent:]
	inSingle, inDouble := false, false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ':':
			if !inSingle && !inDouble {
				return indent, strings.TrimSpace(rest[:i]), strings.TrimSpace(rest[i+1:]), true
			}
		}
	}
	return 0, "", "", false
}

// yamlScalar reads a single scalar: a quoted string (trailing comment dropped
// at the closing quote) or an unquoted value (a `#` only starts a comment when
// whitespace precedes it, so `a#b` survives).
func yamlScalar(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	switch s[0] {
	case '\'':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] != '\'' {
				b.WriteByte(s[i])
				continue
			}
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			return b.String()
		}
		return b.String()
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if s[i] == '"' {
				return b.String()
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimSpace(s[:i])
		}
	}
	return s
}

// parsePnpmFlowList reads a one-line `packages: [...]` value.
func parsePnpmFlowList(s string) []string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") {
		return nil
	}
	inner := s[1:]
	if close := strings.LastIndex(inner, "]"); close >= 0 {
		inner = inner[:close]
	}
	var parts []string
	var cur strings.Builder
	inSingle, inDouble := false, false
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ',':
			if !inSingle && !inDouble {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(inner[i])
	}
	if strings.TrimSpace(cur.String()) != "" {
		parts = append(parts, cur.String())
	}
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(yamlScalar(part)); item != "" {
			out = append(out, item)
		}
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
