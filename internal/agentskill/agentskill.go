// Package agentskill installs repath's Agent Skill file into the user-scope
// skill directories of the supported coding agents.
//
// It is deliberately a pure file copy: no config file is read or written, no
// network call is made, and no index is opened. The platform table is the
// contract; docs/install.md lists the same paths.
package agentskill

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SkillName is the skill directory name and the frontmatter name in SKILL.md.
const SkillName = "repath"

// Platform is one supported agent: a stable flag name and the user-scope
// SKILL.md path relative to the home directory, slash-separated.
type Platform struct {
	Name string
	Path string
}

// platforms is ordered for the help text and documentation. Codex uses the
// cross-agent `~/.agents/skills` location from the OpenAI docs; the others use
// their vendor's user-scope skills directory.
var platforms = []Platform{
	{Name: "claude", Path: ".claude/skills/" + SkillName + "/SKILL.md"},
	{Name: "codex", Path: ".agents/skills/" + SkillName + "/SKILL.md"},
	{Name: "cursor", Path: ".cursor/skills/" + SkillName + "/SKILL.md"},
	{Name: "gemini", Path: ".gemini/skills/" + SkillName + "/SKILL.md"},
	{Name: "vscode", Path: ".copilot/skills/" + SkillName + "/SKILL.md"},
}

// Platforms returns the supported platforms in a stable order.
func Platforms() []Platform {
	out := make([]Platform, len(platforms))
	copy(out, platforms)
	return out
}

// Names returns the platform flag names for usage and error messages.
func Names() []string {
	out := make([]string, len(platforms))
	for i, p := range platforms {
		out[i] = p.Name
	}
	return out
}

// Lookup finds a platform by its flag name.
func Lookup(name string) (Platform, bool) {
	for _, p := range platforms {
		if p.Name == name {
			return p, true
		}
	}
	return Platform{}, false
}

// AbsPath is the absolute SKILL.md path for this platform under home.
func (p Platform) AbsPath(home string) string {
	return filepath.Join(home, filepath.FromSlash(p.Path))
}

// Action is what Install or Uninstall did to the destination.
type Action string

const (
	// ActionInstalled wrote a new file.
	ActionInstalled Action = "installed"
	// ActionAlreadyInstalled found the identical file and wrote nothing.
	ActionAlreadyInstalled Action = "already-installed"
	// ActionKept found a different file and left it alone (no --force).
	ActionKept Action = "kept"
	// ActionOverwritten replaced a different file because --force was set.
	ActionOverwritten Action = "overwritten"
	// ActionRemoved deleted our file.
	ActionRemoved Action = "removed"
	// ActionMissing found nothing to remove.
	ActionMissing Action = "missing"
)

// Result is the outcome for one platform.
type Result struct {
	Platform string `json:"platform"`
	Path     string `json:"path"`
	Action   Action `json:"action"`
}

// Install copies content to the platform's SKILL.md. An existing identical file
// is a no-op; an existing different file is kept unless force is set.
func Install(home string, p Platform, content []byte, force bool) (Result, error) {
	dest := p.AbsPath(home)
	action := ActionInstalled
	existing, err := os.ReadFile(dest)
	switch {
	case err == nil:
		if bytes.Equal(existing, content) {
			return Result{Platform: p.Name, Path: dest, Action: ActionAlreadyInstalled}, nil
		}
		if !force {
			return Result{Platform: p.Name, Path: dest, Action: ActionKept}, nil
		}
		action = ActionOverwritten
	case errors.Is(err, fs.ErrNotExist):
		// New file.
	default:
		return Result{}, fmt.Errorf("%s: %w", dest, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, fmt.Errorf("%s: %w", dest, err)
	}
	if err := os.WriteFile(dest, content, 0o644); err != nil {
		return Result{}, fmt.Errorf("%s: %w", dest, err)
	}
	return Result{Platform: p.Name, Path: dest, Action: action}, nil
}

// Uninstall removes our SKILL.md and the skill directory when it is empty. It
// never removes the platform's skills root or another skill.
func Uninstall(home string, p Platform) (Result, error) {
	dest := p.AbsPath(home)
	if _, err := os.Stat(dest); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Platform: p.Name, Path: dest, Action: ActionMissing}, nil
		}
		return Result{}, fmt.Errorf("%s: %w", dest, err)
	}
	if err := os.Remove(dest); err != nil {
		return Result{}, fmt.Errorf("%s: %w", dest, err)
	}
	// Remove the now-empty <skills>/repath directory; a non-empty one (another
	// file the user added) is left in place.
	_ = os.Remove(filepath.Dir(dest))
	return Result{Platform: p.Name, Path: dest, Action: ActionRemoved}, nil
}

// Purge removes <root>/.repath, the index directory. It is only called for an
// explicit --purge and never by Uninstall itself. It returns the path and
// whether anything was removed.
func Purge(root string) (string, bool, error) {
	path := filepath.Join(root, ".repath")
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, false, nil
		}
		return path, false, err
	}
	if !info.IsDir() {
		return path, false, fmt.Errorf("%s is not a directory", path)
	}
	if err := os.RemoveAll(path); err != nil {
		return path, false, err
	}
	return path, true, nil
}

// SupportedList is the "claude, codex, ..." string used in errors and docs.
func SupportedList() string { return strings.Join(Names(), ", ") }
