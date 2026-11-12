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
