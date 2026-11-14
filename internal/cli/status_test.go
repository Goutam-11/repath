package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/store"
)

// Unresolved reasons are additive status output: the JSON gains the buckets
// plus bounded top lists, and the text renderer explains the same numbers.
func TestStatusReportsUnresolvedReasons(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "src/a.ts", "export class Box {\n  open(): void {}\n}\n")
	writeFile(t, dir, "src/b.ts", "export function run(): void {\n  unknownThing();\n}\nexport function touch(box) {\n  box.open();\n}\n")

	code, out, errOut := run(t, "status", "--json", "--root", dir)
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	if report.Unresolved != 2 {
		t.Fatalf("unresolved = %d, want 2", report.Unresolved)
	}
	sum := 0
	for _, reason := range store.PendingReasons {
		count, ok := report.UnresolvedByReason[reason]
		if !ok {
			t.Fatalf("unresolved_by_reason is missing bucket %q: %+v", reason, report.UnresolvedByReason)
		}
		sum += count
	}
	if sum != report.Unresolved {
		t.Fatalf("reason sum = %d, want unresolved %d (%+v)", sum, report.Unresolved, report.UnresolvedByReason)
	}
	if report.UnresolvedByReason[store.PendingReasonMissingDefinition] != 1 ||
		report.UnresolvedByReason[store.PendingReasonMemberScope] != 1 {
		t.Fatalf("unresolved_by_reason = %+v, want one missing-definition and one member-scope",
			report.UnresolvedByReason)
	}
	if len(report.UnresolvedByLang) == 0 || report.UnresolvedByLang["typescript"] != 2 {
		t.Fatalf("unresolved_by_lang = %+v, want typescript 2", report.UnresolvedByLang)
	}
	if len(report.UnresolvedTopNames) == 0 {
		t.Fatalf("unresolved_top_names is empty: %+v", report)
	}
	names := map[string]int{}
	for _, bucket := range report.UnresolvedTopNames {
		names[bucket.Name] = bucket.Count
	}
	if names["open"] != 1 || names["unknownThing"] != 1 {
		t.Fatalf("unresolved_top_names = %+v", report.UnresolvedTopNames)
	}
	if len(report.UnresolvedTopModules) == 0 {
		t.Fatalf("unresolved_top_modules is empty: %+v", report)
	}

	code, text, errOut := run(t, "status", "--root", dir)
	if code != 0 {
		t.Fatalf("text status exit = %d: %s", code, errOut)
	}
	for _, want := range []string{
		"unresolved reasons: parse-failure 0 · member-scope 1 · missing-definition 1 · scope-gap 0",
		"unresolved top names: ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("text output missing %q:\n%s", want, text)
		}
	}
}

// A clean graph keeps the old text contract: no unresolved lines appear when
// there is nothing to explain.
func TestStatusHidesUnresolvedReasonsWhenClean(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.ts", "export function run(): void {}\n")

	code, out, errOut := run(t, "status", "--root", dir)
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	if strings.Contains(out, "unresolved reasons:") || strings.Contains(out, "unresolved top names:") {
		t.Fatalf("clean repo printed unresolved detail:\n%s", out)
	}
}
