package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/store"
)

// DetectModules groups the repo's files into packages. Manifests win: a
// package.json or Cargo.toml gives exact package boundaries. Without one the
// fallback is directories, which is what "module" means in a plain repo.
func DetectModules(root string, sources []Source) []store.Module {
	if modules := manifestModules(root); len(modules) > 0 {
		return modules
	}
	modules := directoryModules(sources)
	if len(modules) == 0 {
		modules = []store.Module{{Name: "(root)", Kind: "dir", Root: "."}}
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Root < modules[j].Root })
	return modules
}

// ModuleRootFor picks the deepest module whose root contains path.
func ModuleRootFor(modules []store.Module, path string) string {
	best := ""
	for _, m := range modules {
		if m.Root == "." || path == m.Root || strings.HasPrefix(path, m.Root+"/") {
			if len(m.Root) > len(best) {
				best = m.Root
			}
		}
	}
	return best
}

type packageJSON struct {
	Name       string          `json:"name"`
	Workspaces json.RawMessage `json:"workspaces"`
}

func manifestModules(root string) []store.Module {
	if modules := packageJSONModules(root); len(modules) > 0 {
		return modules
	}
	return cargoModules(root)
}

func packageJSONModules(root string) []store.Module {
	data, err := os.ReadFile(filepath.Join(root, "package.json"))
	if err != nil {
		return nil
	}

	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}

	patterns := workspacePatterns(pkg.Workspaces)
	if len(patterns) == 0 {
		return nil
	}

	modules := []store.Module{{Name: nameOr(pkg.Name, "(root)"), Kind: "workspace", Root: "."}}

	seen := map[string]bool{".": true}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			continue
		}
		for _, dir := range matches {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(root, dir)
			if err != nil || rel == "." {
				continue
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] {
				continue
			}
			seen[rel] = true

			name := rel
			if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
				var sub packageJSON
				if json.Unmarshal(data, &sub) == nil && sub.Name != "" {
					name = sub.Name
				}
			}
			modules = append(modules, store.Module{Name: name, Kind: "package", Root: rel})
		}
	}

	sort.Slice(modules, func(i, j int) bool { return modules[i].Root < modules[j].Root })
	return modules
}

// cargoModules reads Cargo.toml. A workspace contributes its root plus each
// member; a single package is one module. Parsing is deliberately minimal: the
// fields needed are [package].name and [workspace].members.
func cargoModules(root string) []store.Module {
	data, err := os.ReadFile(filepath.Join(root, "Cargo.toml"))
	if err != nil {
		return nil
	}

	var modules []store.Module
	name := tomlString(data, "package", "name")
	members := tomlArray(data, "workspace", "members")

	if len(members) > 0 {
		modules = append(modules, store.Module{
			Name: nameOr(name, "(workspace)"), Kind: "workspace", Root: ".",
		})
	}

	seen := map[string]bool{".": true}
	for _, pattern := range members {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			continue
		}
		for _, dir := range matches {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				continue
			}
			rel, err := filepath.Rel(root, dir)
			if err != nil || rel == "." || !filepath.IsLocal(rel) {
				continue
			}
			rel = filepath.ToSlash(rel)
			if seen[rel] {
				continue
			}
			seen[rel] = true

			memberName := rel
			if memberData, err := os.ReadFile(filepath.Join(dir, "Cargo.toml")); err == nil {
				if parsed := tomlString(memberData, "package", "name"); parsed != "" {
					memberName = parsed
				}
			}
			modules = append(modules, store.Module{Name: memberName, Kind: "package", Root: rel})
		}
	}

	if len(modules) == 0 && name != "" {
		modules = append(modules, store.Module{Name: name, Kind: "package", Root: "."})
	}
	if len(modules) == 0 {
		return nil
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].Root < modules[j].Root })
	return modules
}

// tomlString returns key from one TOML section, ignoring comments.
func tomlString(data []byte, section, key string) string {
	lines := strings.Split(string(data), "\n")
	inSection := false
	for _, line := range lines {
		line = strings.TrimSpace(stripTOMLComment(line))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			inSection = strings.TrimSpace(line[1:len(line)-1]) == section
			continue
		}
		if !inSection {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// tomlArray returns a string array such as workspace.members, including arrays
// that span multiple lines.
func tomlArray(data []byte, section, key string) []string {
	lines := strings.Split(stripTOMLComments(string(data)), "\n")
	inSection := false
	collecting := false
	var body strings.Builder

	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if !collecting {
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				inSection = strings.TrimSpace(line[1:len(line)-1]) == section
				continue
			}
			if !inSection {
				continue
			}
			name, value, found := strings.Cut(line, "=")
			if !found || strings.TrimSpace(name) != key {
				continue
			}
			value = strings.TrimSpace(value)
			open := strings.Index(value, "[")
			if open < 0 {
				return nil
			}
			rest := value[open+1:]
			if close := strings.Index(rest, "]"); close >= 0 {
				body.WriteString(rest[:close])
				break
			}
			body.WriteString(rest)
			body.WriteString("\n")
			collecting = true
			continue
		}
		if close := strings.Index(line, "]"); close >= 0 {
			body.WriteString(line[:close])
			break
		}
		body.WriteString(line)
		body.WriteString("\n")
	}

	var out []string
	for _, part := range strings.Split(body.String(), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, strings.Trim(part, `"'`))
	}
	return out
}

func stripTOMLComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inString = !inString
		case '#':
			if !inString {
				return line[:i]
			}
		}
	}
	return line
}

func stripTOMLComments(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = stripTOMLComment(line)
	}
	return strings.Join(lines, "\n")
}

func workspacePatterns(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}

	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list
	}

	var object struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return object.Packages
	}
	return nil
}

// directoryModules keeps one module per first-level unit: src/<x>, or the
// top-level directory, or the root for loose files.
func directoryModules(sources []Source) []store.Module {
	seen := map[string]bool{}
	var modules []store.Module

	add := func(root string) {
		if seen[root] {
			return
		}
		seen[root] = true
		name := root
		if root == "." {
			name = "(root)"
		}
		modules = append(modules, store.Module{Name: name, Kind: "dir", Root: root})
	}

	for _, src := range sources {
		parts := strings.Split(src.Path, "/")
		switch {
		case len(parts) == 1:
			add(".")
		case parts[0] == "src" && len(parts) > 2:
			add("src/" + parts[1])
		case parts[0] == "src":
			add("src")
		default:
			add(parts[0])
		}
	}

	sort.Slice(modules, func(i, j int) bool { return modules[i].Root < modules[j].Root })
	return modules
}

func nameOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
