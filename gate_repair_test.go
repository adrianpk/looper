package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixtureRepairPolicy() gateRepairPolicy {
	return gateRepairPolicy{BranchPrefix: "fix/gate", Title: "fix(gate): correct acceptance", ReportSlice: 1, MaxRepairs: 2, AllowedPaths: []string{"runtime.txt"}, RequiredCommands: []string{"test -f runtime.txt"}}
}

// Gate failures remain red until a scoped PR is validated, integrated and gated again.
func TestGateRepairCycle(t *testing.T) {
	for _, mode := range []string{"gate-auto-repair", "gate-lost-response", "gate-limit", "gate-blocked", "gate-stalled", "gate-unknown-merge", "gate-unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			a, path := setupLoop(t, mode)
			a.config.FullGate = "test \"$(head -n 1 runtime.txt)\" = repaired"
			if mode == "gate-limit" {
				a.config.FullGate = "exit 9"
			}
			if err := writeJSON(a.path, a.config); err != nil {
				t.Fatal(err)
			}
			var err error
			a, err = loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			policy := fixtureRepairPolicy()
			a.state.GateRepairPolicy = &policy
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			err = a.run()
			var f fixture
			if readErr := readJSON(path, &f); readErr != nil {
				t.Fatal(readErr)
			}
			if a.state.Next != 1 || len(a.state.Merged) != 1 || f.Implement != 2 {
				t.Fatalf("repair changed original delivery: %+v", a.state)
			}
			switch mode {
			case "gate-auto-repair", "gate-lost-response":
				if err != nil || a.state.Status != "complete" || f.Merges != 2 || f.Repairs != 1 || len(a.state.GateRepairs) != 1 || a.state.GateRepairs[0].Merge == nil || !a.state.GateRepairs[0].Merge.Recorded || a.state.GateFailure != nil {
					t.Fatalf("repair did not close: %v %+v", err, a.state)
				}
				if a.state.Gate.Head != a.state.GateRepairs[0].Merge.MergedCommit || a.state.Gate.Head == a.state.GateRepairs[0].Failure.Head {
					t.Fatal("gate evidence reused the failed candidate")
				}
				if err := a.run(); err != nil {
					t.Fatal(err)
				}
			case "gate-limit":
				if err == nil || !strings.Contains(err.Error(), "repair limit") || f.Repairs != 2 || f.Merges != 3 {
					t.Fatalf("repair budget not enforced: %v %+v", err, f)
				}
			case "gate-blocked":
				if err == nil || !strings.Contains(err.Error(), "external decision") || f.Repairs != 1 {
					t.Fatalf("worker blocker lost: %v", err)
				}
			case "gate-stalled":
				if err == nil || !strings.Contains(err.Error(), "observable progress") || f.Repairs != a.config.MaxStalled {
					t.Fatalf("stalled repairs repeated: %v %+v", err, f)
				}
			case "gate-unknown-merge":
				if err == nil || a.state.Pending == nil || !a.state.Pending.GateRepair || f.Merges != 2 {
					t.Fatalf("uncertain repair merge not retained: %v", err)
				}
				loaded, loadErr := loadApp(a.path)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if err := loaded.run(); err == nil {
					t.Fatal("uncertain repair automatically resubmitted")
				}
				if err := readJSON(path, &f); err != nil || f.Merges != 2 {
					t.Fatal("repair mutation duplicated")
				}
			case "gate-unauthorized":
				if err == nil || f.Merges != 1 || !strings.Contains(a.state.Feedback, "unauthorized path: unrelated.txt") {
					t.Fatalf("repair path authority bypassed: %v %+v", err, f)
				}
			}
			if mode != "gate-auto-repair" && mode != "gate-lost-response" && (a.state.Gate != nil || f.Closes != 0) {
				t.Fatal("failed gate allowed closure")
			}
		})
	}
}

// Restarting a failed gate invokes repair first, without replaying the known red gate.
func TestGateRepairResume(t *testing.T) {
	a, path := setupLoop(t, "gate-auto-repair")
	a.config.FullGate = "test \"$(head -n 1 runtime.txt)\" = repaired"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.run(); err == nil || a.state.GateFailure == nil {
		t.Fatal("unapproved repair should block")
	}
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	if err := writeJSON(policyPath, fixtureRepairPolicy()); err != nil {
		t.Fatal(err)
	}
	if err := authorizeGateRepairs(a.path, policyPath, ""); err != nil {
		t.Fatal(err)
	}
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if f.Prepare != 4 || f.Repairs != 1 || f.Merges != 2 || a.state.Status != "complete" {
		t.Fatalf("resume replayed preparation or failed repair: %+v", f)
	}
}

