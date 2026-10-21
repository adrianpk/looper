package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Supplemental authority must drive a new scoped PR and a fresh full gate without rewriting history.
func TestAdditionalRepairCycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		paths []string
		want  string
	}{
		{"integrated repair", []string{"runtime.txt"}, ""},
		{"changed scope", []string{"ops/default/report/slice-1.md"}, "unauthorized path: runtime.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, fixturePath := setupLoop(t, "gate-auto-repair")
			a.config.FullGate = "test \"$(tail -n 1 runtime.txt)\" = fix/gate-2"
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			policy := fixtureRepairPolicy()
			policy.MaxRepairs = 1
			a.state.GateRepairPolicy = &policy
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			if err := a.run(); err == nil || !strings.Contains(err.Error(), "repair limit") {
				t.Fatalf("original repair allowance did not exhaust: %v", err)
			}
			before := a.state
			configBytes, err := os.ReadFile(a.path)
			if err != nil {
				t.Fatal(err)
			}
			additional := policy
			additional.Title = "fix(gate): correct another failure"
			additional.AllowedPaths = tc.paths
			additional.RequiredCommands = []string{"test -s runtime.txt"}
			policyPath := filepath.Join(t.TempDir(), "additional.json")
			if err := writeJSON(policyPath, additional); err != nil {
				t.Fatal(err)
			}
			args := []string{"authorize-additional-gate-repairs", "--config", a.path, "--policy", policyPath, "--failed-head", a.state.GateFailure.Head}
			if err := execute(args); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := execute(args); err != nil {
				t.Fatalf("idempotent authorization failed: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil || string(after) != string(checkpoint) {
				t.Fatal("repeated authorization changed the checkpoint")
			}
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			authorized := a.state
			authorized.GateRepairAuthorizations = nil
			if !reflect.DeepEqual(authorized, before) {
				t.Fatal("additional authorization changed prior state, conversation or budgets")
			}
			if err := authorizeGateRepairs(a.path, policyPath, ""); err == nil {
				t.Fatal("original authorization became replaceable")
			}
			// Recovery rejects grants that move backward or refer to a different failed candidate.
			for _, mutation := range []struct {
				name   string
				change func(*state)
			}{
				{"overlapping grant", func(s *state) { s.GateRepairAuthorizations[0].AfterRepairs = 0 }},
				{"different failure", func(s *state) { s.GateRepairAuthorizations[0].FailedHead = strings.Repeat("b", 40) }},
				{"altered prior identity", func(s *state) { s.GateRepairPolicy.Title = "changed authority" }},
			} {
				t.Run(mutation.name, func(t *testing.T) {
					shadow := *a
					shadow.state = state{}
					if err := json.Unmarshal(checkpoint, &shadow.state); err != nil {
						t.Fatal(err)
					}
					mutation.change(&shadow.state)
					if err := shadow.verifyGateRepairState(); err == nil {
						t.Fatal("inconsistent supplemental authority accepted during recovery")
					}
				})
			}
			err = a.run()
			var f fixture
			if readErr := readJSON(fixturePath, &f); readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(a.state.GateRepairs[0], before.GateRepairs[0]) || !reflect.DeepEqual(a.state.Merged, before.Merged) || !reflect.DeepEqual(a.state.GateRepairPolicy, before.GateRepairPolicy) || a.state.Session != before.Session || a.state.Turns <= before.Turns {
				t.Fatal("continuation rewrote prior repair, slice history, authority or worker budgets")
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(a.state.Feedback, tc.want) || f.Merges != 2 || a.state.Gate != nil {
					t.Fatalf("additional scope was not enforced: %v %+v", err, a.state)
				}
			} else if err != nil || a.state.Status != "complete" || f.Repairs != 2 || f.Merges != 3 || a.state.Gate.Head != a.state.GateRepairs[1].Merge.MergedCommit || a.state.Gate.Head == before.GateFailure.Head {
				t.Fatalf("additional repair did not complete a fresh gate: %v %+v", err, a.state)
			}
			if after, err := os.ReadFile(a.path); err != nil || string(after) != string(configBytes) {
				t.Fatal("frozen configuration changed")
			}
		})
	}
}

