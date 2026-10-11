package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func completedLoop(t *testing.T) (*app, string, fixture) {
	t.Helper()
	a, path := setupLoop(t, "normal")
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	return a, path, f
}

// Historical completion survives a new repository owner; inspection never grants an execution lease.
func TestCompletedRunOwnership(t *testing.T) {
	for _, mode := range []string{"complete", "unfinished", "invalid checkpoint", "own controller"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _, _ := completedLoop(t)
			entry, err := registerRun(a)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unfinished" {
				a.state.Status = "blocked"
				if err := a.save(); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "invalid checkpoint" {
				a.config.FullGate = "false"
				if err := writeJSON(a.path, a.config); err != nil {
					t.Fatal(err)
				}
			}
			owner := &app{config: a.config, dir: t.TempDir()}
			if mode == "own controller" {
				owner = a
			}
			lease, err := acquireRun(owner)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			r := inspectRun(entry)
			switch mode {
			case "complete":
				if r.Status != "complete" || r.CheckpointStatus != "complete" || r.Blocker != nil {
					t.Fatalf("historical completion changed: %+v", r)
				}
			case "unfinished":
				if r.Status != "blocked" || r.Blocker == nil || r.Blocker.Code != "repository_busy" {
					t.Fatalf("unfinished run lost ownership protection: %+v", r)
				}
			case "invalid checkpoint":
				if r.Status != "invalid" || r.Blocker == nil || r.Blocker.Code != "invalid_checkpoint" {
					t.Fatalf("completion bypassed checkpoint validation: %+v", r)
				}
			case "own controller":
				if r.Status != "running" || r.CheckpointStatus != "complete" {
					t.Fatalf("same-run live ownership was hidden: %+v", r)
				}
			}
			if duplicate, err := acquireRun(a); err == nil {
				duplicate.close()
				t.Fatal("inspection bypassed repository ownership")
			}
			after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("inspection changed the checkpoint", err)
			}
		})
	}
}

// Closure instructions identify the real artifact and condition, while no-progress turns remain bounded.
func TestClosureFeedback(t *testing.T) {
	for _, tc := range []struct {
		name, artifact, content, condition, expected string
		stalled                                      bool
	}{
		{"plan status", "plan", "Status: Approved", "Status is not Completed", "Status: Completed", false},
		{"tracker tasks", "tracker", "Status: Completed\n- [ ] Close gate", "unchecked tasks or gates remain", "no unchecked tasks or gates", false},
		{"report status", "report", "Status: reviewing", "Status is not Delivered", "Status: Delivered", false},
		{"stalled closure", "plan", "Status: Approved", "Status is not Completed", "Status: Completed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, f := completedLoop(t)
			artifact := map[string]string{"plan": a.config.Plan, "tracker": a.config.Tracker, "report": a.config.Slices[0].Report}[tc.artifact]
			data, err := os.ReadFile(filepath.Join(a.config.Repo, artifact))
			if err != nil {
				t.Fatal(err)
			}
			_, rest, _ := strings.Cut(string(data), "\n")
			if err := os.WriteFile(filepath.Join(a.config.Repo, artifact), []byte(tc.content+"\n"+rest), 0600); err != nil {
				t.Fatal(err)
			}
			if err := fixtureCommit(a.config.Repo, "docs: leave closure pending", true); err != nil {
				t.Fatal(err)
			}
			f.Mode = "closure-feedback"
			if tc.stalled {
				f.Mode = "closure-stalled"
			}
			if err := writeJSON(path, f); err != nil {
				t.Fatal(err)
			}
			before := a.state
			a.state.Status = "running"
			if err := a.closeSet(); err == nil {
				t.Fatal("pending metadata was marked complete")
			}
			var after fixture
			if err := readJSON(path, &after); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{artifact, tc.condition, tc.expected} {
				if !strings.Contains(after.CloseTask, want) {
					t.Fatalf("closure feedback lost %q: %s", want, after.CloseTask)
				}
			}
			turns := 1
			if tc.stalled {
				turns = a.config.MaxStalled
			}
			if a.state.Turns != before.Turns+turns || after.Calls != f.Calls+turns || after.Merges != f.Merges || after.Prepare != f.Prepare || !reflect.DeepEqual(a.state.Gate, before.Gate) || a.state.Session != before.Session {
				t.Fatal("closure changed evidence, conversation, merges or turn bounds")
			}
		})
	}
}

// Access and integrity failures retain progress and return blockers before another documentation turn.
func TestClosureBlockers(t *testing.T) {
	for _, tc := range []struct{ name, class string }{
		{"authentication", "authentication"}, {"transient", "transient"},
		{"canonical merge", "integrity"}, {"unapproved change", "integrity"},
		{"missing report", "integrity"}, {"unapproved metadata", "integrity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, path, f := completedLoop(t)
			switch tc.name {
			case "authentication":
				f.Mode = "closure-auth"
			case "transient":
				f.Mode = "closure-transient"
			case "canonical merge":
				f.PR.MergedCommit = strings.Repeat("a", 40)
			case "unapproved change":
				if err := os.WriteFile(filepath.Join(a.config.Repo, "runtime.txt"), []byte("changed after gate\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing report":
				if err := os.Remove(filepath.Join(a.config.Repo, a.config.Slices[0].Report)); err != nil {
					t.Fatal(err)
				}
			case "unapproved metadata":
				data, err := os.ReadFile(filepath.Join(a.config.Repo, a.config.Plan))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(a.config.Repo, a.config.Plan), []byte(strings.Replace(string(data), "Status: Completed", "Status: Approved", 1)), 0600); err != nil {
					t.Fatal(err)
				}
				a.config.ClosurePaths = []string{a.config.Tracker, a.config.Slices[0].Report}
			}
			if tc.name == "unapproved change" || tc.name == "missing report" || tc.name == "unapproved metadata" {
				if err := fixtureCommit(a.config.Repo, "test: introduce closure failure", true); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeJSON(path, f); err != nil {
				t.Fatal(err)
			}
			before := a.state
			a.state.Status = "running"
			err := a.closeSet()
			var failure *executionFailure
			action := map[string]string{"authentication": "restore_host_authentication_then_resume", "transient": "restore_service_then_resume", "integrity": "inspect_closure_integrity"}[tc.class]
			attempts := 1
			if tc.class == "transient" {
				attempts = 3
			}
			if !errors.As(err, &failure) || failure.Blocker.Class != tc.class || failure.Blocker.Attempts != attempts || failure.Blocker.NextAction == nil || failure.Blocker.NextAction.Type != action {
				t.Fatalf("closure failure lost its class or recovery action: %v", err)
			}
			if err := a.block(err); err == nil {
				t.Fatal("closure failure disappeared")
			}
			var after fixture
			if err := readJSON(path, &after); err != nil {
				t.Fatal(err)
			}
			if a.state.Turns != before.Turns || after.Calls != f.Calls || after.Merges != f.Merges || after.Closes != f.Closes || a.state.Blocker == nil || a.state.Blocker.Class != tc.class || !reflect.DeepEqual(a.state.Gate, before.Gate) || a.state.Session != before.Session || !reflect.DeepEqual(a.state.Merged, before.Merged) {
				t.Fatal("closure failure launched blind edits or lost progress")
			}
			if strings.Contains(a.state.Feedback, "private-value") {
				t.Fatal("access diagnostics leaked into closure feedback")
			}
		})
	}
}
