package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/store"
)

// SARIF ingestion is the bridge reserved by PLAN §3 decision 22 (amended for
// v1.2): repath never invokes a compiler or linter, it only reads the SARIF
// 2.1.0 log those tools already produced. A diagnostic is attached to the
// symbol enclosing its first location; anything outside a symbol is counted as
// skipped instead of disappearing.
const sarifVersion = "2.1.0"

var (
	// ErrSARIFNotFound marks a missing input or a directory with no SARIF
	// files. The CLI maps it to exit code 3.
	ErrSARIFNotFound = errors.New("sarif input not found")

	// ErrSARIFInvalid marks unreadable JSON, a non-2.1.0 version, or a shape
	// repath refuses to guess at. The CLI maps it to exit code 2.
	ErrSARIFInvalid = errors.New("invalid sarif")
)

// Diagnostic is one SARIF result normalized for the check report.
type Diagnostic struct {
	Tool     string
	RuleID   string
	Message  string
	File     string
	Line     int
	Severity string
}

// DiagnosticSummary counts one SARIF ingestion. Results always equals
// Attached+Skipped+Duplicates, so a skipped result stays visible.
type DiagnosticSummary struct {
	Sources    int `json:"sources"`
	Results    int `json:"results"`
	Attached   int `json:"attached"`
	Skipped    int `json:"skipped"`
	Duplicates int `json:"duplicates"`
}

// SARIFLoad is the normalized, deduplicated input before symbols are attached.
type SARIFLoad struct {
	Diagnostics []Diagnostic
	Summary     DiagnosticSummary
}

// LoadSARIF reads one or more SARIF 2.1.0 files. A directory input is walked
// recursively for *.sarif and *.sarif.json files. Every input is parsed before
// the caller touches the index, so a bad version or missing file cannot leave a
// half-run report behind.
func LoadSARIF(root string, inputs []string) (SARIFLoad, error) {
	load := SARIFLoad{Diagnostics: make([]Diagnostic, 0)}

	var files []string
	seenInput := make(map[string]bool)
	for _, input := range inputs {
		if strings.TrimSpace(input) == "" {
			continue
		}
		info, err := os.Stat(input)
		if err != nil {
			if os.IsNotExist(err) {
				return load, fmt.Errorf("%w: %s", ErrSARIFNotFound, input)
			}
			return load, fmt.Errorf("stat sarif %s: %w", input, err)
		}
		if info.IsDir() {
			found, err := sarifFilesIn(input)
			if err != nil {
				return load, err
			}
			if len(found) == 0 {
				return load, fmt.Errorf("%w: no .sarif files in %s", ErrSARIFNotFound, input)
			}
			files = append(files, found...)
			continue
		}
		files = append(files, input)
	}
	if len(files) == 0 {
		return load, fmt.Errorf("%w: no --sarif input", ErrSARIFNotFound)
	}

	seenResult := make(map[string]bool)
	for _, file := range files {
		if seenInput[file] {
			continue
		}
		seenInput[file] = true

		data, err := os.ReadFile(file)
		if err != nil {
			if os.IsNotExist(err) {
				return load, fmt.Errorf("%w: %s", ErrSARIFNotFound, file)
			}
			return load, fmt.Errorf("read sarif %s: %w", file, err)
		}
		log, err := parseSARIF(data, file)
		if err != nil {
			return load, err
		}
		load.Summary.Sources++

		for _, run := range log.Runs {
			tool := sarifToken(run.Tool.Driver.Name)
			if tool == "" {
				tool = "unknown"
			}
			for _, result := range run.Results {
				load.Summary.Results++
				message := strings.TrimSpace(result.Message.Text)
				if message == "" {
					message = strings.TrimSpace(result.Message.Markdown)
				}
				location, line := firstLocation(result)
				diagnostic := Diagnostic{
					Tool:     tool,
					RuleID:   sarifToken(resolveRuleID(result, run.Tool.Driver)),
					Message:  message,
					File:     normalizeSARIFPath(root, location),
					Line:     line,
					Severity: sarifSeverity(result.Level),
				}
				key := diagnosticKey(diagnostic)
				if seenResult[key] {
					load.Summary.Duplicates++
					continue
				}
				seenResult[key] = true
				load.Diagnostics = append(load.Diagnostics, diagnostic)
			}
		}
	}
	return load, nil
}

type sarifLog struct {
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name  string      `json:"name"`
	Rules []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID string `json:"id"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Rule      *sarifRuleRef   `json:"rule"`
	Level     string          `json:"level"`
	Message   sarifMessage    `json:"message"`
	Locations []sarifLocation `json:"locations"`
}

type sarifRuleRef struct {
	Index *int `json:"index"`
}

type sarifMessage struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysicalLocation `json:"physicalLocation"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           sarifRegion           `json:"region"`
}

type sarifArtifactLocation struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

