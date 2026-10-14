package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func canonicalBody() string {
	return "## Summary\n\nDeliver an account workflow.\n\n## Changes\n\n- Deliver the workflow.\n- [Slice 1 report](https://forge.example.org/team/app/src/commit/" + strings.Repeat("a", 40) + "/ops/default/report/slice-1.md)\n\n## Validation\n\n- `go test ./...`\n- `git diff --check`"
}

// TestBodyContract rejects structural drift rather than accepting roughly equivalent Markdown.
func TestBodyContract(t *testing.T) {
	s := slice{Number: 1, Report: "ops/default/report/slice-1.md", RequiredCommands: []string{"go test ./..."}}
	for _, tc := range []struct {
		name   string
		change func(string) string
		ok     bool
	}{
		{"canonical", func(s string) string { return s }, true},
		{"final newline", func(s string) string { return s + "\n" }, true},
		{"extra final blank", func(s string) string { return s + "\n\n" }, false},
		{"extra heading", func(s string) string { return s + "\n\n## Scope\n\nOther work" }, false},
		{"preface", func(s string) string { return "PR description\n\n" + s }, false},
		{"heading spelling", func(s string) string { return strings.Replace(s, "## Changes", "## Change", 1) }, false},
		{"heading order", func(s string) string { return strings.Replace(s, "## Summary", "## Validation", 1) }, false},
		{"summary list", func(s string) string {
			return strings.Replace(s, "Deliver an account workflow.", "- Deliver an account workflow.", 1)
		}, false},
		{"summary paragraphs", func(s string) string {
			return strings.Replace(s, "Deliver an account workflow.", "First.\n\nSecond.", 1)
		}, false},
		{"nested changes", func(s string) string {
			return strings.Replace(s, "- Deliver the workflow.", "  - Deliver the workflow.", 1)
		}, false},
		{"empty change", func(s string) string { return strings.Replace(s, "- Deliver the workflow.", "- ", 1) }, false},
		{"report not last", func(s string) string {
			return strings.Replace(s, "\n\n## Validation", "\n- More changes.\n\n## Validation", 1)
		}, false},
		{"branch report URL", func(s string) string {
			return strings.Replace(s, "src/commit/"+strings.Repeat("a", 40), "src/branch/dev", 1)
		}, false},
		{"short report SHA", func(s string) string { return strings.Replace(s, strings.Repeat("a", 40), "aaaaaaa", 1) }, false},
		{"wrong report number", func(s string) string { return strings.Replace(s, "Slice 1 report", "Slice 2 report", 1) }, false},
		{"wrong report path", func(s string) string { return strings.Replace(s, "report/slice-1.md", "report/other.md", 1) }, false},
		{"wrong forge", func(s string) string { return strings.Replace(s, "forge.example.org", "other.example.org", 1) }, false},
		{"validation prose", func(s string) string { return strings.Replace(s, "- `go test ./...`", "- Tests passed", 1) }, false},
		{"validation fences", func(s string) string { return strings.Replace(s, "- `go test ./...`", "- ```go test ./...```", 1) }, false},
		{"duplicate check", func(s string) string {
			return strings.Replace(s, "- `go test ./...`", "- `go test ./...`\n- `go test ./...`", 1)
		}, false},
		{"diff not last", func(s string) string { return s + "\n- `go vet ./...`" }, false},
		{"missing required check", func(s string) string { return strings.Replace(s, "go test ./...", "go vet ./...", 1) }, false},
		{"CRLF", func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseBody(tc.change(canonicalBody()), s, "https://forge.example.org", "team/app")
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tc.ok, err)
			}
		})
	}
}

