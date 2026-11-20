package agentskill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformsAreStableAndDistinct(t *testing.T) {
	all := Platforms()
	if len(all) != 5 {
		t.Fatalf("platforms = %d, want 5", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		if !strings.HasSuffix(p.Path, "/repath/SKILL.md") {
			t.Errorf("%s path = %q, want a repath SKILL.md", p.Name, p.Path)
		}
		if seen[p.Path] {
			t.Errorf("duplicate path %q", p.Path)
		}
		seen[p.Path] = true
		if _, ok := Lookup(p.Name); !ok {
			t.Errorf("Lookup(%q) failed", p.Name)
		}
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup accepted an unknown platform")
	}
	if got := SupportedList(); got != "claude, codex, cursor, gemini, vscode" {
		t.Errorf("SupportedList = %q", got)
	}
}

func TestInstallIsIdempotentAndHonorsForce(t *testing.T) {
	home := t.TempDir()
	platform, _ := Lookup("claude")
	content := []byte("skill-v1\n")
	dest := platform.AbsPath(home)

	result, err := Install(home, platform, content, false)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if result.Action != ActionInstalled || result.Path != dest {
		t.Fatalf("result = %+v, want installed at %s", result, dest)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != string(content) {
		t.Fatalf("installed bytes = %q, err=%v", data, err)
	}

	result, err = Install(home, platform, content, false)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if result.Action != ActionAlreadyInstalled {
		t.Fatalf("second install action = %q, want already-installed", result.Action)
	}

	if err := os.WriteFile(dest, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err = Install(home, platform, content, false)
	if err != nil {
		t.Fatalf("install over local edit: %v", err)
	}
	if result.Action != ActionKept {
		t.Fatalf("action = %q, want kept", result.Action)
	}
	if data, _ := os.ReadFile(dest); string(data) != "local edit\n" {
		t.Fatalf("kept file was overwritten: %q", data)
	}

	result, err = Install(home, platform, content, true)
	if err != nil {
		t.Fatalf("forced install: %v", err)
	}
	if result.Action != ActionOverwritten {
		t.Fatalf("action = %q, want overwritten", result.Action)
	}
	if data, _ := os.ReadFile(dest); string(data) != string(content) {
		t.Fatalf("forced file = %q", data)
	}
}

func TestUninstallRemovesOnlyOurFileAndEmptyDirectory(t *testing.T) {
	home := t.TempDir()
	platform, _ := Lookup("claude")
	if _, err := Install(home, platform, []byte("skill\n"), false); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Dir(platform.AbsPath(home))
	skillsRoot := filepath.Dir(skillDir)
	// Another skill must survive.
	other := filepath.Join(skillsRoot, "other", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Uninstall(home, platform)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if result.Action != ActionRemoved {
		t.Fatalf("action = %q, want removed", result.Action)
	}
	if _, err := os.Stat(platform.AbsPath(home)); !os.IsNotExist(err) {
		t.Fatalf("our file still exists: %v", err)
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("empty skill dir still exists: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("sibling skill was removed: %v", err)
	}
}

func TestUninstallKeepsNonEmptySkillDirectory(t *testing.T) {
	home := t.TempDir()
	platform, _ := Lookup("claude")
	if _, err := Install(home, platform, []byte("skill\n"), false); err != nil {
		t.Fatal(err)
	}
	reference := filepath.Join(filepath.Dir(platform.AbsPath(home)), "references", "x.md")
	if err := os.MkdirAll(filepath.Dir(reference), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reference, []byte("ref\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(home, platform); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(reference); err != nil {
		t.Fatalf("user file in the skill directory was removed: %v", err)
	}
}

func TestUninstallMissingIsNotAnError(t *testing.T) {
	home := t.TempDir()
	platform, _ := Lookup("codex")
	result, err := Uninstall(home, platform)
	if err != nil {
		t.Fatalf("uninstall missing: %v", err)
	}
	if result.Action != ActionMissing {
		t.Fatalf("action = %q, want missing", result.Action)
	}
}

func TestPurgeOnlyRemovesTheIndexDirectory(t *testing.T) {
	root := t.TempDir()
	indexDir := filepath.Join(root, ".repath")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "index.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "app.py")
	if err := os.WriteFile(source, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, removed, err := Purge(root)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if !removed || path != indexDir {
		t.Fatalf("purge = %q removed=%v, want %q true", path, removed, indexDir)
	}
	if _, err := os.Stat(indexDir); !os.IsNotExist(err) {
		t.Fatalf(".repath still exists: %v", err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("purge removed a source file: %v", err)
	}

	if _, removed, err := Purge(root); err != nil || removed {
		t.Fatalf("second purge = removed=%v err=%v, want false nil", removed, err)
	}
}

func TestPurgeRefusesAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".repath"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Purge(root); err == nil {
		t.Fatal("purge accepted a regular file at .repath")
	}
}
