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
// package.json with workspaces gives exact package boundaries. Without one the
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
