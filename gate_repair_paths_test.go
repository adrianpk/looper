package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A missing path can be approved without changing prior authority or resetting stalled work.
func TestRepairPathRecovery(t *testing.T) {
	a, _ := setupLoop(t, "gate-auto-repair")
	a.config.FullGate = "test \"$(head -n 1 runtime.txt)\" = repaired"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	policy := fixtureRepairPolicy()
	policy.AllowedPaths = []string{"ops/default/report/slice-1.md"}
	a.state.GateRepairPolicy = &policy
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	if err := a.run(); err == nil || a.activeGateRepair() == nil {
		t.Fatalf("missing path did not block an active repair: %v", err)
	}
	// The public controller records the returned failure as blocked.
	a.state.Status = "blocked"
	a.state.Stalled = a.config.MaxStalled
	a.state.Fingerprint, err = a.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before := a.state
	config, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"authorize-gate-repair-paths", "--config", a.path, "--failed-head", a.state.GateFailure.Head, "--path", "runtime.txt", "--reason", "Include the existing fixture omitted during preparation"}
	if err := execute(args); err != nil {
		t.Fatal(err)
	}
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	authorized := a.state
	authorized.GateRepairPaths = nil
	if !reflect.DeepEqual(authorized, before) {
		t.Fatal("path authorization rewrote policy, failure, conversation, progress or counters")
	}
	if progress, err := a.fingerprint(); err != nil || progress == before.Fingerprint {
		t.Fatal("new file authority did not resolve the observable progress blocker")
	}
	if !a.gateRepairPathGranted(1, "runtime.txt") || a.gateRepairPathGranted(1, "runtime.txt/child") || a.gateRepairPathGranted(2, "runtime.txt") {
		t.Fatal("exact-file authority escaped its file or repair")
	}
	checkpoint, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(args); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(filepath.Join(a.dir, "state.json")); err != nil || string(after) != string(checkpoint) {
		t.Fatal("replayed approval changed the checkpoint")
	}
	// Recovery must reject a grant moved to another repair, base, head or directory.
	for _, tc := range []struct {
		name   string
		change func(*gateRepairPathGrant)
	}{
		{"different repair", func(g *gateRepairPathGrant) { g.Repair++ }},
		{"different base", func(g *gateRepairPathGrant) { g.FailedHead = strings.Repeat("b", 40) }},
		{"unknown head", func(g *gateRepairPathGrant) { g.AuthorizedHead = strings.Repeat("b", 40) }},
		{"directory scope", func(g *gateRepairPathGrant) { g.Paths = []string{"ops"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shadow := *a
			shadow.state.GateRepairPaths = append([]gateRepairPathGrant(nil), a.state.GateRepairPaths...)
			tc.change(&shadow.state.GateRepairPaths[0])
			if err := shadow.verifyGateRepairState(); err == nil {
				t.Fatal("inconsistent path authorization accepted during recovery")
			}
		})
	}
	if err := a.run(); err != nil || a.state.Status != "complete" || a.state.GateFailure != nil || a.state.Gate == nil {
		t.Fatalf("approved path did not recover through normal validation and merge: %v", err)
	}
	if a.state.Session != before.Session || a.state.Turns < before.Turns || !reflect.DeepEqual(a.state.GateRepairPolicy, before.GateRepairPolicy) {
		t.Fatal("recovery lost prior policy, conversation or worker turns")
	}
	if after, err := os.ReadFile(a.path); err != nil || string(after) != string(config) {
		t.Fatal("recovery changed frozen configuration")
	}
}

// Rejected grants must neither broaden file scope nor change the retained checkpoint.
func TestRepairPathGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		reason string
		change func(*testing.T, *app, *string)
		want   string
	}{
		{"directory", "ops", "Fix omitted fixture", nil, "regular tracked file"},
		{"repository root", ".", "Fix omitted fixture", nil, "regular tracked file"},
		{"git metadata", ".git/config", "Fix omitted fixture", nil, "regular tracked file"},
		{"outside checkout", "../runtime.txt", "Fix omitted fixture", nil, "regular tracked file"},
		{"missing file", "missing.txt", "Fix omitted fixture", nil, "regular tracked file"},
		{"wildcard", "*.txt", "Fix omitted fixture", nil, "regular tracked file"},
		{"empty reason", "runtime.txt", "", nil, "single-line reason"},
		{"multiline reason", "runtime.txt", "first\nsecond", nil, "single-line reason"},
		{"wrong failed head", "runtime.txt", "Fix omitted fixture", func(_ *testing.T, _ *app, head *string) { *head = strings.Repeat("b", 40) }, "exact retained failed base"},
		{"active status", "runtime.txt", "Fix omitted fixture", func(_ *testing.T, a *app, _ *string) { a.state.Status = "running" }, "blocked active repair"},
		{"uncertain merge", "runtime.txt", "Fix omitted fixture", func(_ *testing.T, a *app, _ *string) {
			a.state.Pending = &pendingMerge{PR: 2, Head: a.state.GateFailure.Head, GateRepair: true}
		}, "uncertain merge"},
		{"dirty primary", "runtime.txt", "Fix omitted fixture", func(t *testing.T, a *app, _ *string) {
			if err := os.WriteFile(filepath.Join(a.config.Repo, "dirty.txt"), []byte("uncommitted"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "checkout must be clean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := setupLoop(t, "normal")
			head, err := cleanHead(a.config.Repo)
			if err != nil {
				t.Fatal(err)
			}
			policy := fixtureRepairPolicy()
			policy.AllowedPaths = []string{"ops/default/report/slice-1.md"}
			policy.RequiredCommands = []string{"test -s runtime.txt"}
			a.state.GateRepairPolicy = &policy
			a.state.Next = len(a.config.Slices)
			a.state.Merged = []mergeRecord{{PR: 1, Head: head, MergedCommit: head}}
			a.state.Status = "blocked"
			a.state.GateFailure = &gateFailure{Head: head, Message: "fixture failure"}
			a.state.GateRepairs = []gateRepair{{Identity: a.gateRepairIdentity(1), Base: head, Failure: *a.state.GateFailure}}
			if _, err := git(a.config.Repo, "branch", a.activeGateRepair().Identity.Branch, head); err != nil {
				t.Fatal(err)
			}
			if tc.change != nil {
				tc.change(t, a, &head)
			}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			err = authorizeGateRepairPaths(a.path, head, []string{tc.path}, tc.reason)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid grant: got %v, want %q", err, tc.want)
			}
			if after, err := os.ReadFile(filepath.Join(a.dir, "state.json")); err != nil || string(before) != string(after) {
				t.Fatal("rejected grant changed checkpoint")
			}
		})
	}
}