func parseSARIF(data []byte, name string) (sarifLog, error) {
	var log sarifLog
	if err := json.Unmarshal(data, &log); err != nil {
		return log, fmt.Errorf("%w: %s: %v", ErrSARIFInvalid, name, err)
	}
	if log.Version != sarifVersion {
		return log, fmt.Errorf("%w: %s: unsupported SARIF version %q (only %s)",
			ErrSARIFInvalid, name, log.Version, sarifVersion)
	}
	return log, nil
}

// sarifFilesIn walks a directory for SARIF logs, sorted for deterministic runs.
func sarifFilesIn(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := strings.ToLower(d.Name())
		if strings.HasSuffix(name, ".sarif") || strings.HasSuffix(name, ".sarif.json") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk sarif %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}

// resolveRuleID prefers an explicit ruleId, then the rule index into the
// driver's rule list, then "unknown".
func resolveRuleID(result sarifResult, driver sarifDriver) string {
	if id := strings.TrimSpace(result.RuleID); id != "" {
		return id
	}
	if result.Rule != nil && result.Rule.Index != nil {
		index := *result.Rule.Index
		if index >= 0 && index < len(driver.Rules) {
			if id := strings.TrimSpace(driver.Rules[index].ID); id != "" {
				return id
			}
		}
	}
	return "unknown"
}

// sarifSeverity maps SARIF levels onto the check ladder. A missing level means
// "warning" in SARIF; "note" and "none" are informational.
func sarifSeverity(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "error":
		return SeverityError
	case "note", "none":
		return SeverityInfo
	case "warning":
		return SeverityWarning
	default:
		return SeverityWarning
	}
}

// sarifToken keeps a rule/tool id shell- and JSON-friendly: whitespace and
// slashes collapse to "-", so "my tool/rule" becomes "my-tool-rule".
func sarifToken(value string) string {
	value = strings.Join(strings.Fields(value), "-")
	value = strings.ReplaceAll(value, "/", "-")
	return strings.Trim(value, "-")
}

// normalizeSARIFPath turns a SARIF artifact URI into a repo-relative POSIX path.
// Relative URIs pass through; a file URI or an absolute path under root is
// made relative. Anything else is returned as-is and will be counted as skipped.
func normalizeSARIFPath(root, uri string) string {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return ""
	}
	if unescaped, err := url.PathUnescape(uri); err == nil {
		uri = unescaped
	}
	if strings.HasPrefix(uri, "file://") {
		if parsed, err := url.Parse(uri); err == nil {
			uri = parsed.Path
		}
	}
	uri = filepath.ToSlash(uri)
	if filepath.IsAbs(uri) && root != "" {
		if rel, err := filepath.Rel(root, uri); err == nil && !strings.HasPrefix(rel, "..") {
			uri = filepath.ToSlash(rel)
		}
	}
	return strings.TrimPrefix(uri, "./")
}

// firstLocation returns the URI and line of the first location that names a
// file, so a diagnostic's file and line cannot come from different results.
func firstLocation(result sarifResult) (string, int) {
	for _, location := range result.Locations {
		physical := location.PhysicalLocation
		if physical.ArtifactLocation.URI != "" {
			return physical.ArtifactLocation.URI, physical.Region.StartLine
		}
	}
	return "", 0
}

func diagnosticKey(d Diagnostic) string {
	return strings.Join([]string{
		d.Tool, d.RuleID, d.File, strconv.Itoa(d.Line), d.Message,
	}, "\x00")
}

// diagnosticFindings attaches each diagnostic to the symbol enclosing its line.
// A missing file, a line outside every symbol, or a malformed region is counted
// as skipped; the summary is updated in place.
func diagnosticFindings(ctx context.Context, st *store.Store, load *SARIFLoad,
	files map[string]store.FileState) ([]Finding, error) {

	// Re-running with the same load must not double-count.
	load.Summary.Attached = 0
	load.Summary.Skipped = 0

	findings := make([]Finding, 0, len(load.Diagnostics))
	for _, diagnostic := range load.Diagnostics {
		state, ok := files[diagnostic.File]
		if !ok || diagnostic.Line <= 0 {
			load.Summary.Skipped++
			continue
		}
		nodeID, found, err := st.EnclosingNode(ctx, state.ID, diagnostic.Line)
		if err != nil {
			return nil, err
		}
		if !found {
			load.Summary.Skipped++
			continue
		}
		node, err := st.NodeByID(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		load.Summary.Attached++
		findings = append(findings, Finding{
			Kind:     "diagnostic",
			Severity: diagnostic.Severity,
			Rule:     "sarif." + diagnostic.Tool + "." + diagnostic.RuleID,
			Message:  diagnostic.Message,
			QName:    node.QName,
			File:     diagnostic.File,
			Line:     diagnostic.Line,
			Count:    1,
		})
	}
	return findings, nil
}