// Invalid approvals must leave the durable checkpoint byte-for-byte unchanged.
func TestAdditionalRepairGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*app, *gateRepairPolicy, *string)
		want   string
	}{
		{"missing failed head", func(_ *app, _ *gateRepairPolicy, head *string) { *head = "" }, "retained --failed-head"},
		{"stale failed head", func(_ *app, _ *gateRepairPolicy, head *string) { *head = strings.Repeat("b", 40) }, "retained --failed-head"},
		{"remaining allowance", func(a *app, _ *gateRepairPolicy, _ *string) { a.state.GateRepairPolicy.MaxRepairs = 2 }, "exhausted authority"},
		{"active repair", func(a *app, _ *gateRepairPolicy, _ *string) {
			a.state.GateRepairs[0].Merge = nil
			a.state.GateRepairs[0].Failure.Head = a.state.GateFailure.Head
		}, "exhausted authority"},
		{"uncertain merge", func(a *app, _ *gateRepairPolicy, _ *string) {
			a.state.Pending = &pendingMerge{PR: 2, Head: a.state.GateFailure.Head}
		}, "no uncertain merge"},
		{"successful gate", func(a *app, _ *gateRepairPolicy, _ *string) {
			a.state.Gate = &evidence{Head: a.state.GateFailure.Head}
			a.state.GateFailure = nil
		}, "no successful gate"},
		{"unsafe scope", func(_ *app, p *gateRepairPolicy, _ *string) { p.AllowedPaths = []string{"../outside"} }, "unsafe gate repair path"},
		{"full gate substitution", func(a *app, p *gateRepairPolicy, _ *string) { p.RequiredCommands = []string{a.config.FullGate} }, "focused"},
		{"total repair ceiling", func(_ *app, p *gateRepairPolicy, _ *string) { p.MaxRepairs = 100 }, "cannot exceed 100"},
		{"dirty primary", func(a *app, _ *gateRepairPolicy, _ *string) {
			if err := os.WriteFile(filepath.Join(a.config.Repo, "dirty.txt"), []byte("uncommitted"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "checkout must be clean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := setupLoop(t, "normal")
			a.config.FullGate = "exit 9"
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			head, err := cleanHead(a.config.Repo)
			if err != nil {
				t.Fatal(err)
			}
			policy := fixtureRepairPolicy()
			policy.MaxRepairs = 1
			a.state.GateRepairPolicy = &policy
			a.state.Next = len(a.config.Slices)
			a.state.Merged = []mergeRecord{{PR: 1, Head: head, MergedCommit: head}}
			a.state.GateRepairs = []gateRepair{{Identity: a.gateRepairIdentity(1), Base: head, Failure: gateFailure{Head: strings.Repeat("a", 40)}, Merge: &mergeRecord{PR: 2, Head: head, MergedCommit: head}}}
			a.state.GateFailure = &gateFailure{Head: head, Message: "second gate failure"}
			additional := fixtureRepairPolicy()
			additional.MaxRepairs = 1
			failedHead := head
			tc.change(a, &additional, &failedHead)
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "additional.json")
			if err := writeJSON(path, additional); err != nil {
				t.Fatal(err)
			}
			if err := authorizeAdditionalGateRepairs(a.path, path, failedHead); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("approval was not rejected at its guard: %v", err)
			}
			if after, err := os.ReadFile(filepath.Join(a.dir, "state.json")); err != nil || string(after) != string(before) {
				t.Fatal("rejected approval mutated checkpoint")
			}
		})
	}
}

// Authorization cannot overlap a controller, worker or an independently owned validation batch.
func TestAdditionalRepairOwnership(t *testing.T) {
	for _, mode := range []string{"controller", "worker", "validator"} {
		t.Run(mode, func(t *testing.T) {
			var a *app
			if mode == "validator" {
				a, _, _, _ = holdingValidation(t)
			} else {
				a, _ = setupLoop(t, "normal")
			}
			want := "validation still owns"
			switch mode {
			case "controller":
				lease, err := acquireRun(a)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.close()
				want = "another controller owns"
			case "worker":
				owner, err := identifyProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Worker: owner}); err != nil {
					t.Fatal(err)
				}
				want = "is still active"
			}
			path := filepath.Join(t.TempDir(), "additional.json")
			if err := writeJSON(path, fixtureRepairPolicy()); err != nil {
				t.Fatal(err)
			}
			if err := authorizeAdditionalGateRepairs(a.path, path, strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("authorization bypassed ownership: %v", err)
			}
		})
	}
}
