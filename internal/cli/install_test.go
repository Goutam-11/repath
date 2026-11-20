package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/skills"
)

// tempHome points HOME at a fresh directory so no test can touch a real agent
// configuration directory.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestInstallClaudeWritesOneFileAndIsIdempotent(t *testing.T) {
	home := tempHome(t)
	code, out, errOut := run(t, "install", "--platform", "claude")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	dest := filepath.Join(home, ".claude", "skills", "repath", "SKILL.md")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed skill: %v", err)
	}
	if !bytes.Equal(data, skills.Repath) {
		t.Fatalf("installed bytes do not match the embedded skill")
	}
	if !strings.Contains(out, "installed claude at "+dest) {
		t.Fatalf("stdout = %q", out)
	}

	code, out, errOut = run(t, "install", "--platform", "claude")
	if code != 0 {
		t.Fatalf("second exit code = %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "already-installed claude") {
		t.Fatalf("second stdout = %q, want a no-op report", out)
	}

	files := 0
	err = filepath.WalkDir(home, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Fatalf("home has %d files, want exactly 1", files)
	}
}

func TestInstallDefaultsToClaude(t *testing.T) {
	home := tempHome(t)
	if code, _, errOut := run(t, "install"); code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "repath", "SKILL.md")); err != nil {
		t.Fatalf("default platform did not install claude: %v", err)
	}
}

func TestInstallAllWritesEveryPlatform(t *testing.T) {
	home := tempHome(t)
	if code, _, errOut := run(t, "install", "--all"); code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	for _, rel := range []string{
		".claude/skills/repath/SKILL.md",
		".agents/skills/repath/SKILL.md",
		".cursor/skills/repath/SKILL.md",
		".gemini/skills/repath/SKILL.md",
		".copilot/skills/repath/SKILL.md",
	} {
		path := filepath.Join(home, filepath.FromSlash(rel))
		if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, skills.Repath) {
			t.Errorf("%s: err=%v", rel, err)
		}
	}
}

func TestInstallForceOverwritesLocalEdit(t *testing.T) {
	home := tempHome(t)
	dest := filepath.Join(home, ".claude", "skills", "repath", "SKILL.md")
	if code, _, errOut := run(t, "install", "--platform", "claude"); code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	if err := os.WriteFile(dest, []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, "install", "--platform", "claude")
	if code != 0 || !strings.Contains(out, "kept claude") {
		t.Fatalf("kept run = code %d stdout %q", code, out)
	}
	if data, _ := os.ReadFile(dest); string(data) != "local edit\n" {
		t.Fatalf("install overwrote without --force: %q", data)
	}

	code, out, _ = run(t, "install", "--platform", "claude", "--force")
	if code != 0 || !strings.Contains(out, "overwritten claude") {
		t.Fatalf("force run = code %d stdout %q", code, out)
	}
	if data, _ := os.ReadFile(dest); !bytes.Equal(data, skills.Repath) {
		t.Fatalf("forced install did not restore the skill")
	}
}

func TestInstallUnknownPlatformIsUsageError(t *testing.T) {
	tempHome(t)
	code, _, errOut := run(t, "install", "--platform", "nope")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, `unknown platform "nope"`) ||
		!strings.Contains(errOut, "claude, codex, cursor, gemini, vscode") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestInstallRejectsPlatformWithAll(t *testing.T) {
	tempHome(t)
	code, _, errOut := run(t, "install", "--platform", "claude", "--all")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut, "mutually exclusive") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestInstallRejectsPurge(t *testing.T) {
	tempHome(t)
	if code, _, _ := run(t, "install", "--purge"); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestInstallRejectsPositionalArguments(t *testing.T) {
	tempHome(t)
	if code, _, _ := run(t, "install", "extra"); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestInstallJSON(t *testing.T) {
	home := tempHome(t)
	code, out, errOut := run(t, "install", "--platform", "codex", "--json")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	var report struct {
		Home    string `json:"home"`
		Results []struct {
			Platform string `json:"platform"`
			Path     string `json:"path"`
			Action   string `json:"action"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if report.Home != home || len(report.Results) != 1 {
		t.Fatalf("report = %+v", report)
	}
	got := report.Results[0]
	if got.Platform != "codex" || got.Action != "installed" ||
		got.Path != filepath.Join(home, ".agents", "skills", "repath", "SKILL.md") {
		t.Fatalf("result = %+v", got)
	}
}

func TestUninstallRestoresState(t *testing.T) {
	home := tempHome(t)
	if code, _, errOut := run(t, "install", "--platform", "claude"); code != 0 {
		t.Fatalf("install exit code = %d, stderr=%q", code, errOut)
	}
	code, out, errOut := run(t, "uninstall", "--platform", "claude")
	if code != 0 {
		t.Fatalf("uninstall exit code = %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "removed claude") {
		t.Fatalf("stdout = %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "repath", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("skill still exists: %v", err)
	}

	code, out, _ = run(t, "uninstall", "--platform", "claude")
	if code != 0 || !strings.Contains(out, "missing claude") {
		t.Fatalf("second uninstall = code %d stdout %q", code, out)
	}
}

func TestUninstallPurgeIsExplicit(t *testing.T) {
	tempHome(t)
	root := t.TempDir()
	indexDir := filepath.Join(root, ".repath")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, "index.db"), []byte("db"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "uninstall", "--platform", "claude", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr=%q", code, errOut)
	}
	if strings.Contains(out, "purged") {
		t.Fatalf("uninstall purged without --purge: %q", out)
	}
	if _, err := os.Stat(indexDir); err != nil {
		t.Fatalf("plain uninstall removed .repath: %v", err)
	}

	code, out, errOut = run(t, "uninstall", "--platform", "claude", "--purge", "--root", root)
	if code != 0 {
		t.Fatalf("purge exit code = %d, stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "purged "+indexDir) {
		t.Fatalf("stdout = %q", out)
	}
	if _, err := os.Stat(indexDir); !os.IsNotExist(err) {
		t.Fatalf(".repath was not removed: %v", err)
	}
}
