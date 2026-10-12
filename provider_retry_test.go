package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Error classification must consume stdout JSON even when activity display is disabled.
func TestWorkerJSONFailure(t *testing.T) {
	for _, tc := range []struct{ mode, code string }{
		{"capacity-always", "provider_capacity"},
		{"authentication-json", "worker_authentication"},
		{"unknown-json", "worker_execution"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			a, _ := setupLoop(t, tc.mode)
			err := a.invokeWorker("implement", "Prepare the approved slice")
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Code != tc.code {
				t.Fatalf("stdout error classification: %v", err)
			}
			if a.state.Session == "" || strings.Contains(err.Error(), "private-value") {
				t.Fatal("conversation lost or raw diagnostics retained")
			}
		})
	}
}

type retryTestClock struct {
	now   time.Time
	waits []time.Time
	hook  func(time.Time) error
}

func newRetryClock() *retryTestClock {
	return &retryTestClock{now: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
}

func (c *retryTestClock) Now() time.Time { return c.now }
func (c *retryTestClock) WaitUntil(at time.Time) error {
	c.waits = append(c.waits, at)
	if c.hook != nil {
		if err := c.hook(at); err != nil {
			return err
		}
	}
	c.now = at
	return nil
}

func retryCheckpoint(t *testing.T, path string) *app {
	t.Helper()
	a, err := loadApp(path)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Exact protocol messages and explicit codes prevent quota, authentication and unknown errors from retrying.
func TestCapacityClassification(t *testing.T) {
	for _, tc := range []struct{ name, raw, want string }{
		{"installed shape", `{"type":"turn.failed","error":{"message":"unexpected status 503 Service Unavailable: Selected model is at capacity. Please try a different model., url: https://provider.example/responses?token=private-value"}}`, capacityReason},
		{"explicit capacity", `{"type":"turn.failed","error":{"code":"server_overloaded","message":"private-value"}}`, capacityReason},
		{"code precedence", `{"type":"error","code":"unknown","message":"Selected model is at capacity. Please try a different model."}`, "worker_execution"},
		{"quota", `{"type":"error","message":"Quota exceeded"}`, "worker_execution"},
		{"generic overload", `{"type":"error","message":"503 Service Unavailable"}`, "worker_execution"},
		{"capacity mentioned", `{"type":"error","message":"The log says: Selected model is at capacity. Please try a different model."}`, "worker_execution"},
		{"authentication", `{"type":"turn.failed","error":{"message":"401 Unauthorized token=private-value"}}`, "worker_authentication"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e workerEvent
			if err := json.Unmarshal([]byte(tc.raw), &e); err != nil {
				t.Fatal(err)
			}
			failure := e.failure()
			if failure.Blocker.Code != tc.want || strings.Contains(failure.Error(), "private-value") {
				t.Fatalf("classification: %+v", failure)
			}
		})
	}
}

// Real child processes and Git worktrees recover the same phase without duplicating delivery or gate repairs.
func TestCapacityRecovery(t *testing.T) {
	for _, mode := range []string{"capacity-once", "capacity-partial", "capacity-close", "capacity-gate"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, path := setupLoop(t, mode)
			if mode == "capacity-gate" {
				a.config.FullGate = `test "$(head -n 1 runtime.txt)" = repaired`
				if err := writeJSON(a.path, a.config); err != nil {
					t.Fatal(err)
				}
				a = retryCheckpoint(t, a.path)
				if err := a.run(); err == nil || a.state.GateFailure == nil {
					t.Fatal("fixture must first retain the failed full gate")
				}
				policy := fixtureRepairPolicy()
				a.state.GateRepairPolicy = &policy
				if err := a.save(); err != nil {
					t.Fatal(err)
				}
			}
			clock := newRetryClock()
			var retained state
			var partialHead string
			clock.hook = func(at time.Time) error {
				checkpoint := retryCheckpoint(t, a.path)
				retained = checkpoint.state
				if retained.ProviderRetry == nil || retained.ProviderRetry.NextAt == nil || !retained.ProviderRetry.NextAt.Equal(at) || retained.ProviderRetry.Attempts != 0 || retained.Session == "" {
					t.Fatal("retry intent was not persisted before waiting")
				}
				if err := requireIdleWorker(checkpoint); err != nil {
					t.Fatal(err)
				}
				if duplicate, err := acquireRun(checkpoint); err == nil {
					duplicate.close()
					t.Fatal("waiting controller released ownership")
				}
				if mode == "capacity-partial" {
					var f fixture
					if err := readJSON(path, &f); err != nil {
						t.Fatal(err)
					}
					var err error
					partialHead, err = git(f.Worktree, "rev-parse", "HEAD")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Join(f.Worktree, "runtime.txt")); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}
			if err := runController(a, controllerOptions{RetryClock: clock, Output: outputJSONL}); err != nil {
				t.Fatal(err)
			}
			a = retryCheckpoint(t, a.path)
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			merges := 1
			if mode == "capacity-gate" {
				merges = 2
			}
			if a.state.Status != "complete" || a.state.ProviderRetry.Attempts != 1 || a.state.ProviderRetry.Phase != "" || a.state.Turns != f.Calls || f.Merges != merges || len(clock.waits) != 1 || a.state.Gate == nil || a.state.Session != retained.Session {
				t.Fatalf("recovery lost progress: %+v %+v", a.state, f)
			}
			for i, session := range f.WorkerSessions {
				if i > 0 && session != a.state.Session {
					t.Fatal("retry launched an unrelated conversation")
				}
			}
			if mode == "capacity-partial" {
				if f.Implement != 2 {
					t.Fatal("partial implementation was replayed")
				}
				if _, err := git(f.Worktree, "merge-base", "--is-ancestor", partialHead, "HEAD"); err != nil {
					t.Fatal("partial commit discarded", err)
				}
			}
			if mode == "capacity-close" && (!reflect.DeepEqual(retained.Gate, a.state.Gate) || !reflect.DeepEqual(retained.Merged, a.state.Merged)) {
				t.Fatal("closure retry changed validation or merge evidence")
			}
			if mode == "capacity-gate" && (len(a.state.GateRepairs) != 1 || !reflect.DeepEqual(a.state.GateRepairs[0].Identity, retained.GateRepairs[0].Identity)) {
				t.Fatal("capacity allocated another gate-repair attempt")
			}
		})
	}
}

