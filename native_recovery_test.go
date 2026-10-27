package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pinFixtureRecovery(t *testing.T, a *app, policy recoveryPolicy) *app {
	t.Helper()
	path := filepath.Join(t.TempDir(), "recovery-policy.json")
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	if err := authorizeRecovery(a.path, path); err != nil {
		t.Fatal(err)
	}
	return retryCheckpoint(t, a.path)
}

func fixtureRecoveryPolicy() recoveryPolicy { return recoveryPolicy{Version: 1, Profile: "bounded-v1"} }

func setupNativePathRecovery(t *testing.T, exclude bool) (*app, string) {
	t.Helper()
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, "recovery-path")
	a.config.FullGate = `test "$(head -n 1 runtime.txt)" = repaired`
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	policy := fixtureRecoveryPolicy()
	repair := fixtureRepairPolicy()
	repair.MaxRepairs = 0
	repair.AllowedPaths = []string{a.config.Slices[0].Report}
	policy.GateRepair = &repair
	if exclude {
		policy.ExcludedPaths = []string{"runtime.txt"}
	}
	return pinFixtureRecovery(t, a, policy), fixturePath
}

// One owned controller resolves a missing permission and finishes the same run;
// its derived grant cannot replace the explicit policy or consume a new run.
func TestNativePathRecovery(t *testing.T) {
	a, fixturePath := setupNativePathRecovery(t, false)
	original := *a.state.GateRepairPolicy
	if err := runController(a, controllerOptions{Color: colorNever}); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(fixturePath, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Status != "complete" || a.state.Gate == nil || f.Merges != 2 || len(a.state.GateRepairPaths) != 1 || a.state.GateRepairPaths[0].Source == nil || !reflect.DeepEqual(original, *a.state.GateRepairPolicy) || original.MaxRepairs != 8 || a.state.Recovery.Diagnostics != 0 {
		t.Fatalf("native exact-file recovery did not retain authority and finish: %+v", a.state)
	}
	if len(a.state.Recovery.Incidents) != 1 || a.state.Recovery.Incidents[0].Status != "resolved" {
		t.Fatal("resolved incident was not retained")
	}
	if a.state.Turns != f.Calls || a.state.Session == "" {
		t.Fatal("recovery refunded turns or lost its conversation")
	}
}

// A grant persisted before controller interruption is reconciled once; a later
// human grant can also resolve a typed permission decision without policy edits.
func TestRecoveryPathContinuation(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(fmt.Sprint(manual), func(t *testing.T) {
			a, path := setupNativePathRecovery(t, manual)
			if manual {
				err := runController(a, controllerOptions{Color: colorNever})
				var failure *executionFailure
				if !errors.As(err, &failure) || failure.Blocker.Class != "decision" {
					t.Fatalf("excluded path was not held for approval: %v", err)
				}
				a = retryCheckpoint(t, a.path)
				if len(a.state.GateRepairPaths) != 0 {
					t.Fatal("excluded path received automatic authority")
				}
				if err := authorizeGateRepairPaths(a.path, a.activeGateRepair().Base, []string{"runtime.txt"}, "Approve the exact retained fixture file"); err != nil {
					t.Fatal(err)
				}
			} else {
				lease, err := acquireRun(a)
				if err != nil {
					t.Fatal(err)
				}
				a.lease = lease
				err = a.run()
				if err == nil {
					lease.close()
					t.Fatal("fixture must request a path")
				}
				if err := a.newIncident(err); err != nil {
					lease.close()
					t.Fatal(err)
				}
				if resolved, err := a.applyRecoveryRequest(a.activeIncident()); err != nil || !resolved {
					lease.close()
					t.Fatalf("grant was not persisted before interruption: %v", err)
				}
				lease.close() // The incident still says open, just as after a crash following the grant write.
			}
			before := retryCheckpoint(t, a.path).state.Turns
			if err := runController(a, controllerOptions{Color: colorNever}); err != nil {
				t.Fatal(err)
			}
			a = retryCheckpoint(t, a.path)
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			if a.state.Status != "complete" || len(a.state.GateRepairPaths) != 1 || f.Merges != 2 || a.state.Turns <= before || a.state.Turns != f.Calls {
				t.Fatal("continuation duplicated authority or refunded work")
			}
		})
	}
}

