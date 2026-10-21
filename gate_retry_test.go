package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A host-only correction must retry the frozen gate without buying another code repair.
func TestGateRetryCycle(t *testing.T) {
	for _, corrected := range []bool{true, false} {
		name := "environment restored"
		if !corrected {
			name = "environment still broken"
		}
		t.Run(name, func(t *testing.T) {
			a, fixturePath := setupLoop(t, "gate-auto-repair")
			marker := filepath.Join(t.TempDir(), "available")
			a.config.FullGate = "test -f " + shellQuote(marker)
			a.config.MaxTurns = 40
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
				t.Fatalf("expected exhausted code repair authority: %v", err)
			}
			before := a.state
			configBytes, err := os.ReadFile(a.path)
			if err != nil {
				t.Fatal(err)
			}
			v, err := a.gateRetryValidation(before.GateFailure.Head)
			if err != nil {
				t.Fatal(err)
			}
			oldLog, err := os.ReadFile(v.Log)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"retry-gate", "--config", a.path, "--failed-head", before.GateFailure.Head, "--reason", "Restore the owned fixture environment"}
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
			if after, err := os.ReadFile(filepath.Join(a.dir, "state.json")); err != nil || string(after) != string(checkpoint) {
				t.Fatal("repeated approval changed the checkpoint")
			}
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			authorized := a.state
			authorized.GateRetries = nil
			if !reflect.DeepEqual(authorized, before) {
				t.Fatal("authorization rewrote configuration, history or budgets")
			}
			if corrected {
				if err := os.WriteFile(marker, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = a.run()
			var f fixture
			if readErr := readJSON(fixturePath, &f); readErr != nil {
				t.Fatal(readErr)
			}
			if !reflect.DeepEqual(a.state.Merged, before.Merged) || !reflect.DeepEqual(a.state.GateRepairs, before.GateRepairs) || !reflect.DeepEqual(a.state.GateRepairPolicy, before.GateRepairPolicy) || a.state.Session != before.Session || a.state.Turns <= before.Turns || f.Merges != 2 || f.Repairs != 1 {
				t.Fatal("retry replaced history, reset budgets or created another repair")
			}
			retry := a.state.GateRetries[0]
			if retry.Result == nil || retry.Result.ValidationID == v.ID || retry.Failure != *before.GateFailure {
				t.Fatal("retry did not retain its source failure and new outcome")
			}
			if corrected {
				if err != nil || a.state.Status != "complete" || a.state.Gate.Head != before.GateFailure.Head || retry.Result.Status != "passed" {
					t.Fatalf("same candidate did not complete: %v %+v", err, a.state)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "repair limit") || a.state.Gate != nil || retry.Result.Status != "failed" {
					t.Fatalf("failed retry bypassed authority: %v %+v", err, a.state)
				}
				outcome := retry.Result.ValidationID
				if err := a.run(); err == nil || !strings.Contains(err.Error(), "repair limit") {
					t.Fatalf("consumed retry executed again: %v", err)
				}
				v, err := a.gateRetryValidation(before.GateFailure.Head)
				if err != nil || v.ID != outcome {
					t.Fatalf("resume reran an unapproved gate: %v", err)
				}
			}
			if after, err := os.ReadFile(a.path); err != nil || string(after) != string(configBytes) {
				t.Fatal("frozen configuration changed")
			}
			if after, err := os.ReadFile(v.Log); err != nil || string(after) != string(oldLog) {
				t.Fatal("original failure diagnostics changed")
			}
		})
	}
}

func failedGateFixture(t *testing.T) (*app, string) {
	t.Helper()
	a, _ := setupLoop(t, "normal")
	marker := filepath.Join(t.TempDir(), "available")
	a.config.FullGate = "test -f " + shellQuote(marker)
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
	if err := a.validate(a.config.Repo, []string{a.config.FullGate}); err == nil {
		t.Fatal("fixture gate unexpectedly passed")
	} else {
		a.state.GateFailure = &gateFailure{Head: head, Message: err.Error()}
	}
	a.state.Next = len(a.config.Slices)
	a.state.Merged = []mergeRecord{{PR: 1, Head: head, MergedCommit: head}}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	return a, marker
}

