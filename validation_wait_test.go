package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func validationFIFO(t *testing.T, path string) *os.File {
	t.Helper()
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func fifoSignal(t *testing.T, f *os.File) {
	t.Helper()
	result := make(chan bool, 1)
	go func() { scanner := bufio.NewScanner(f); result <- scanner.Scan() }()
	select {
	case ok := <-result:
		if !ok {
			t.Fatal("fixture barrier closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fixture did not reach its synchronization barrier")
	}
}

type validationFixture struct {
	a                     *app
	path, count, repaired string
	ready, release        *os.File
	commands              []string
}

func setupValidation(t *testing.T, mode string) *validationFixture {
	t.Helper()
	recoveryEnvironment(t)
	a, path := setupLoop(t, mode)
	return configureValidation(t, a, path, mode)
}

func configureValidation(t *testing.T, a *app, path, mode string) *validationFixture {
	t.Helper()
	root := filepath.Dir(a.path)
	ackPath := filepath.Join(root, "worker-ack")
	_ = validationFIFO(t, ackPath)
	readyPath, releasePath := filepath.Join(root, "check-ready"), filepath.Join(root, "check-release")
	v := &validationFixture{a: a, path: path, count: filepath.Join(root, "check-count"), repaired: filepath.Join(root, "repaired"), ready: validationFIFO(t, readyPath), release: validationFIFO(t, releasePath)}
	check := "test -f runtime.txt"
	if mode == "validation-failure" {
		check = "echo fixture-validation-diagnostic >&2; test -f " + shellQuote(v.repaired)
	}
	v.commands = []string{"printf 'run\\n' >> " + shellQuote(v.count) + "; printf 'started\\n' > " + shellQuote(ackPath) + "; printf 'ready\\n' > " + shellQuote(readyPath) + "; read token < " + shellQuote(releasePath) + "; " + check, "git diff --check"}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	f.Mode = mode
	f.ValidationCommands, f.ValidationAck, f.ValidationRepairPath = v.commands, ackPath, v.repaired
	if err := writeJSON(path, f); err != nil {
		t.Fatal(err)
	}
	a.config.Slices[0].RequiredCommands = []string{v.commands[0]}
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	var err error
	v.a, err = loadApp(a.path)
	if err != nil {
		t.Fatal(err)
	}
	v.a.state.ProviderRetry = &providerRetry{Reason: capacityReason, Attempts: 2}
	if err := v.a.save(); err != nil {
		t.Fatal(err)
	}
	return v
}

type validationController struct {
	cmd     *exec.Cmd
	waiting chan struct{}
	done    chan struct{}
	err     error
	logs    strings.Builder
}

func startValidationController(t *testing.T, v *validationFixture, command string) *validationController {
	t.Helper()
	c := &validationController{cmd: exec.Command("looper", command, "--config", v.a.path, "--output", "jsonl"), waiting: make(chan struct{}, 8), done: make(chan struct{})}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.cmd.Stderr = io.Discard
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			c.logs.WriteString(scanner.Text() + "\n")
			var event logEvent
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "validation.waiting" {
				select {
				case c.waiting <- struct{}{}:
				default:
				}
			}
		}
		c.err = c.cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		_, _ = v.release.WriteString("finish\nfinish\nfinish\n")
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
			_ = c.cmd.Process.Kill()
			<-c.done
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			busy, err := lockBusy(filepath.Join(v.a.dir, "validate.lock"))
			if err == nil && !busy {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("fixture validator did not leave its released barrier")
	})
	return c
}

func (c *validationController) wait(t *testing.T) {
	t.Helper()
	select {
	case <-c.waiting:
	case <-c.done:
		data, _ := os.ReadFile(c.cmd.Args[3] + ".state/state.json")
		t.Fatalf("controller exited while validation was active: %v %s %s", c.err, data, c.logs.String())
	case <-time.After(5 * time.Second):
		t.Fatal("active validation was not observable as waiting")
	}
}

