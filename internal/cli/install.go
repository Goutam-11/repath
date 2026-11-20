package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/Goutam-11/repath/internal/agentskill"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/skills"
)

// installCmd copies the embedded Agent Skill into one or all supported agent
// skill directories. It is a pure file copy: no index, no network, and no
// config file.
func installCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "install takes no positional arguments\n\n%s", usage)
		return 2
	}
	if opts.purge {
		fmt.Fprintf(stderr, "install does not take --purge (use uninstall --purge)\n\n%s", usage)
		return 2
	}
	selected, err := selectPlatforms(opts)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "resolve home directory: %v\n", err)
		return 1
	}
	results := make([]agentskill.Result, 0, len(selected))
	for _, platform := range selected {
		result, err := agentskill.Install(home, platform, skills.Repath, opts.force)
		if err != nil {
			fmt.Fprintf(stderr, "install %s: %v\n", platform.Name, err)
			return 1
		}
		results = append(results, result)
	}
	if opts.json {
		if err := writeJSON(stdout, installReport{Home: home, Results: results}); err != nil {
			fmt.Fprintf(stderr, "encode install: %v\n", err)
			return 1
		}
		return 0
	}
	for _, result := range results {
		fmt.Fprintf(stdout, "%s %s at %s\n", result.Action, result.Platform, result.Path)
		if result.Action == agentskill.ActionKept {
			fmt.Fprintln(stdout, "  exists with different content; rerun with --force to overwrite")
		}
	}
	return 0
}

// uninstallCmd removes our SKILL.md and, with --purge, the local index. A
// missing file is success.
func uninstallCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "uninstall takes no positional arguments\n\n%s", usage)
		return 2
	}
	if opts.force {
		fmt.Fprintf(stderr, "uninstall does not take --force\n\n%s", usage)
		return 2
	}
	selected, err := selectPlatforms(opts)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "resolve home directory: %v\n", err)
		return 1
	}
	report := uninstallReport{Home: home}
	for _, platform := range selected {
		result, err := agentskill.Uninstall(home, platform)
		if err != nil {
			fmt.Fprintf(stderr, "uninstall %s: %v\n", platform.Name, err)
			return 1
		}
		report.Results = append(report.Results, result)
	}
	if opts.purge {
		root, err := query.ResolveRoot(opts.root)
		if err != nil {
			fmt.Fprintf(stderr, "resolve root: %v\n", err)
			return 1
		}
		path, removed, err := agentskill.Purge(root)
		if err != nil {
			fmt.Fprintf(stderr, "purge %s: %v\n", path, err)
			return 1
		}
		report.Purged = path
		report.PurgeRemoved = removed
	}
	if opts.json {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintf(stderr, "encode uninstall: %v\n", err)
			return 1
		}
		return 0
	}
	for _, result := range report.Results {
		fmt.Fprintf(stdout, "%s %s at %s\n", result.Action, result.Platform, result.Path)
	}
	if report.Purged != "" {
		if report.PurgeRemoved {
			fmt.Fprintf(stdout, "purged %s\n", report.Purged)
		} else {
			fmt.Fprintf(stdout, "nothing to purge at %s\n", report.Purged)
		}
	}
	return 0
}

// selectPlatforms resolves --platform (default claude) or --all.
func selectPlatforms(opts options) ([]agentskill.Platform, error) {
	if opts.all {
		if opts.platform != "" {
			return nil, fmt.Errorf("--all and --platform are mutually exclusive")
		}
		return agentskill.Platforms(), nil
	}
	name := opts.platform
	if name == "" {
		name = "claude"
	}
	platform, ok := agentskill.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown platform %q; supported: %s", name, agentskill.SupportedList())
	}
	return []agentskill.Platform{platform}, nil
}

type installReport struct {
	Home    string              `json:"home"`
	Results []agentskill.Result `json:"results"`
}

type uninstallReport struct {
	Home         string              `json:"home"`
	Results      []agentskill.Result `json:"results"`
	Purged       string              `json:"purged,omitempty"`
	PurgeRemoved bool                `json:"purge_removed,omitempty"`
}