// Optional automation must not prevent an existing run from accepting a manually integrated fix.
func TestManualGateRetry(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	a.config.FullGate = "test \"$(head -n 1 runtime.txt)\" = repaired"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	var err error
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.run(); err == nil {
		t.Fatal("fixture gate should fail")
	}
	if err := os.WriteFile(filepath.Join(a.config.Repo, "runtime.txt"), []byte("repaired\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fixtureCommit(a.config.Repo, "fix: manually correct acceptance", true); err != nil {
		t.Fatal(err)
	}
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	if a.state.Status != "complete" || len(a.state.GateRepairs) != 0 {
		t.Fatal("manual retry changed authorization or failed closure")
	}
}

// Malformed policies cannot expand paths, introduce new slices or replace the gate with a focused check.
func TestGateRepairPolicy(t *testing.T) {
	a, _ := setupLoop(t, "normal")
	for _, tc := range []struct {
		name   string
		change func(*gateRepairPolicy)
	}{
		{"unbounded", func(p *gateRepairPolicy) { p.MaxRepairs = 0 }},
		{"whole repository", func(p *gateRepairPolicy) { p.AllowedPaths = []string{"."} }},
		{"git metadata", func(p *gateRepairPolicy) { p.AllowedPaths = []string{".git/config"} }},
		{"outside repository", func(p *gateRepairPolicy) { p.AllowedPaths = []string{"../elsewhere"} }},
		{"new report slice", func(p *gateRepairPolicy) { p.ReportSlice = 2 }},
		{"full gate replay", func(p *gateRepairPolicy) { p.RequiredCommands = []string{a.config.FullGate} }},
		{"invalid branch", func(p *gateRepairPolicy) { p.BranchPrefix = "fix/../gate" }},
		{"invalid command", func(p *gateRepairPolicy) { p.RequiredCommands = []string{"true\nfalse"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixtureRepairPolicy()
			tc.change(&p)
			if err := a.validateGateRepairPolicy(p); err == nil {
				t.Fatal("unsafe repair policy accepted")
			}
		})
	}
}

// An inactive pre-upgrade run can receive bounded authority without config or history edits.
func TestGateRepairAuthorization(t *testing.T) {
	a, _ := setupLoop(t, "gate-failure")
	if err := a.run(); err == nil {
		t.Fatal("fixture gate should fail")
	}
	a.state.Status = "blocked"
	head := a.state.GateFailure.Head
	a.state.Blocker = &runBlocker{Class: "validation", Message: "Check failed; diagnostics: " + filepath.Join(a.dir, "checks", head+".log")}
	a.state.GateFailure = nil // Legacy checkpoints did not retain structured gate failures.
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before := a.state
	configBytes, err := os.ReadFile(a.path)
	if err != nil {
		t.Fatal(err)
	}
	policy := fixtureRepairPolicy()
	path := filepath.Join(t.TempDir(), "policy.json")
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	if err := authorizeGateRepairs(a.path, path, head); err != nil {
		t.Fatal(err)
	}
	if err := authorizeGateRepairs(a.path, path, head); err != nil {
		t.Fatalf("idempotent approval failed: %v", err)
	}
	a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if a.state.GateFailure == nil || a.state.GateFailure.Head != head || !reflect.DeepEqual(a.state.GateRepairPolicy, &policy) {
		t.Fatal("authority/failure not persisted")
	}
	a.state.GateFailure, a.state.GateRepairPolicy = nil, nil
	if !reflect.DeepEqual(a.state, before) {
		t.Fatal("authorization changed history, budgets or conversation")
	}
	after, err := os.ReadFile(a.path)
	if err != nil || string(after) != string(configBytes) {
		t.Fatal("frozen configuration changed")
	}
	policy.MaxRepairs++
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	if err := authorizeGateRepairs(a.path, path, ""); err == nil {
		t.Fatal("authority widened")
	}
	policy.MaxRepairs--
	if err := writeJSON(path, policy); err != nil {
		t.Fatal(err)
	}
	lease, err := acquireRun(a)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	if err := authorizeGateRepairs(a.path, path, ""); err == nil {
		t.Fatal("authorization bypassed active owner")
	}
	// Optional checkpoint fields must not affect the hash of the original configuration.
	var c config
	if err := json.Unmarshal(configBytes, &c); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if before.ConfigHash != digest(b) {
		t.Fatal("configuration identity drifted")
	}
}