// Restarting a controller during a wait retains its deadline, retry allowance, spent turns and conversation.
func TestCapacityRestart(t *testing.T) {
	for _, stopAt := range []int{1, 3} {
		t.Run(string(rune('0'+stopAt)), func(t *testing.T) {
			recoveryEnvironment(t)
			a, path := setupLoop(t, "capacity-always")
			clock := newRetryClock()
			interrupted := errors.New("fixture controller interruption")
			clock.hook = func(time.Time) error {
				if len(clock.waits) == stopAt {
					return interrupted
				}
				return nil
			}
			if err := runController(a, controllerOptions{RetryClock: clock}); !errors.Is(err, interrupted) {
				t.Fatal(err)
			}
			saved := retryCheckpoint(t, a.path)
			count, turns, session, at := saved.state.ProviderRetry.Attempts, saved.state.Turns, saved.state.Session, *saved.state.ProviderRetry.NextAt
			if count != stopAt-1 || turns != stopAt {
				t.Fatal("interruption refunded work")
			}
			clock.now = clock.now.Add(30 * time.Second)
			clock.hook = func(deadline time.Time) error {
				if len(clock.waits) == stopAt+1 && !deadline.Equal(at) {
					t.Fatal("restart reset the waiting deadline")
				}
				return nil
			}
			err := runController(saved, controllerOptions{RetryClock: clock})
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Code != "provider_capacity_exhausted" {
				t.Fatal(err)
			}
			a = retryCheckpoint(t, a.path)
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			if a.state.ProviderRetry.Attempts != 5 || a.state.ProviderRetry.NextAt != nil || a.state.Turns != 6 || f.Calls != 6 || f.Merges != 0 || a.state.Session != session || a.state.Blocker.NextAction == nil || !reflect.DeepEqual(a.state.Blocker.NextAction.Argv, []string{"looper", "resume", "--config", a.path, "--detach", "--json"}) {
				t.Fatal("retry exhaustion lost its budget or recovery command")
			}
			// An explicit manual attempt is allowed, but does not replenish automatic retries.
			waits := len(clock.waits)
			if err := runController(a, controllerOptions{RetryClock: clock}); !errors.As(err, &failure) || failure.Blocker.Code != "provider_capacity_exhausted" {
				t.Fatal(err)
			}
			a = retryCheckpoint(t, a.path)
			if a.state.Turns != 7 || a.state.ProviderRetry.Attempts != 5 || len(clock.waits) != waits {
				t.Fatal("manual resume reset automatic allowance")
			}
		})
	}
}