// A legacy stall gets a charged diagnostic rather than silently exiting or
// asking a human to restart; an actual decision is retained as a distinct stop.
func TestNativeDiagnosis(t *testing.T) {
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, "recovery-stalled")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	err := runController(a, controllerOptions{Color: colorNever})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Class != "decision" {
		t.Fatalf("missing decision outcome: %v", err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(fixturePath, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Recovery.Diagnostics != 1 || f.Calls != 6 || a.state.Turns != 6 || a.state.Stalled != 5 || a.activeIncident().Status != "needs_decision" {
		t.Fatalf("diagnosis reset accounting: %+v", a.state)
	}
	entry, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	if info := inspectRun(entry); info.Status != "needs_decision" || info.Recovery == nil || info.Recovery.Diagnostics != 1 {
		t.Fatal("status hid a recovery decision behind ordinary stalls")
	}
	if err := runController(a, controllerOptions{Color: colorNever}); err == nil {
		t.Fatal("resume bypassed a retained decision")
	}
	a = retryCheckpoint(t, a.path)
	if a.state.Turns != 6 {
		t.Fatal("a decision stop launched another worker")
	}
}

// A temporary-failure exit is an explicit check contract, unlike an arbitrary
// failing assertion. Both retry count and the original failed batch survive.
func TestNativeGateRetry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		always bool
	}{{"resolved", false}, {"repeated", true}} {
		t.Run(tc.name, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "normal")
			marker := filepath.Join(t.TempDir(), "temporary-cause-resolved")
			a.config.FullGate = "if test -f " + shellQuote(marker) + "; then test -f runtime.txt; else touch " + shellQuote(marker) + "; exit 75; fi"
			if tc.always {
				a.config.FullGate = "exit 75"
			}
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			a = pinFixtureRecovery(t, retryCheckpoint(t, a.path), fixtureRecoveryPolicy())
			err := runController(a, controllerOptions{Color: colorNever})
			if (err != nil) != tc.always {
				t.Fatalf("environmental outcome: %v", err)
			}
			a = retryCheckpoint(t, a.path)
			if a.state.Recovery.GateRetries != 1 || len(a.state.GateRetries) != 1 || !a.state.GateRetries[0].Automatic || a.state.GateRetries[0].Result == nil {
				t.Fatal("automatic retry lost its reservation or outcome")
			}
			if tc.always && a.state.Gate != nil {
				t.Fatal("failed retry manufactured passing gate evidence")
			}
		})
	}
}

// The producer wrapper proves attribution through exclusive allocation and an
// inherited descriptor. Cleanup must preserve foreign and changed content.
func TestNativeOutputRecovery(t *testing.T) {
	for _, mode := range []string{"owned", "foreign", "modified"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "gate-auto-repair")
			producer := "printf 'owned output' >&3"
			generate := "looper generate-output --config " + shellQuote(a.path) + " --path generated.bin --command " + shellQuote(producer)
			suffix := ""
			if mode == "foreign" {
				suffix = "; printf 'user work' > user.txt"
			}
			if mode == "modified" {
				suffix = "; printf 'changed' >> generated.bin"
			}
			a.config.FullGate = "if test \"$(head -n 1 runtime.txt)\" = repaired; then exit 0; fi; " + generate + suffix + "; exit 9"
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			a = retryCheckpoint(t, a.path)
			policy := fixtureRecoveryPolicy()
			repair := fixtureRepairPolicy()
			policy.GateRepair, policy.Outputs = &repair, []outputPolicy{{Path: "generated.bin", Command: producer}}
			a = pinFixtureRecovery(t, a, policy)
			err := runController(a, controllerOptions{Color: colorNever})
			if (err != nil) != (mode != "owned") {
				t.Fatalf("quarantine outcome: %v", err)
			}
			if mode == "foreign" {
				b, err := os.ReadFile(filepath.Join(a.config.Repo, "user.txt"))
				if err != nil || string(b) != "user work" {
					t.Fatal("foreign content was removed or staged")
				}
			}
			if mode == "modified" {
				b, err := os.ReadFile(filepath.Join(a.config.Repo, "generated.bin"))
				if err != nil || string(b) != "owned outputchanged" {
					t.Fatal("changed output was moved or discarded")
				}
			} else {
				if _, err := os.Stat(filepath.Join(a.config.Repo, "generated.bin")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("proven output still blocks cleanliness")
				}
				files, err := filepath.Glob(filepath.Join(recoveryMetadataPath(a), "quarantine", "*", "payload"))
				if err != nil || len(files) != 1 {
					t.Fatal("attributed output was not preserved in quarantine")
				}
				b, err := os.ReadFile(files[0])
				if err != nil || string(b) != "owned output" {
					t.Fatal("quarantine lost output content")
				}
			}
		})
	}
}

