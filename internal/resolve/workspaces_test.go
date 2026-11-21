package resolve_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Goutam-11/repath/internal/resolve"
)

// TestWorkspacePatternsPnpm pins the bounded pnpm-workspace.yaml reader (#79).
// It parses only the top-level `packages:` block: the `hoistPattern`, `catalog`,
// and `overrides` lists are ignored, a quoted entry drops its trailing comment,
// an unquoted entry drops a `#` comment but keeps `a#b`, and a `!` negation
// survives for the expansion step.
func TestWorkspacePatternsPnpm(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"name":"mono","workspaces":["ignored/*"]}`)
	writeFile(t, root, "pnpm-workspace.yaml", `nodeLinker: hoisted
hoistPattern:
  - '*'
catalog:
  react: 19.0.0
overrides:
  '**/left-pad': 1.3.0

packages:
  - 'packages/*'      # quoted, trailing comment
  - apps/*            # unquoted, trailing comment
  - 'scoped/a#b'
  - "!packages/legacy"
  - tools/*

lockfile: true
`)

	got := resolve.WorkspacePatterns(root)
	want := []string{"packages/*", "apps/*", "scoped/a#b", "!packages/legacy", "tools/*"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("patterns = %q, want %q", got, want)
	}
}

// TestWorkspacePatternsPnpmFlowList covers the one-line list form and the
// precedence when a pnpm manifest has no usable list.
func TestWorkspacePatternsPnpmFlowList(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"name":"mono","workspaces":["from-json/*"]}`)
	writeFile(t, root, "pnpm-workspace.yaml", "packages: ['packages/*', \"apps/*\"]\n")
	if got, want := resolve.WorkspacePatterns(root), []string{"packages/*", "apps/*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("flow list = %q, want %q", got, want)
	}

	// An empty pnpm packages list falls back to package.json instead of
	// silencing a declaration.
	writeFile(t, root, "pnpm-workspace.yaml", "packages: []\n")
	if got, want := resolve.WorkspacePatterns(root), []string{"from-json/*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty pnpm list = %q, want package.json fallback %q", got, want)
	}

	// Object-form package.json workspaces still work with no pnpm manifest.
	writeFile(t, root, "pnpm-workspace.yaml", "nodeLinker: hoisted\n")
	writeFile(t, root, "package.json", `{"name":"mono","workspaces":{"packages":["obj/*"],"nohoist":[]}}`)
	if got, want := resolve.WorkspacePatterns(root), []string{"obj/*"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("object form = %q, want %q", got, want)
	}
}

// TestExpandWorkspaceDirs pins pattern expansion: `*`, recursive `**`, and
// exclusion. The recursive form does not match its own base directory and never
// descends into node_modules or hidden state.
func TestExpandWorkspaceDirs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{
		"packages/alpha", "packages/beta", "packages/skip", "packages/group/inner",
		"apps/web", "tools/tool", "node_modules/pkg", ".cache/pkg",
	} {
		writeFile(t, root, dir+"/package.json", `{"name":"x"}`)
	}

	got := resolve.ExpandWorkspaceDirs(root, []string{
		"packages/*", "packages/**", "apps/*", "!packages/skip", "!packages/group",
	})
	want := []string{
		"apps/web",
		"packages/alpha",
		"packages/beta",
		"packages/group/inner",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dirs = %q, want %q", got, want)
	}

	// `**` alone walks the visible tree only, excludes its own base, and keeps
	// node_modules and dot directories out of the workspace set.
	got = resolve.ExpandWorkspaceDirs(root, []string{"**"})
	want = []string{
		"apps", "apps/web",
		"packages", "packages/alpha", "packages/beta", "packages/group",
		"packages/group/inner", "packages/skip",
		"tools", "tools/tool",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("** dirs = %q, want %q", got, want)
	}
}

// TestTypeScriptPnpmWorkspaceResolves is the end-to-end #79 case: a repo whose
// only workspace manifest is pnpm-workspace.yaml resolves `@demo/*` imports to
// the package source entries, while a negated package and a directory no
// pattern matches stay external instead of being guessed.
func TestTypeScriptPnpmWorkspaceResolves(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json", `{"name":"pnpm-mono","private":true}`)
	writeFile(t, root, "pnpm-workspace.yaml", `nodeLinker: hoisted
hoistPattern:
  - '*'

packages:
  - 'packages/*'
  - apps/*
  - "!packages/legacy"
`)
	writeFile(t, root, "packages/widget/package.json", `{"name":"@demo/widget","main":"src/index.ts"}`)
	writeFile(t, root, "packages/widget/src/index.ts", "export * from './impl';\n")
	writeFile(t, root, "packages/widget/src/impl.ts", "export function gadget() { return 1; }\n")
	writeFile(t, root, "packages/gizmo/package.json", `{"name":"@demo/gizmo","exports":"./index.ts"}`)
	writeFile(t, root, "packages/gizmo/index.ts", "export function spin() { return 2; }\n")
	writeFile(t, root, "packages/legacy/package.json", `{"name":"@demo/legacy","main":"index.ts"}`)
	writeFile(t, root, "packages/legacy/index.ts", "export function old() { return 3; }\n")
	writeFile(t, root, "apps/web/package.json", `{"name":"@demo/web","main":"src/index.ts"}`)
	writeFile(t, root, "apps/web/src/index.ts", "export function render(): number { return 4; }\n")
	writeFile(t, root, "tools/unlisted/package.json", `{"name":"@demo/unlisted","main":"index.ts"}`)
	writeFile(t, root, "tools/unlisted/index.ts", "export function stray(): number { return 5; }\n")
	writeFile(t, root, "use.ts", `import { gadget } from "@demo/widget";
import { spin } from "@demo/gizmo";
import { render } from "@demo/web";
import { old } from "@demo/legacy";
import { stray } from "@demo/unlisted";

export function run(): number {
  return gadget() + spin() + render() + old() + stray();
}
`)

	st := indexRepo(t, root)
	got := tsEdges(t, st)
	want := map[string]string{
		"run|gadget": "ts.import.workspace.star",
		"run|spin":   "ts.import.workspace",
		"run|render": "ts.import.workspace",
	}
	for key, rule := range want {
		if got[key] != rule {
			t.Fatalf("edge %s rule = %q, want %q (all: %v)", key, got[key], rule, got)
		}
	}
	for _, key := range []string{"run|old", "run|stray"} {
		if rule, ok := got[key]; ok {
			t.Fatalf("edge %s resolved as %q; a negated or unmatched package must stay external", key, rule)
		}
	}

	// External is neither a low-confidence guess nor pending: no edge at any
	// confidence may point at the negated/unmatched package symbols.
	all, err := st.Edges(context.Background(), 0)
	if err != nil {
		t.Fatalf("read all edges: %v", err)
	}
	for _, e := range all {
		if e.DstQName == "old" || e.DstQName == "stray" {
			t.Fatalf("edge %s -> %s:%s (%s %.2f) resolved an external package", e.SrcQName, e.DstPath, e.DstQName, e.Rule, e.Confidence)
		}
	}
	pending, err := st.Pending(context.Background())
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d rows, want 0 (external bindings are not unresolved): %+v", len(pending), pending)
	}
}
