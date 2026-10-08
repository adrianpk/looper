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
	for _, bad := range []string{strings.Replace(plan, "Slice 1", "Slice 2", 1), strings.Replace(plan, "ops/default/report/slice-1.md", "../outside.md", 1), "no slice map"} {
		if _, err := parseSliceTable(bad); err == nil {
			t.Fatal("invalid slice map accepted")
		}
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