// Failure-family attempts survive different candidate commits and checkpoint
// reloads. Changing the candidate must not create a fresh retry allowance.
func TestRecoveryRecurrence(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	command := "printf 'LOOPER_FAILURE_FAMILY=fixture-case\\n'; exit 9"
	for n := 1; n <= 3; n++ {
		if err := os.WriteFile(filepath.Join(a.config.Repo, "candidate.txt"), []byte(fmt.Sprint(n)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := fixtureCommit(a.config.Repo, "fix: attempt fixture repair", false); err != nil {
			t.Fatal(err)
		}
		if err := a.validate(a.config.Repo, []string{command}); err == nil {
			t.Fatal("fixture must fail")
		}
		a.replyPhase = "repair"
		err := a.observeRecoveryValidation()
		if (err != nil) != (n == 3) {
			t.Fatalf("attempt %d: %v", n, err)
		}
		a = retryCheckpoint(t, a.path)
	}
	if len(a.state.Recovery.Families) != 1 || a.state.Recovery.Families[0].FailedRepairs != 3 {
		t.Fatal("candidate churn reset recurrence")
	}
}

// Delegation is opt-in and idempotent, does not refund consumed turns, and
// cannot be adopted through another live owner or replace existing authority.
func TestRecoveryDelegation(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	a.state.Turns, a.state.Stalled = 7, 3
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	policy := fixtureRecoveryPolicy()
	a = pinFixtureRecovery(t, a, policy)
	if a.state.Turns != 7 || a.state.Stalled != 3 {
		t.Fatal("delegation reset consumed counters")
	}
	before := a.state
	a = pinFixtureRecovery(t, a, policy)
	if !reflect.DeepEqual(before, a.state) {
		t.Fatal("replayed delegation changed accounting")
	}
	lease, err := acquireRun(a)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	err = authorizeRecovery(a.path, path)
	lease.close()
	if err == nil {
		t.Fatal("live owner admitted another authorizer")
	}
	policy.DiagnosticsPerRun = 9
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	if err := authorizeRecovery(a.path, path); err == nil || !strings.Contains(err.Error(), "already pinned") {
		t.Fatal("delegation was replaced or widened")
	}
}

// Invalid diagnostic results have their own finite allowance even after an
// ordinary stall. Reinvocation and checkpoint reload cannot reset it.
func TestRecoveryDiagnosticLimit(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "recovery-stalled-malformed")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	err := runController(a, controllerOptions{Color: colorNever})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Code != "recovery_diagnostics_exhausted" {
		t.Fatalf("diagnostic bound: %v", err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Recovery.Diagnostics != 2 || a.state.Turns != 7 || f.Calls != 7 || a.state.Stalled != 5 {
		t.Fatal("invalid results bypassed a hard bound")
	}
	if err := runController(a, controllerOptions{Color: colorNever}); err == nil {
		t.Fatal("exhausted diagnostic allowance resumed")
	}
	if next := retryCheckpoint(t, a.path); next.state.Turns != 7 {
		t.Fatal("resume minted a diagnostic launch")
	}
}

// A reserved diagnostic with an unknown outcome is inspected rather than
// automatically repeated, even when the old child process is no longer alive.
func TestRecoveryInterruptedAction(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	a.state.Session = "11111111-1111-4111-8111-111111111111"
	if err := a.newIncident(&recoveryCondition{Code: "worker_blocked", Phase: "implement", Message: "Retained technical blocker"}); err != nil {
		t.Fatal(err)
	}
	i := a.activeIncident()
	i.Action, i.ActionID, i.Status = "diagnose", 1, "reserved"
	if err := a.reserveDiagnostic(); err != nil {
		t.Fatal(err)
	}
	a.state.Turns++
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	err := runController(a, controllerOptions{Color: colorNever})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Code != "recovery_interrupted" {
		t.Fatalf("uncertain diagnostic replayed: %v", err)
	}
	a = retryCheckpoint(t, a.path)
	if a.state.Turns != 1 || a.state.Recovery.Diagnostics != 1 {
		t.Fatal("interruption refunded a reserved launch")
	}
	if err := runController(a, controllerOptions{Color: colorNever}); err == nil {
		t.Fatal("later resume replayed the uncertain action")
	}
	if next := retryCheckpoint(t, a.path); next.state.Turns != 1 {
		t.Fatal("uncertain action was launched twice")
	}
}

// Successful commands that dirty their candidate still need a fresh batch.
// Proven cleanup permits only the delegated same-candidate retry, never a loop.
func TestNativeDirtyGateRetry(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		t.Run(fmt.Sprint(persistent), func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "normal")
			producer := "printf fixture >&3"
			generate := "looper generate-output --config " + shellQuote(a.path) + " --path generated.bin --command " + shellQuote(producer)
			a.config.FullGate = generate
			if !persistent {
				marker := filepath.Join(t.TempDir(), "output-resolved")
				a.config.FullGate = "if ! test -f " + shellQuote(marker) + "; then " + generate + "; touch " + shellQuote(marker) + "; fi"
			}
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			policy := fixtureRecoveryPolicy()
			policy.Outputs = []outputPolicy{{Path: "generated.bin", Command: producer}}
			a = pinFixtureRecovery(t, retryCheckpoint(t, a.path), policy)
			err := runController(a, controllerOptions{Color: colorNever})
			if (err != nil) != persistent {
				t.Fatalf("dirty-candidate retry: %v", err)
			}
			a = retryCheckpoint(t, a.path)
			if a.state.Recovery.GateRetries != 1 || len(a.state.GateRetries) != 1 || a.state.GateRetries[0].Result == nil {
				t.Fatal("cleanup bypassed retry accounting")
			}
			if persistent && (a.state.Gate != nil || a.state.GateRetries[0].Result.Status != "failed") {
				t.Fatal("dirty retry became a pass or lost its terminal failure")
			}
		})
	}
}