// TestSliceMap requires exact agreement while allowing plan and tracker status columns to differ.
func TestSliceMap(t *testing.T) {
	plan := "| Slice | Short name | Exact branch | Expected PR title | Report |\n| --- | --- | --- | --- | --- |\n| Slice 1 | Flow | `feat/flow` | `feat(slice-1): deliver flow` | `ops/default/report/slice-1.md` |\n"
	tracker := strings.Replace(plan, "Exact branch", "Branch", 1)
	a, err := parseSliceTable(plan)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseSliceTable(tracker)
	if err != nil {
		t.Fatal(err)
	}
	if !sameSlices(a, b) {
		t.Fatal("equivalent plan/tracker rejected")
	}
	b[0].Title = "feat(slice-1): improvise"
	if sameSlices(a, b) {
		t.Fatal("title drift accepted")
	}
	evidence := "| Evidence | State | Meaning |\n| --- | --- | --- |\n| Slice 1 live Codex probe | passed | Separate owned workers verified. |\n| Slice 1 contract gate | passed | Exact candidate checked. |\n"
	other := "| Slice | State |\n| --- | --- |\n| Slice 9 | delivered |\n"
	irisTracker := "| Slice | Short Name | Status | Branch | Expected PR title | PR | Report |\n| --- | --- | --- | --- | --- | --- | --- |\n| Slice 1 | Flow | reviewing | `feat/flow` | `feat(slice-1): deliver flow` | pending | `ops/default/report/slice-1.md` |\n"
	for _, tc := range []struct {
		name, doc string
		ok        bool
	}{
		{"plan", plan, true},
		{"tracker", tracker, true},
		{"compact separators", strings.ReplaceAll(plan, "---", "-"), true},
		{"aligned separators", strings.ReplaceAll(plan, "---", ":---:"), true},
		{"tables before", evidence + "\n" + other + "\n" + plan, true},
		{"tables after", plan + "\n" + evidence + "\n" + other, true},
		{"prose boundary", plan + "End of slice map.\n" + evidence, true},
		{"Iris tracker", "## Entry conditions\n\n" + other + "\n## Slice status\n\n" + irisTracker + "\n## Validation evidence\n\n" + evidence, true},
		{"number gap", strings.Replace(plan, "Slice 1", "Slice 2", 1), false},
		{"number repeated", plan + strings.Split(plan, "\n")[2] + "\n", false},
		{"number padded", strings.Replace(plan, "Slice 1", "Slice 01", 1), false},
		{"number signed", strings.Replace(plan, "Slice 1", "Slice +1", 1), false},
		{"label inside", strings.Replace(plan, "Slice 1", "Slice 1 live Codex probe", 1), false},
		{"foreign row inside", plan + "| Evidence | passed | ignored | ignored | ignored |\n", false},
		{"short row", plan + "| Slice 2 |\n", false},
		{"unsafe report", strings.Replace(plan, "ops/default/report/slice-1.md", "../outside.md", 1), false},
		{"incomplete header", strings.Replace(plan, "Expected PR title", "Title", 1), false},
		{"duplicate table", plan + "\n" + tracker, false},
		{"duplicate adjacent", plan + tracker, false},
		{"duplicate continued numbering", plan + "\n" + strings.Replace(tracker, "Slice 1", "Slice 2", 1), false},
		{"missing table", "no slice map", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSliceTable(tc.doc)
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%v, want %v: %v", err == nil, tc.ok, err)
			}
			if tc.ok && !sameSlices(a, got) {
				t.Fatalf("other tables changed slice identities: %+v", got)
			}
		})
	}
}

