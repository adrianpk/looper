package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func setupSequence(t *testing.T, mode string) (*app, *app, string) {
	t.Helper()
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, mode)
	data, err := json.Marshal(a.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Plan = "ops/default/plan/next.md"
	cfg.Tracker = "ops/default/tracker/next.md"
	cfg.Slices[0].Branch = "feat/next"
	cfg.Slices[0].Title = "feat(slice-1): deliver next"
	cfg.Slices[0].Report = "ops/default/report/next-1.md"
	cfg.ClosurePaths = []string{cfg.Plan, cfg.Tracker, cfg.Slices[0].Report}
	cfg.Session = "22222222-2222-4222-8222-222222222222"
	for i, path := range []string{cfg.Plan, cfg.Tracker} {
		original := []string{a.config.Plan, a.config.Tracker}[i]
		doc, err := os.ReadFile(filepath.Join(a.config.Repo, original))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ReplaceAll(string(doc), "feat/flow", cfg.Slices[0].Branch)
		text = strings.ReplaceAll(text, a.config.Slices[0].Title, cfg.Slices[0].Title)
		text = strings.ReplaceAll(text, a.config.Slices[0].Report, cfg.Slices[0].Report)
		if err := os.WriteFile(filepath.Join(cfg.Repo, path), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixtureCommit(a.config.Repo, "docs: approve successor", true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(a.path), "next.json")
	if err := writeJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	b, err := loadApp(path)
	if err != nil {
		t.Fatal(err)
	}
	return a, b, fixturePath
}

// Two independently approved sets integrate serially, including lost merge responses.
func TestApprovedSequence(t *testing.T) {
	for _, mode := range []string{"normal", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			a, b, path := setupSequence(t, mode)
			if err := prepareSequence(a, []string{b.path}); err != nil {
				t.Fatal(err)
			}
			if err := runController(a, controllerOptions{}); err != nil {
				t.Fatal(err)
			}
			s, err := loadSequence(a)
			if err != nil || s.Phase != "complete" || len(s.Receipts) != 2 {
				t.Fatalf("sequence did not complete: %+v %v", s, err)
			}
			first, err := loadApp(a.path)
			if err != nil {
				t.Fatal(err)
			}
			second, err := loadApp(b.path)
			if err != nil {
				t.Fatal(err)
			}
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			if f.Merges != 2 || f.Closes != 2 || first.state.Turns != 5 || second.state.Turns != 5 || first.state.Session == second.state.Session || second.state.Session != b.config.Session {
				t.Fatalf("sequence lost state or repeated work: %+v first=%+v second=%+v", f, first.state, second.state)
			}
			before := f
			if err := runController(a, controllerOptions{}); err != nil {
				t.Fatal(err)
			}
			if err := readJSON(path, &f); err != nil || !reflect.DeepEqual(f, before) {
				t.Fatal("completed sequence repeated work", err)
			}
		})
	}
}

// Public continuation flags reserve successors and keep automatic project selection on the supervisor.
func TestSequenceCLI(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	a, b, path := setupSequence(t, "paused-worker")
	t.Cleanup(func() { finishDetachedFixture(t, a, path, "tmux") })
	if _, err := registerRun(b); err != nil {
		t.Fatal(err)
	}
	response, code, diagnostic := recoveryCLI("resume", "--config", a.path, "--then", b.path, "--detach", "--backend", "tmux", "--json")
	if code != 0 || diagnostic != "" || response.Status != "started" || response.Run == nil || response.Run.Sequence == nil || response.Run.Sequence.Total != 2 {
		t.Fatalf("sequence CLI failed: %+v %d %s", response, code, diagnostic)
	}
	waitForFile(t, path+".entered")
	t.Chdir(a.config.Repo)
	r, code, diagnostic := recoveryCLI("status", "--json")
	if code != 0 || diagnostic != "" || r.Run == nil || r.Run.Config != a.path {
		t.Fatalf("sequence duplicated active selection: %+v %d %s", r, code, diagnostic)
	}
	r, code, diagnostic = recoveryCLI("resume", "--config", b.path, "--detach", "--json")
	if code != 4 || diagnostic != "" || r.Error == nil || r.Error.Code != "sequence_owned" {
		t.Fatalf("successor bypassed owner: %+v %d %s", r, code, diagnostic)
	}
}