// Other worktrees and unrelated primary files cannot conceal a stalled worker.
func TestRecoveryProgressScope(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	before, err := a.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	if _, err := git(a.config.Repo, "worktree", "add", "-b", "feat/other", other); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(other, "unrelated.txt"), filepath.Join(a.config.Repo, "foreign.txt")} {
		if err := os.WriteFile(path, []byte("foreign work\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	after, err := a.fingerprint()
	if err != nil || after != before {
		t.Fatal("unrelated activity became recovery progress")
	}
}

// Independent acceptance-fixture defects can consume an adequate upfront
// allowance beyond the old three-repair cap without receiving runtime grants.
func TestNativeFiveRepairs(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "recovery-five")
	a.config.FullGate = "for n in 1 2 3 4 5; do if ! test -f checks/part-$n; then printf 'LOOPER_FAILURE_FAMILY=part-%s\\n' \"$n\"; exit 9; fi; done"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	policy := fixtureRecoveryPolicy()
	repair := fixtureRepairPolicy()
	repair.MaxRepairs = 0
	repair.AllowedPaths = []string{"runtime.txt", "checks"}
	policy.GateRepair = &repair
	a = pinFixtureRecovery(t, a, policy)
	if err := runController(a, controllerOptions{Color: colorNever}); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Status != "complete" || len(a.state.GateRepairs) != 5 || f.Merges != 6 || len(a.state.GateRepairAuthorizations) != 0 || a.state.Turns != 20 || a.config.MaxTurns != 20 {
		t.Fatalf("upfront repair authority did not retain its independent budgets: %+v", a.state)
	}
}

// Diagnostics repair the approved unit, supply real exact-HEAD evidence and
// hand control back to canonical integration without an external resume.
func TestNativeDiagnosticRepair(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "recovery-diagnose")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	if err := runController(a, controllerOptions{Color: colorNever}); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Status != "complete" || a.state.Recovery.Diagnostics != 2 || a.state.Turns != 6 || f.Calls != 6 || f.Merges != 1 || len(a.state.Recovery.Incidents) != 1 || a.state.Recovery.Incidents[0].Status != "resolved" {
		t.Fatalf("diagnosis did not repair and continue the same run: %+v", a.state)
	}
}

// Delegated recovery cannot resubmit an uncertain merge on either its first
// failure or a later explicit resume.
func TestRecoveryMergeAmbiguity(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "unknown-merge")
	a = pinFixtureRecovery(t, a, fixtureRecoveryPolicy())
	for n := 0; n < 2; n++ {
		if err := runController(a, controllerOptions{Color: colorNever}); err == nil {
			t.Fatal("unconfirmed merge accepted")
		}
		a = retryCheckpoint(t, a.path)
		var f fixture
		if err := readJSON(path, &f); err != nil {
			t.Fatal(err)
		}
		if a.state.Pending == nil || f.Merges != 1 || a.state.Recovery.Diagnostics != 0 {
			t.Fatal("recovery replayed an ambiguous mutation")
		}
	}
}

// Restoring identical bytes after an intervening foreign edit is insufficient
// provenance. The delivery record must remain the last attributable change.
func TestRecoveryPathProvenance(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	a.config.FullGate = "exit 9"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	if err := a.run(); err == nil || len(a.state.Merged) != 1 {
		t.Fatal("fixture must integrate and retain a failed gate")
	}
	head, err := cleanHead(a.config.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.repairPathSource(head, "runtime.txt"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(a.config.Repo, "runtime.txt")
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]byte{[]byte("foreign edit\n"), original} {
		if err := os.WriteFile(file, content, 0600); err != nil {
			t.Fatal(err)
		}
		if err := fixtureCommit(a.config.Repo, "fix: external fixture change", false); err != nil {
			t.Fatal(err)
		}
	}
	head, err = cleanHead(a.config.Repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.repairPathSource(head, "runtime.txt"); err == nil || !strings.Contains(err.Error(), "intervening") {
		t.Fatalf("foreign history became automatic authority: %v", err)
	}
	if _, err := a.repairPathSource(head, "ops/default/plan/flow.md"); err == nil {
		t.Fatal("baseline file became delivery-produced authority")
	}
}