func (c *validationController) finish(t *testing.T, wantSuccess bool) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("controller did not reconcile validation")
	}
	if (c.err == nil) != wantSuccess {
		data, _ := os.ReadFile(c.cmd.Args[3] + ".validator-output")
		t.Fatalf("controller outcome %v; logs %s; validator %s", c.err, c.logs.String(), data)
	}
}

// Real detached validation, FIFO barriers and flock reproduce continue, repair and candidate transitions.
func TestValidationContinues(t *testing.T) {
	for _, mode := range []string{"validation-background", "validation-failure", "validation-commands", "validation-head", "validation-dirty", "validation-finished"} {
		t.Run(mode, func(t *testing.T) {
			v := setupValidation(t, mode)
			c := startValidationController(t, v, "run")
			fifoSignal(t, v.ready)
			var f fixture
			if mode != "validation-finished" {
				c.wait(t)
				saved := retryCheckpoint(t, v.a.path)
				if saved.state.Status != "waiting" || saved.state.Turns != 2 || saved.state.Stalled != 0 || saved.state.ProviderRetry.Attempts != 2 || saved.state.ValidationWait == nil {
					t.Fatal("validation wait changed execution budgets or lost its context")
				}
				if err := readJSON(v.path, &f); err != nil {
					t.Fatal(err)
				}
				if f.Calls != 2 || f.Merges != 0 {
					t.Fatal("validation overlapped a worker or merge")
				}
				r, code, diagnostic := recoveryCLI("status", "--config", v.a.path, "--json")
				if code != 0 || r.Run == nil || r.Run.Status != "waiting" || r.Run.Validation == nil || r.Run.Validation.Check != 1 || r.Run.Validation.Total != 2 || r.Run.Blocker != nil {
					t.Fatalf("JSON wait: %+v %s", r, diagnostic)
				}
				human, err := exec.Command("looper", "status", "--run", r.Run.ID, "--color", "never").CombinedOutput()
				if err != nil || !bytes.Contains(human, []byte("waiting:")) || !bytes.Contains(human, []byte("check 1/2")) || !bytes.Contains(human, []byte("elapsed")) {
					t.Fatalf("human wait: %s %v", human, err)
				}
				response, code, diagnostic := recoveryCLI("resume", "--run", r.Run.ID, "--detach", "--json")
				if code != 0 || response.Status != "already_running" {
					t.Fatalf("duplicate controller: %+v %s", response, diagnostic)
				}
				duplicate := exec.Command("looper", "validate", "--config", v.a.path, "--worktree", f.Worktree, "--command", "true")
				if err := duplicate.Run(); err == nil {
					t.Fatal("duplicate validator accepted")
				}
				if lease, err := acquireRun(saved); err == nil {
					lease.close()
					t.Fatal("waiting controller released repository ownership")
				}
				switch mode {
				case "validation-commands":
					f.PR.Body = strings.Replace(f.PR.Body, "- `git diff --check`", "- `test -f tracked.txt`\n- `git diff --check`", 1)
					if err := writeJSON(v.path, f); err != nil {
						t.Fatal(err)
					}
				case "validation-head":
					if _, err := git(f.Worktree, "commit", "--allow-empty", "-m", "test: change candidate"); err != nil {
						t.Fatal(err)
					}
				case "validation-dirty":
					if err := os.WriteFile(filepath.Join(f.Worktree, "tracked.txt"), []byte("dirty\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, _ = v.release.WriteString("finish\nfinish\n")
			success := mode != "validation-head" && mode != "validation-dirty"
			c.finish(t, success)
			a := retryCheckpoint(t, v.a.path)
			if err := readJSON(v.path, &f); err != nil {
				t.Fatal(err)
			}
			count, err := os.ReadFile(v.count)
			if err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if mode == "validation-failure" || mode == "validation-commands" {
				wantCount = 2
			}
			if strings.Count(string(count), "run\n") != wantCount {
				t.Fatalf("validation ran %q; want %d launches", count, wantCount)
			}
			if a.state.ProviderRetry.Attempts != 2 {
				t.Fatal("validation consumed provider retry allowance")
			}
			if !success {
				if f.Merges != 0 || f.Calls != 2 || a.state.Status != "blocked" || a.state.Blocker == nil || a.state.Blocker.Code != "validation_candidate_changed" {
					t.Fatalf("changed candidate accepted: %+v %+v", a.state, f)
				}
				return
			}
			if a.state.Status != "complete" || f.Merges != 1 || f.Implement != 2 || a.state.ValidationWait != nil {
				t.Fatal("validation did not continue delivery exactly once")
			}
			if mode == "validation-failure" {
				if f.Repairs != 1 || f.Calls != 6 {
					t.Fatal("validation failure did not follow the repair path")
				}
				logs, _ := filepath.Glob(filepath.Join(a.dir, "checks", "*.log"))
				found := false
				for _, path := range logs {
					data, _ := os.ReadFile(path)
					found = found || bytes.Contains(data, []byte("fixture-validation-diagnostic"))
				}
				if !found {
					t.Fatal("failed validation diagnostics lost")
				}
			} else if f.Calls != 5 {
				t.Fatal("waiting spent an extra worker turn")
			}
			for i, session := range f.WorkerSessions {
				if i > 0 && session != a.state.Session {
					t.Fatal("validation lost the conversation")
				}
			}
			if mode == "validation-background" && !strings.Contains(c.logs.String(), "Reusing successful checks") {
				t.Fatal("matching evidence was not reused")
			}
		})
	}
}

// A killed fixture controller can resume while its original validator still owns both locks.
func TestValidationRecovery(t *testing.T) {
	for _, finishBeforeResume := range []bool{false, true} {
		name := "active validation"
		if finishBeforeResume {
			name = "finished validation"
		}
		t.Run(name, func(t *testing.T) {
			v := setupValidation(t, "validation-background")
			c := startValidationController(t, v, "run")
			fifoSignal(t, v.ready)
			c.wait(t)
			before := retryCheckpoint(t, v.a.path)
			if err := c.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			c.finish(t, false)
			r, code, diagnostic := recoveryCLI("status", "--config", v.a.path, "--json")
			if code != 0 || r.Run.Status != "interrupted" || r.Run.Blocker.Code != "validation_controller_interrupted" || !r.Run.Blocker.Retryable {
				t.Fatalf("interruption hidden: %+v %s", r, diagnostic)
			}
			if finishBeforeResume {
				_, _ = v.release.WriteString("finish\n")
				deadline := time.Now().Add(5 * time.Second)
				for {
					_, active, err := before.validationState()
					if err == nil && !active {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("validator did not finish")
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			resumed := startValidationController(t, v, "resume")
			if !finishBeforeResume {
				resumed.wait(t)
			}
			if !finishBeforeResume {
				after := retryCheckpoint(t, v.a.path)
				if !reflect.DeepEqual(before.state.ValidationWait, after.state.ValidationWait) || before.state.Turns != after.state.Turns || before.state.Stalled != after.state.Stalled || before.state.Session != after.state.Session || !reflect.DeepEqual(before.state.ProviderRetry, after.state.ProviderRetry) || before.state.ConfigHash != after.state.ConfigHash {
					t.Fatal("resume reset retained execution state")
				}
			}
			_, _ = v.release.WriteString("finish\n")
			resumed.finish(t, true)
			var f fixture
			if err := readJSON(v.path, &f); err != nil {
				t.Fatal(err)
			}
			if f.Calls != 5 || f.Merges != 1 {
				t.Fatal("resume duplicated execution")
			}
			count, _ := os.ReadFile(v.count)
			if string(count) != "run\n" {
				t.Fatalf("resume repeated validation: %q", count)
			}
		})
	}
}

// This fixture represents the gap after a command exits while the batch retains real OS locks.
func fixtureValidationOwner(args []string) error {
	a, err := loadApp(args[0])
	if err != nil {
		return err
	}
	var f fixture
	if err := readJSON(os.Getenv("SLICE_LOOP_TEST_FIXTURE"), &f); err != nil {
		return err
	}
	root, err := projectIdentity(a.config.Repo)
	if err != nil {
		return err
	}
	lease := &runLease{}
	defer lease.close()
	for _, path := range []string{filepath.Join(root, "looper-validation.lock"), filepath.Join(a.dir, "validate.lock")} {
		if err := lease.acquire(path); err != nil {
			return err
		}
	}
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		return err
	}
	head, err := cleanHead(f.Worktree)
	if err != nil {
		return err
	}
	v := validationRun{processIdentity: owner, Version: 1, ID: "fixture-batch", Config: a.path, ConfigHash: a.state.ConfigHash, Repository: root, Worktree: f.Worktree, Head: head, Commands: []string{"true", "git diff --check"}, Status: "running", Started: time.Now().Add(-time.Minute), Check: 1, Completed: 1, CheckStart: time.Now().Add(-time.Minute), Log: filepath.Join(a.dir, "checks", head+".fixture-batch.log")}
	if err := writeJSON(filepath.Join(a.dir, "validation.json"), owner); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(a.dir, "validation-run.json"), v); err != nil {
		return err
	}
	ready, err := os.OpenFile(args[1], os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = ready.WriteString("ready\n")
	_ = ready.Close()
	if err != nil {
		return err
	}
	release, err := os.Open(args[2])
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(release)
	scanner.Scan()
	_ = release.Close()
	return scanner.Err()
}

func holdingValidation(t *testing.T) (*app, *exec.Cmd, *os.File, validationRun) {
	t.Helper()
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	if err := a.work("implement", "Prepare the approved slice"); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(a.path)
	readyPath, releasePath := filepath.Join(root, "owner-ready"), filepath.Join(root, "owner-release")
	ready, release := validationFIFO(t, readyPath), validationFIFO(t, releasePath)
	child := exec.Command("looper", "_hold-validation", a.path, readyPath, releasePath)
	child.Stdout, child.Stderr = io.Discard, io.Discard
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = release.WriteString("finish\n")
		_ = child.Wait()
	})
	fifoSignal(t, ready)
	var record validationRun
	if err := readJSON(filepath.Join(a.dir, "validation-run.json"), &record); err != nil {
		t.Fatal(err)
	}
	return a, child, release, record
}

// Batch identity and lock ownership protect command gaps, PID reuse and foreign or competing owners.
func TestValidationOwnership(t *testing.T) {
	for _, mode := range []string{"command gap", "stale check PID", "reused owner PID", "foreign config", "foreign repository", "foreign candidate", "legacy identity", "unrelated lock", "competing worker"} {
		t.Run(mode, func(t *testing.T) {
			a, _, _, record := holdingValidation(t)
			switch mode {
			case "stale check PID":
				record.Child = processIdentity{PID: os.Getpid(), Start: "0"}
			case "reused owner PID":
				record.Start = "0"
			case "foreign config":
				record.Config += "-other"
			case "foreign repository":
				record.Repository = newGitFixture(t)
			case "foreign candidate":
				record.Worktree = newGitFixture(t)
			case "legacy identity":
				if err := os.Remove(filepath.Join(a.dir, "validation-run.json")); err != nil {
					t.Fatal(err)
				}
			case "unrelated lock":
				record.processIdentity, _ = identifyProcess(os.Getpid())
				opened, err := os.Open(filepath.Join(a.dir, "validate.lock"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = opened.Close() })
			case "competing worker":
				owner, _ := identifyProcess(os.Getpid())
				if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Worker: owner}); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "legacy identity" {
				if err := writeJSON(filepath.Join(a.dir, "validation-run.json"), record); err != nil {
					t.Fatal(err)
				}
			}
			v, active, err := a.validationState()
			if mode == "command gap" || mode == "stale check PID" || mode == "competing worker" {
				if err != nil || !active {
					t.Fatalf("verified batch was lost during a command gap: %v", err)
				}
				if mode == "command gap" && (v.info().Status != "between_checks" || v.info().Elapsed < 60) {
					t.Fatal("gap or elapsed time hidden")
				}
				if mode == "competing worker" {
					if err := a.waitValidation(); err == nil {
						t.Fatal("competing worker accepted")
					}
				}
				return
			}
			var f *executionFailure
			if !errors.As(err, &f) || f.Blocker.Class != "ownership" {
				t.Fatalf("uncertain ownership accepted: %v", err)
			}
			turns, stalled := a.state.Turns, a.state.Stalled
			if err := a.work("implement", "Continue the same slice"); err == nil || a.state.Turns != turns || a.state.Stalled != stalled {
				t.Fatal("uncertain owner spent execution budget")
			}
		})
	}
}

// Disappearance never proves success; only a matching outcome, candidate and ordered evidence can recover.
func TestValidationOutcomes(t *testing.T) {
	for _, mode := range []string{"success", "missing outcome", "missing record", "missing evidence", "wrong evidence", "changed commands", "changed head", "dirty tree", "integrity failure"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, path := setupLoop(t, "normal")
			if err := a.work("implement", "Prepare the approved slice"); err != nil {
				t.Fatal(err)
			}
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			commands := []string{"test -f runtime.txt", "git diff --check"}
			if err := a.validate(f.Worktree, commands); err != nil {
				t.Fatal(err)
			}
			var v validationRun
			if err := readJSON(filepath.Join(a.dir, "validation-run.json"), &v); err != nil {
				t.Fatal(err)
			}
			a.state.ValidationWait = &validationWait{ID: v.ID, Owner: v.processIdentity, Head: v.Head, Worktree: v.Worktree, Commands: append([]string(nil), v.Commands...)}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireRun(a)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			a.lease = lease
			switch mode {
			case "missing outcome":
				v.Status, v.Finished = "running", nil
			case "missing evidence":
				if err := os.Remove(a.evidencePath(v.Head)); err != nil {
					t.Fatal(err)
				}
			case "wrong evidence":
				if err := writeJSON(a.evidencePath(v.Head), evidence{Head: v.Head, Commands: commands, Log: "other-batch.log"}); err != nil {
					t.Fatal(err)
				}
			case "changed commands":
				v.Commands = []string{"git diff --check", "test -f runtime.txt"}
			case "changed head":
				if _, err := git(f.Worktree, "commit", "--allow-empty", "-m", "test: new candidate"); err != nil {
					t.Fatal(err)
				}
			case "dirty tree":
				if err := os.WriteFile(filepath.Join(f.Worktree, "tracked.txt"), []byte("dirty\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "integrity failure":
				v.Status = "failed"
				v.Failure = &validationFailure("incomplete", "integrity", "Fixture integrity failure").Blocker
			}
			if err := writeJSON(filepath.Join(a.dir, "validation-run.json"), v); err != nil {
				t.Fatal(err)
			}
			if mode == "missing record" {
				if err := os.Remove(filepath.Join(a.dir, "validation-run.json")); err != nil {
					t.Fatal(err)
				}
			}
			before := a.state.Turns
			err = a.waitValidation()
			if mode == "success" {
				if err != nil || a.state.ValidationWait != nil {
					t.Fatalf("completed validation not recovered: %v", err)
				}
			} else if err == nil {
				t.Fatal("missing or inconsistent result treated as success")
			}
			if a.state.Turns != before {
				t.Fatal("outcome reconciliation spent worker turns")
			}
		})
	}
}

// Sequence entry recovery observes its current validator and never starts the successor early.
func TestValidationSequence(t *testing.T) {
	for _, position := range []string{"first set", "successor"} {
		t.Run(position, func(t *testing.T) {
			a, b, path := setupSequence(t, "normal")
			var v *validationFixture
			if position == "successor" {
				if err := a.run(); err != nil {
					t.Fatal(err)
				}
				v = configureValidation(t, b, path, "validation-background")
			} else {
				v = configureValidation(t, a, path, "validation-background")
				a = v.a
			}
			if err := prepareSequence(a, []string{b.path}); err != nil {
				t.Fatal(err)
			}
			launch := *v
			launch.a = a
			c := startValidationController(t, &launch, "run")
			fifoSignal(t, v.ready)
			c.wait(t)
			entry, err := entryFor(a)
			if err != nil {
				t.Fatal(err)
			}
			r := inspectRun(entry)
			if r.Status != "waiting" || r.Validation == nil || r.Sequence == nil || r.Sequence.Current != v.a.path {
				t.Fatalf("sequence wait hidden: %+v", r)
			}
			if position == "first set" {
				if retryCheckpoint(t, b.path).state.Turns != 0 {
					t.Fatal("successor started before current validation and closure")
				}
			} else {
				if err := c.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				c.finish(t, false)
				r = inspectRun(entry)
				if r.Status != "interrupted" || r.Blocker.Code != "validation_controller_interrupted" || r.Blocker.NextAction.Argv[3] != a.path {
					t.Fatalf("sequence recovery lost root ownership: %+v", r)
				}
				c = startValidationController(t, &launch, "resume")
				c.wait(t)
			}
			_, _ = v.release.WriteString("finish\nfinish\n")
			c.finish(t, true)
			var f fixture
			if err := readJSON(path, &f); err != nil {
				t.Fatal(err)
			}
			sequence, err := loadSequence(a)
			if err != nil || sequence.Phase != "complete" || len(sequence.Receipts) != 2 || f.Merges != 2 || f.Calls != 10 || retryCheckpoint(t, a.path).state.Turns != 5 || retryCheckpoint(t, b.path).state.Turns != 5 {
				t.Fatalf("sequence duplicated or skipped execution: %+v %+v %v", sequence, f, err)
			}
		})
	}
}

// Admission and merge guards exclude real validators without looking like an active validation batch.
func TestValidationGuard(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "normal")
	if err := a.work("implement", "Prepare the approved slice"); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	guard, err := a.guardValidation()
	if err != nil {
		t.Fatal(err)
	}
	defer guard.close()
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Controller: owner}); err != nil {
		t.Fatal(err)
	}
	entry, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := acquireRun(a)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	r, code, diagnostic := recoveryCLI("status", "--run", entry.ID, "--json")
	if code != 0 || r.Run.Status != "running" || r.Run.Blocker != nil {
		t.Fatalf("shared controller guard misreported as a validator: %+v %s", r, diagnostic)
	}
	marker := filepath.Join(filepath.Dir(a.path), "unexpected-validation")
	child := exec.Command("looper", "validate", "--config", a.path, "--worktree", f.Worktree, "--command", "printf 'unexpected' > "+shellQuote(marker))
	if err := child.Run(); err == nil {
		t.Fatal("validator overlapped worker admission or merge verification")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("excluded validator executed a command")
	}
}

