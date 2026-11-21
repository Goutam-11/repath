package index

import "testing"

func TestDetectCargoPackageModule(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", `[package]
name = "solo"
version = "0.1.0"
`)
	write(t, dir, "src/lib.rs", "pub fn run() {}\n")

	modules := DetectModules(dir, nil)
	if len(modules) != 1 {
		t.Fatalf("modules = %+v, want one package", modules)
	}
	if modules[0].Name != "solo" || modules[0].Kind != "package" || modules[0].Root != "." {
		t.Fatalf("module = %+v, want solo/package/.", modules[0])
	}
}

func TestDetectCargoWorkspaceModules(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "Cargo.toml", `[workspace]
members = [
    "crates/alpha", # first
    "crates/beta",
]
resolver = "2"
`)
	write(t, dir, "crates/alpha/Cargo.toml", `[package]
name = "alpha-core"
`)
	write(t, dir, "crates/alpha/src/lib.rs", "pub fn a() {}\n")
	write(t, dir, "crates/beta/Cargo.toml", `[package]
name = "beta-core"
`)
	write(t, dir, "crates/beta/src/main.rs", "fn main() {}\n")

	modules := DetectModules(dir, nil)
	if len(modules) != 3 {
		t.Fatalf("modules = %+v, want workspace + two members", modules)
	}

	got := map[string]string{}
	for _, m := range modules {
		got[m.Root] = m.Name
	}
	if got["."] != "(workspace)" {
		t.Errorf("workspace name = %q", got["."])
	}
	if got["crates/alpha"] != "alpha-core" || got["crates/beta"] != "beta-core" {
		t.Fatalf("member names = %+v", got)
	}
}

// TestDetectPnpmWorkspaceModules pins #79 module boundaries: pnpm-workspace.yaml
// is a manifest source alongside package.json, a negation removes a package, and
// a `**` match without a package.json (a nested source directory) is not a
// module.
func TestDetectPnpmWorkspaceModules(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"name":"pnpm-mono","private":true}`)
	write(t, dir, "pnpm-workspace.yaml", `hoistPattern:
  - '*'
packages:
  - 'packages/*'
  - 'packages/**'
  - apps/*
  - '!packages/skip'
`)
	write(t, dir, "packages/alpha/package.json", `{"name":"@demo/alpha"}`)
	write(t, dir, "packages/alpha/index.ts", "export const a = 1;\n")
	write(t, dir, "packages/widget/package.json", `{"name":"@demo/widget"}`)
	write(t, dir, "packages/widget/src/index.ts", "export const w = 2;\n")
	write(t, dir, "packages/skip/package.json", `{"name":"@demo/skip"}`)
	write(t, dir, "apps/web/package.json", `{"name":"@demo/web"}`)
	write(t, dir, "tools/unlisted/package.json", `{"name":"@demo/unlisted"}`)

	modules := DetectModules(dir, nil)
	got := map[string]string{}
	for _, m := range modules {
		got[m.Root] = m.Name
	}
	if got["."] != "pnpm-mono" || got["packages/alpha"] != "@demo/alpha" || got["packages/widget"] != "@demo/widget" || got["apps/web"] != "@demo/web" {
		t.Fatalf("modules = %+v", got)
	}
	for _, absent := range []string{"packages/skip", "packages/widget/src", "tools/unlisted", "packages"} {
		if name, ok := got[absent]; ok {
			t.Fatalf("module %q = %q, want no module (negated, manifest-less, or unmatched)", absent, name)
		}
	}
}