// Reject stale candidates, substituted checks and ambiguous ownership without changing state.
func TestGateRetryGuards(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *app, *string, *string)
	}{
		{"missing head", func(_ *testing.T, _ *app, head, _ *string) { *head = "" }},
		{"stale head", func(_ *testing.T, _ *app, head, _ *string) { *head = strings.Repeat("b", 40) }},
		{"missing reason", func(_ *testing.T, _ *app, _, reason *string) { *reason = "" }},
		{"multiline reason", func(_ *testing.T, _ *app, _, reason *string) { *reason = "fixed\nagain" }},
		{"oversized reason", func(_ *testing.T, _ *app, _, reason *string) { *reason = strings.Repeat("a", 513) }},
		{"dirty candidate", func(t *testing.T, a *app, _, _ *string) {
			if err := os.WriteFile(filepath.Join(a.config.Repo, "dirty"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"uncertain merge", func(_ *testing.T, a *app, _, _ *string) {
			a.state.Pending = &pendingMerge{PR: 1, Head: a.state.GateFailure.Head}
		}},
		{"successful gate", func(_ *testing.T, a *app, _, _ *string) { a.state.Gate = &evidence{Head: a.state.GateFailure.Head} }},
		{"replaced validation", func(t *testing.T, a *app, _, _ *string) {
			if err := a.validate(a.config.Repo, []string{"true"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"missing diagnostics", func(t *testing.T, a *app, _, _ *string) {
			v, err := a.gateRetryValidation(a.state.GateFailure.Head)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(v.Log); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := failedGateFixture(t)
			head, reason := a.state.GateFailure.Head, "Restore fixture access"
			tc.change(t, a, &head, &reason)
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := authorizeGateRetry(a.path, head, reason); err == nil {
				t.Fatal("invalid retry was authorized")
			}
			if after, err := os.ReadFile(filepath.Join(a.dir, "state.json")); err != nil || string(after) != string(before) {
				t.Fatal("rejected retry changed checkpoint")
			}
		})
	}
}

// A controller restart must reconcile a completed retry instead of executing it twice.
func TestGateRetryRecovery(t *testing.T) {
	for _, outcome := range []string{"passed", "failed", "missing receipt", "changed candidate"} {
		t.Run(outcome, func(t *testing.T) {
			a, marker := failedGateFixture(t)
			head := a.state.GateFailure.Head
			if err := authorizeGateRetry(a.path, head, "Restore fixture access"); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.reconcileGateRetry(); err != nil {
				t.Fatal(err)
			}
			if outcome == "changed candidate" {
				if err := os.WriteFile(filepath.Join(a.config.Repo, "changed.txt"), []byte("another candidate\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := fixtureCommit(a.config.Repo, "docs: change candidate", true); err != nil {
					t.Fatal(err)
				}
			} else {
				if outcome != "failed" {
					if err := os.WriteFile(marker, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := a.validate(a.config.Repo, []string{a.config.FullGate}); (err != nil) != (outcome == "failed") {
					t.Fatalf("unexpected validator result: %v", err)
				}
				if outcome == "missing receipt" {
					if err := os.Remove(a.evidencePath(head)); err != nil {
						t.Fatal(err)
					}
				}
			}
			// Reload exactly as a new controller would after losing the checkpoint update.
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			err = a.reconcileGateRetry()
			if outcome == "missing receipt" || outcome == "changed candidate" {
				if err == nil || a.pendingGateRetry() == nil || a.state.Gate != nil {
					t.Fatalf("unproven recovery consumed or satisfied retry: %v", err)
				}
			} else if err != nil || a.pendingGateRetry() != nil || a.state.GateRetries[0].Result.Status != outcome || (a.state.Gate != nil) != (outcome == "passed") || (a.state.GateFailure != nil) != (outcome == "failed") {
				t.Fatalf("terminal retry was not reconciled: %v %+v", err, a.state)
			}
		})
	}
}

// Operator approval cannot overlap a controller, worker or independently owned validator.
func TestGateRetryOwnership(t *testing.T) {
	for _, owner := range []string{"controller", "worker", "validator"} {
		t.Run(owner, func(t *testing.T) {
			var a *app
			if owner == "validator" {
				a, _, _, _ = holdingValidation(t)
			} else {
				a, _ = setupLoop(t, "normal")
			}
			want := "validation still owns"
			switch owner {
			case "controller":
				lease, err := acquireRun(a)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.close()
				want = "another controller owns"
			case "worker":
				identity, err := identifyProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Worker: identity}); err != nil {
					t.Fatal(err)
				}
				want = "is still active"
			}
			if err := authorizeGateRetry(a.path, strings.Repeat("a", 40), "Restore fixture access"); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("retry bypassed ownership: %v", err)
			}
		})
	}
}