// Old checkpoints, process metadata and successful evidence remain readable without migration or rerunning checks.
func TestValidationCompatibility(t *testing.T) {
	recoveryEnvironment(t)
	a, path := setupLoop(t, "normal")
	if err := a.work("implement", "Prepare the approved slice"); err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := readJSON(path, &f); err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(filepath.Dir(a.path), "legacy-count")
	commands := []string{"printf 'run\\n' >> " + shellQuote(count), "git diff --check"}
	if err := a.validate(f.Worktree, commands); err != nil {
		t.Fatal(err)
	}
	head, err := cleanHead(f.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.dir, "validation-run.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(a.dir, "validation.json"), processIdentity{PID: os.Getpid(), Start: "0"}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(a.evidencePath(head), evidence{Head: head, Commands: commands, Log: filepath.Join(a.dir, "checks", head+".log")}); err != nil {
		t.Fatal(err)
	}
	before := a.state
	reloaded := retryCheckpoint(t, a.path)
	if err := reloaded.ensureValidation(f.Worktree, commands); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reloaded.state, before) {
		t.Fatal("legacy evidence reuse changed the checkpoint")
	}
	data, _ := os.ReadFile(count)
	if string(data) != "run\n" {
		t.Fatalf("legacy evidence caused duplicate validation: %q", data)
	}
}
