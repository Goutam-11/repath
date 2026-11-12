package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

var (
	// ErrNotGitRepo marks a diff request outside a git work tree. The CLI maps
	// it to exit code 3.
	ErrNotGitRepo = errors.New("not a git work tree")

	// ErrDiffBaseNotFound marks an unknown base ref or a ref that cannot be
	// resolved to a commit. The CLI maps it to exit code 3.
	ErrDiffBaseNotFound = errors.New("diff base not found")
)

// DefaultDiffBases is the documented fallback order for `tests --diff` with no
// explicit base: a fetched mainline first, then a local main, then HEAD.
var DefaultDiffBases = []string{"origin/main", "main", "HEAD"}

// ChangedFile is one entry from `git diff --name-status`, repo-relative POSIX.
type ChangedFile struct {
	Path   string
	Status string
}

// ResolveDiffBase validates an explicit base or picks the first available
// default. It returns the user-visible ref name, not a SHA, so reports say
// "origin/main" rather than a hash.
func ResolveDiffBase(ctx context.Context, root, explicit string) (string, error) {
	if err := requireGitRepo(ctx, root); err != nil {
		return "", err
	}

	candidates := DefaultDiffBases
	if explicit != "" {
		candidates = []string{explicit}
	}
	for _, candidate := range candidates {
		if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "--quiet", candidate+"^{commit}"); err == nil {
			return candidate, nil
		}
	}
	if explicit == "" {
		return "", fmt.Errorf("%w: tried %s", ErrDiffBaseNotFound, strings.Join(DefaultDiffBases, ", "))
	}
	return "", fmt.Errorf("%w: %s", ErrDiffBaseNotFound, explicit)
}

// ChangedFiles lists the names and statuses of files that differ between base
// and the current working tree. This is the single diff walker: it shells out
// to git and does not reimplement ignore, rename, or binary rules. Untracked
// files are not included (git diff does not see them); `git add` them first.
func ChangedFiles(ctx context.Context, root, base string) ([]ChangedFile, error) {
	if err := requireGitRepo(ctx, root); err != nil {
		return nil, err
	}
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrDiffBaseNotFound, base)
	}

	// --relative keeps paths rooted at the caller's --root, matching the index,
	// even when --root is a subdirectory of a larger work tree.
	out, err := gitOutput(ctx, root, "diff", "--relative", "--name-status", "-z", "--no-ext-diff", base, "--")
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w", base, err)
	}
	changed, err := parseNameStatus(out)
	if err != nil {
		return nil, err
	}
	return changed, nil
}

// parseNameStatus reads the NUL-delimited `git diff --name-status -z` format.
// A normal entry is `status\0path`; a rename/copy is `R100\0old\0new`.
func parseNameStatus(out []byte) ([]ChangedFile, error) {
	fields := strings.Split(string(out), "\x00")
	changed := make([]ChangedFile, 0, len(fields))
	for i := 0; i < len(fields); {
		status := fields[i]
		i++
		if status == "" {
			continue
		}
		code := status[0]
		switch code {
		case 'R', 'C':
			if i+1 >= len(fields) {
				return nil, fmt.Errorf("malformed git rename status %q", status)
			}
			oldPath := fields[i]
			newPath := fields[i+1]
			i += 2
			if newPath == "" {
				return nil, fmt.Errorf("malformed git rename from %q", oldPath)
			}
			changed = append(changed, ChangedFile{Path: newPath, Status: statusName(code)})
		default:
			if i >= len(fields) {
				return nil, fmt.Errorf("malformed git status %q", status)
			}
			path := fields[i]
			i++
			if path == "" {
				continue
			}
			changed = append(changed, ChangedFile{Path: path, Status: statusName(code)})
		}
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].Path < changed[j].Path })
	return changed, nil
}

func statusName(code byte) string {
	switch code {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "type-changed"
	case 'U':
		return "unmerged"
	default:
		return "modified"
	}
}

func requireGitRepo(ctx context.Context, root string) error {
	out, err := gitOutput(ctx, root, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("%w: %s", ErrNotGitRepo, root)
	}
	return nil
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return cmd.Output()
}