// Launcher failure remains visible after its caller exits, before any set consumes a turn.
func TestSequenceLaunchFailure(t *testing.T) {
	a, b, _ := setupSequence(t, "normal")
	if err := prepareSequence(a, []string{b.path}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	r, code, _ := recoveryCLI("resume", "--config", a.path, "--detach", "--backend", "herdr", "--json")
	if code != 5 || r.Error == nil || r.Error.Code != "backend_unavailable" {
		t.Fatalf("launch failure not returned: %+v %d", r, code)
	}
	e, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	status := inspectRun(e)
	if status.Status != "blocked" || status.Blocker == nil || status.Blocker.Component != "launcher" || status.Sequence.Completed != 0 || status.WorkerTurns != 0 {
		t.Fatalf("launch failure was hidden: %+v", status)
	}
}

// A failed full gate prevents closure receipts and successor activation.
func TestSequenceValidationFailure(t *testing.T) {
	a, b, _ := setupSequence(t, "gate-failure")
	if err := prepareSequence(a, []string{b.path}); err != nil {
		t.Fatal(err)
	}
	err := runController(a, controllerOptions{})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Class != "validation" {
		t.Fatalf("gate failure misclassified: %v", err)
	}
	s, err := loadSequence(a)
	if err != nil || len(s.Receipts) != 0 || s.Current != 0 || s.Phase != "blocked" {
		t.Fatalf("failed gate advanced sequence: %+v %v", s, err)
	}
	second, err := loadApp(b.path)
	if err != nil || second.state.Turns != 0 {
		t.Fatal("successor started after failed validation", err)
	}
}

// A handoff failure retains the predecessor's closure and resumes the original successor checkpoint.
func TestHandoffRecovery(t *testing.T) {
	a, b, path := setupSequence(t, "normal")
	a.state.Session = "11111111-1111-4111-8111-111111111111"
	a.state.Turns = 2
	b.state.Turns = 3
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	if err := b.save(); err != nil {
		t.Fatal(err)
	}
	if err := prepareSequence(a, []string{b.path}); err != nil {
		t.Fatal(err)
	}
	fault := filepath.Join(t.TempDir(), "unavailable")
	if err := os.WriteFile(fault, nil, 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(t.TempDir(), "provider")
	script := "#!/bin/sh\nif test -f " + shellQuote(fault) + " && grep -q '\"phase\": \"handoff\"' " + shellQuote(filepath.Join(a.dir, "sequence.json")) + "; then exit 77; fi\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	testHostContext(t, executionContext{Version: 1, ProviderProbe: probe})
	err := runController(a, controllerOptions{})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Class != "authentication" {
		t.Fatalf("handoff did not block on auth: %v", err)
	}
	s, err := loadSequence(a)
	if err != nil || len(s.Receipts) != 1 || s.Current != 0 || s.Phase != "blocked" {
		t.Fatalf("handoff state lost: %+v %v", s, err)
	}
	first, err := loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := loadApp(b.path)
	if err != nil {
		t.Fatal(err)
	}
	if first.state.Status != "complete" || first.state.Turns != 7 || second.state.Turns != 3 || second.state.Session != b.config.Session {
		t.Fatal("handoff changed checkpoints")
	}
	entry, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	r := inspectRun(entry)
	if r.Status != "blocked" || r.Sequence == nil || r.Sequence.Completed != 1 || r.Blocker == nil || r.Blocker.Component != "provider" || r.Blocker.NextAction == nil {
		t.Fatalf("handoff failure hidden: %+v", r)
	}
	if err := os.Remove(fault); err != nil {
		t.Fatal(err)
	}
	if err := runController(a, controllerOptions{}); err != nil {
		t.Fatal(err)
	}
	retained, err := loadApp(a.path)
	if err != nil || !reflect.DeepEqual(first.state, retained.state) {
		t.Fatal("predecessor repeated or lost progress", err)
	}
	second, err = loadApp(b.path)
	if err != nil || second.state.Turns != 8 || second.state.Session != b.config.Session {
		t.Fatal("successor budget or conversation reset", err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil || f.Merges != 2 || f.Closes != 2 {
		t.Fatal("handoff repeated integration", err)
	}
}

// The supervisor can disappear during read-only handoff preflight, before any successor worker exists.
func TestInterruptedHandoff(t *testing.T) {
	for _, phase := range []string{"handoff", "running"} {
		t.Run(phase, func(t *testing.T) { testInterruptedHandoff(t, phase) })
	}
}

func testInterruptedHandoff(t *testing.T, phase string) {
	a, b, path := setupSequence(t, "normal")
	if err := prepareSequence(a, []string{b.path}); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "handoff")
	done := marker + ".done"
	probe := filepath.Join(t.TempDir(), "provider")
	condition := "grep -q '\"phase\": \"handoff\"' "
	if phase == "running" {
		condition = "grep -q '\"current_index\": 1' "
	}
	script := "#!/bin/sh\nif " + condition + shellQuote(filepath.Join(a.dir, "sequence.json")) + "; then printf ready > " + shellQuote(marker) + "; sleep 1; printf done > " + shellQuote(done) + "; fi\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	contextPath := testHostContext(t, executionContext{Version: 1, ProviderProbe: probe})
	c := exec.Command("looper", "run", "--config", a.path)
	var diagnostic bytes.Buffer
	c.Stdout = &diagnostic
	c.Stderr = &diagnostic
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if c.ProcessState == nil {
			_ = c.Wait()
		}
	}()
	waitForFile(t, marker)
	s, err := loadSequence(a)
	if err != nil || s.Phase != phase || len(s.Receipts) != 1 {
		t.Fatalf("not at safe handoff boundary: %+v %v", s, err)
	}
	current, err := loadApp(b.path)
	if err != nil || current.state.Turns != 0 {
		t.Fatal("successor started before handoff", err)
	}
	repoLock, err := repositoryLock(a)
	if err != nil {
		t.Fatal(err)
	}
	if busy, err := lockBusy(repoLock); err != nil || !busy {
		t.Fatal("repository lease was released during handoff", err)
	}
	// Only this fixture supervisor is interrupted; its gate and workers have already exited.
	if err := c.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := c.Wait(); err == nil {
		t.Fatal("fixture supervisor did not stop")
	}
	waitForFile(t, done)
	entry, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	r := inspectRun(entry)
	if r.Status != "blocked" || r.Blocker == nil || r.Blocker.Code != "handoff_interrupted" || r.Sequence.Completed != 1 {
		t.Fatalf("interruption not detected: %+v", r)
	}
	if err := writeJSON(contextPath, executionContext{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := runController(a, controllerOptions{}); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil || f.Merges != 2 || f.Closes != 2 {
		t.Fatal("interrupted recovery repeated work", err)
	}
}

// Pinned approval and live ownership cannot be bypassed through a successor or changed list.
func TestSequenceGuards(t *testing.T) {
	for _, mode := range []string{"duplicate", "unapproved", "changed", "owned", "worker"} {
		t.Run(mode, func(t *testing.T) {
			a, b, _ := setupSequence(t, "normal")
			switch mode {
			case "duplicate":
				if err := prepareSequence(a, []string{a.path}); err == nil {
					t.Fatal("duplicate set accepted")
				}
				return
			case "unapproved":
				p := filepath.Join(b.config.Repo, b.config.Plan)
				doc, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(doc), "Approved:", "Withdrawn:")), 0600); err != nil {
					t.Fatal(err)
				}
				if err := fixtureCommit(b.config.Repo, "docs: withdraw approval", true); err != nil {
					t.Fatal(err)
				}
				if err := prepareSequence(a, []string{b.path}); err == nil {
					t.Fatal("unapproved successor accepted")
				}
				return
			case "worker":
				owner, err := identifyProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				if err := writeJSON(filepath.Join(b.dir, "runtime.json"), runRuntime{Worker: owner}); err != nil {
					t.Fatal(err)
				}
				if err := prepareSequence(a, []string{b.path}); err == nil {
					t.Fatal("live worker ignored")
				}
				return
			}
			if err := prepareSequence(a, []string{b.path}); err != nil {
				t.Fatal(err)
			}
			if mode == "owned" {
				if err := runController(b, controllerOptions{}); err == nil {
					t.Fatal("successor bypassed sequence owner")
				}
			} else {
				b.config.FullGate = "false"
				if err := writeJSON(b.path, b.config); err != nil {
					t.Fatal(err)
				}
				if err := runController(a, controllerOptions{}); err == nil {
					t.Fatal("changed config pin accepted")
				}
			}
			first, err := loadApp(a.path)
			if err != nil || first.state.Turns != 0 {
				t.Fatal("rejected sequence started work", err)
			}
		})
	}
}