// Candidate documents must retain frozen identities even while origin/dev is still valid.
func TestCandidateDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, artifact string
		change         func(string) string
		ok             bool
	}{
		{"unchanged", "tracker", func(s string) string { return s }, true},
		{"progress update", "tracker", func(s string) string { return s + "\nProgress: Slice 1 reviewing.\n" }, true},
		{"evidence table", "tracker", func(s string) string {
			return s + "\n## Validation evidence\n\n| Evidence | State | Meaning |\n| --- | --- | --- |\n| Slice 1 live Codex probe | passed | Owned workers verified. |\n"
		}, true},
		{"plan title drift", "plan", func(s string) string { return strings.ReplaceAll(s, "deliver flow", "change scope") }, false},
		{"tracker branch drift", "tracker", func(s string) string { return strings.ReplaceAll(s, "feat/flow", "feat/other") }, false},
		{"tracker report drift", "tracker", func(s string) string { return strings.ReplaceAll(s, "slice-1.md", "other.md") }, false},
		{"malformed row", "tracker", func(s string) string { return strings.ReplaceAll(s, "Slice 1", "Slice 1 live Codex probe") }, false},
		{"duplicate table", "tracker", func(s string) string { return s + "\n" + s }, false},
		{"approval removed", "plan", func(s string) string { return strings.ReplaceAll(s, "Approved: 2026-10-07", "Approved:") }, false},
		{"missing document", "tracker", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, fixturePath := setupLoop(t, "normal")
			for range 2 {
				if err := a.work("implement", "Prepare the approved candidate"); err != nil {
					t.Fatal(err)
				}
			}
			var f fixture
			if err := readJSON(fixturePath, &f); err != nil {
				t.Fatal(err)
			}
			artifact := a.config.Tracker
			if tc.artifact == "plan" {
				artifact = a.config.Plan
			}
			path := filepath.Join(f.Worktree, artifact)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.change == nil {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte(tc.change(string(body))), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "unchanged" {
				if err := fixtureCommit(f.Worktree, "docs: update candidate artifacts", false); err != nil {
					t.Fatal(err)
				}
			}
			f.PR.Head.SHA, err = cleanHead(f.Worktree)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(fixturePath, f); err != nil {
				t.Fatal(err)
			}
			if err := a.verifyIdentity(); err != nil {
				t.Fatalf("fixture dev identities changed: %v", err)
			}
			parsed, err := a.checkDeliveryPR(a.config.Slices[0], *f.PR)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.ensureValidation(f.Worktree, parsed.Commands); err != nil {
				t.Fatal(err)
			}
			integrated, repair, err := a.verifyAndMerge(a.config.Slices[0], *f.PR, f.Worktree, parsed.Commands)
			if tc.ok {
				if err != nil || !integrated || repair != "" {
					t.Fatalf("valid candidate rejected: integrated=%v repair=%s err=%v", integrated, repair, err)
				}
			} else if err == nil || integrated || !strings.Contains(err.Error(), artifact) || !strings.Contains(err.Error(), f.PR.Head.SHA) {
				t.Fatalf("invalid candidate must identify its commit and artifact: integrated=%v repair=%s err=%v", integrated, repair, err)
			}
			if err := readJSON(fixturePath, &f); err != nil {
				t.Fatal(err)
			}
			wantMerges := 0
			if tc.ok {
				wantMerges = 1
			}
			if f.Merges != wantMerges || f.Calls != 2 || a.state.Pending != nil || a.state.Next != wantMerges {
				t.Fatalf("candidate verification changed delivery unexpectedly: %+v state=%+v", f, a.state)
			}
		})
	}
}

func newGitFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "dev"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "test: initialize fixture"}} {
		if _, err := git(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestEvidence refuses failed checks, dirty trees, foreign repositories and HEAD-changing commands.
func TestEvidence(t *testing.T) {
	dir := newGitFixture(t)
	a := &app{config: config{Repo: dir}, dir: t.TempDir()}
	checks := []string{"test -f tracked.txt", "git diff --check"}
	if err := a.validate(dir, checks); err != nil {
		t.Fatal(err)
	}
	head, err := cleanHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	var e evidence
	if err := readJSON(a.evidencePath(head), &e); err != nil {
		t.Fatal(err)
	}
	if e.Head != head || !equalStrings(e.Commands, checks) {
		t.Fatal("successful exact-HEAD evidence missing")
	}
	if err := a.validate(dir, []string{"exit 7"}); err == nil {
		t.Fatal("failed command accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.validate(dir, checks); err == nil {
		t.Fatal("dirty checkout accepted")
	}
	if _, err := git(dir, "restore", "tracked.txt"); err != nil {
		t.Fatal(err)
	}
	other := newGitFixture(t)
	if err := a.validate(other, checks); err == nil {
		t.Fatal("foreign repository accepted")
	}
	if err := a.validate(dir, []string{"git commit --allow-empty -m 'test: change candidate'"}); err == nil {
		t.Fatal("HEAD changed during validation without invalidating evidence")
	}
	newHead, err := cleanHead(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := readJSON(a.evidencePath(newHead), &e); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("changed HEAD received evidence")
	}
}