// The complete policy has five waits; worker, progress and model selection limits remain unchanged.
func TestCapacityLimits(t *testing.T) {
	for _, mode := range []string{"capacity-always", "unknown-json", "authentication-json", "turn limit"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			fixtureMode := mode
			if mode == "turn limit" {
				fixtureMode = "capacity-always"
			}
			a, path := setupLoop(t, fixtureMode)
			if mode == "turn limit" {
				a.config.MaxTurns = 2
				if err := writeJSON(a.path, a.config); err != nil {
					t.Fatal(err)
				}
				a = retryCheckpoint(t, a.path)
			}
			clock := newRetryClock()
			start := clock.now
			err := runController(a, controllerOptions{RetryClock: clock})
			if err == nil {
				t.Fatal("failed worker completed")
			}
			a = retryCheckpoint(t, a.path)
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			if f.Merges != 0 || a.state.Turns != f.Calls || a.state.Stalled != 0 {
				t.Fatal("failure changed progress or repeated a merge")
			}
			switch mode {
			case "capacity-always":
				want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute}
				if len(clock.waits) != len(want) {
					t.Fatal("retry policy not bounded")
				}
				for i, at := range clock.waits {
					if at.Sub(start) != want[i] {
						t.Fatal("wrong backoff")
					}
					start = at
				}
			case "turn limit":
				if a.state.Turns != 2 || a.state.ProviderRetry.Attempts != 1 || len(clock.waits) != 1 {
					t.Fatal("capacity bypassed the worker turn limit")
				}
			default:
				if f.Calls != 1 || len(clock.waits) != 0 || a.state.ProviderRetry != nil {
					t.Fatal("non-capacity failure was automatically retried")
				}
			}
		})
	}
}

// Live watch output and both status formats expose the same safe retry reason, number and absolute deadline.
func TestCapacityStatus(t *testing.T) {
	for _, format := range []outputFormat{outputHuman, outputJSONL} {
		t.Run(string(format), func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "capacity-once")
			entry, err := registerRun(a)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := acquireRun(a)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			a.lease = lease
			var out bytes.Buffer
			a.output = newStreamOutput(&out, colorNever, format, entry.ID)
			clock := newRetryClock()
			a.retryClock = clock
			clock.hook = func(at time.Time) error {
				r, code, diagnostic := recoveryCLI("status", "--run", entry.ID, "--json")
				if code != 0 || r.Run == nil || r.Run.Status != "waiting" || r.Run.ProviderRetry.NextRetry != 1 || !r.Run.ProviderRetry.NextAt.Equal(at) {
					t.Fatalf("JSON status: %+v %s", r, diagnostic)
				}
				human, err := exec.Command("looper", "status", "--run", entry.ID, "--color", "never").CombinedOutput()
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(string(human), "waiting:") {
					t.Fatalf("human status hid the run state: %s", human)
				}
				for _, text := range []string{capacityReason, "automatic retry 1/5", at.Format(time.RFC3339), "phase implement"} {
					if !strings.Contains(string(human), text) || !strings.Contains(out.String(), text) {
						t.Fatalf("status/watch lost %q: %s %s", text, human, out.String())
					}
				}
				if len(activeRuns([]runInfo{*r.Run})) != 1 {
					t.Fatal("waiting run cannot be watched")
				}
				return nil
			}
			if err := a.work("implement", "Prepare the approved slice"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Only the fixture controller is killed, after its worker has exited and its retry deadline is durable.
func TestCapacityControllerCrash(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "capacity-once")
	controller := exec.Command("looper", "resume", "--config", a.path, "--output", "jsonl")
	controller.Stdout, controller.Stderr = io.Discard, io.Discard
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = controller.Process.Kill()
			_ = controller.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		loaded, err := loadApp(a.path)
		if err == nil && loaded.state.Status == "waiting" && loaded.state.ProviderRetry != nil && loaded.state.ProviderRetry.NextAt != nil {
			a = loaded
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture controller did not enter retry waiting")
		}
		time.Sleep(20 * time.Millisecond)
	}
	at := *a.state.ProviderRetry.NextAt
	if err := requireIdleWorker(a); err != nil {
		t.Fatal("worker still active before fixture interruption", err)
	}
	entry, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	response, code, diagnostic := recoveryCLI("resume", "--run", entry.ID, "--detach", "--json")
	if code != 0 || response.Status != "already_running" {
		t.Fatalf("concurrent resume: %+v %s", response, diagnostic)
	}
	if err := controller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = controller.Wait()
	waited = true
	a = retryCheckpoint(t, a.path)
	if r := inspectRun(entry); r.Status != "interrupted" || r.ProviderRetry.NextRetry != 1 || !r.ProviderRetry.NextAt.Equal(at) {
		t.Fatalf("crash discarded retry intent: %+v", r)
	}
	clock := newRetryClock()
	clock.now = at.Add(-30 * time.Second)
	clock.hook = func(next time.Time) error {
		if !next.Equal(at) {
			t.Fatal("crash reset retry deadline")
		}
		return nil
	}
	if err := runController(a, controllerOptions{RetryClock: clock, Output: outputJSONL}); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if a.state.Status != "complete" || f.Calls != 6 || f.Merges != 1 || a.state.Turns != 6 || a.state.ProviderRetry.Attempts != 1 {
		t.Fatal("crash recovery duplicated or refunded execution")
	}
}

// A retry cannot bypass a live worker, a lost repository lease or an uncertain merge submission.
func TestCapacityOwnership(t *testing.T) {
	for _, mode := range []string{"active worker", "lost lease", "uncertain merge"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, path := setupLoop(t, "capacity-always")
			lease, err := acquireRun(a)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			a.lease = lease
			home, err := workerHome()
			if err != nil {
				t.Fatal(err)
			}
			a.runtime = &runRuntime{WorkerHome: home}
			clock := newRetryClock()
			a.retryClock = clock
			clock.hook = func(time.Time) error {
				switch mode {
				case "active worker":
					identity, err := identifyProcess(os.Getpid())
					if err != nil {
						t.Fatal(err)
					}
					a.runtime.Worker = identity
					return a.saveRuntime()
				case "lost lease":
					lease.close()
				case "uncertain merge":
					a.state.Pending = &pendingMerge{PR: 1, Head: strings.Repeat("a", 40)}
				}
				return nil
			}
			err = a.work("implement", "Prepare the approved slice")
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Class != "ownership" && failure.Blocker.Class != "integrity" {
				t.Fatal(err)
			}
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			if f.Calls != 1 || f.Merges != 0 || a.state.Turns != 1 || a.state.ProviderRetry.Attempts != 0 {
				t.Fatal("retry bypassed ownership or consumed unlaunched work")
			}
		})
	}
}

// Sequence entry status exposes the current set's wait and recovery command without launching its successor twice.
func TestCapacitySequence(t *testing.T) {
	a, b, path := setupSequence(t, "normal")
	if err := a.run(); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	f.Mode = "capacity-always"
	if err := writeJSON(path, f); err != nil {
		t.Fatal(err)
	}
	if err := prepareSequence(a, []string{b.path}); err != nil {
		t.Fatal(err)
	}
	entry, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	clock := newRetryClock()
	clock.hook = func(at time.Time) error {
		r := inspectRun(entry)
		if r.Status != "waiting" || r.ProviderRetry == nil || !r.ProviderRetry.NextAt.Equal(at) || r.Sequence.Current != b.path {
			t.Fatalf("sequence wait hidden: %+v", r)
		}
		return nil
	}
	if err := runController(a, controllerOptions{RetryClock: clock, Output: outputJSONL}); err == nil {
		t.Fatal("capacity-limited successor completed")
	}
	r := inspectRun(entry)
	if r.Blocker == nil || r.Blocker.Code != "provider_capacity_exhausted" || r.ProviderRetry.Attempts != 5 || r.Blocker.NextAction.Argv[3] != a.path {
		t.Fatalf("sequence recovery points to a successor: %+v", r)
	}
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if f.Merges != 1 || retryCheckpoint(t, b.path).state.Turns != 6 || retryCheckpoint(t, a.path).state.Turns != 5 {
		t.Fatal("capacity repeated the completed predecessor")
	}
}

// Resuming an already-ready preparation phase must run the gate, not spend more preparation turns.
func TestCapacityGatePreparation(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "capacity-prepare")
	clock := newRetryClock()
	interrupted := errors.New("fixture controller interruption")
	clock.hook = func(time.Time) error { return interrupted }
	if err := runController(a, controllerOptions{RetryClock: clock, Output: outputJSONL}); !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	if a.state.ProviderRetry.Phase != "prepare_gate" || len(a.state.Merged) != 1 || a.state.Gate != nil {
		t.Fatal("preparation retry was not retained")
	}
	clock.hook = nil
	if err := runController(a, controllerOptions{RetryClock: clock, Output: outputJSONL}); err != nil {
		t.Fatal(err)
	}
	a = retryCheckpoint(t, a.path)
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	if f.Prepare != 2 || f.Merges != 1 || a.state.Status != "complete" || a.state.Turns != 6 || a.state.ProviderRetry.Attempts != 1 {
		t.Fatal("resumption repeated a completed preparation turn")
	}
}
