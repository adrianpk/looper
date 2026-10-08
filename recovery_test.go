package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func recoveryEnvironment(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	// Herdr and Zellij append session metadata to Unix socket paths, so both roots must be short.
	runtime, err := os.MkdirTemp("", "sl-term-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtime) })
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(runtime, "config"))
	herdrConfig := filepath.Join(t.TempDir(), "herdr.toml")
	if err := os.WriteFile(herdrConfig, []byte("[terminal]\ndefault_shell = \"/bin/sh\"\n[ui.sound]\nenabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_CONFIG_PATH", herdrConfig)
}

func recoveryCLI(args ...string) (agentResponse, int, string) {
	c := exec.Command("looper", args...)
	var out, diagnostic bytes.Buffer
	c.Stdout, c.Stderr = &out, &diagnostic
	err := c.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			return agentResponse{}, -1, err.Error()
		}
	}
	var response agentResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		return response, -1, out.String() + diagnostic.String() + err.Error()
	}
	return response, code, diagnostic.String()
}

// Stable IDs and explicit ambiguity prevent resuming a different project or delivery set.
func TestRunSelection(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	b, _ := setupLoop(t, "normal")
	otherProject, err := registerRun(b)
	if err != nil {
		t.Fatal(err)
	}
	otherRepo := b.config.Repo
	b.config = a.config
	b.path = filepath.Join(filepath.Dir(b.path), "other.json")
	b.dir = b.path + ".state"
	if err := writeJSON(b.path, b.config); err != nil {
		t.Fatal(err)
	}
	loaded, loadErr := loadApp(b.path)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	b = loaded
	first, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registerRun(b)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias.json")
	if err := os.Symlink(a.path, alias); err != nil {
		t.Fatal(err)
	}
	linked, err := loadApp(alias)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := registerRun(linked)
	if err != nil || entry.ID != first.ID || linked.dir != a.dir {
		t.Fatalf("configuration alias changed recovery identity: %+v %v", entry, err)
	}
	for _, tc := range []struct {
		name   string
		args   []string
		status string
		code   int
		id     string
	}{
		{"exact run", []string{"status", "--run", first.ID, "--json"}, "ok", 0, first.ID},
		{"exact set", []string{"status", "--project", b.config.Repo, "--set", second.Set, "--json"}, "ok", 0, second.ID},
		{"other repository", []string{"status", "--project", otherRepo, "--json"}, "ok", 0, otherProject.ID},
		{"ambiguous project", []string{"resume", "--project", first.Project, "--detach", "--json"}, "selection_required", 3, ""},
		{"unknown run", []string{"resume", "--run", "missing", "--detach", "--json"}, "not_found", 2, ""},
		{"mixed selectors", []string{"status", "--run", first.ID, "--config", b.path, "--json"}, "invalid_arguments", 2, ""},
		{"json foreground", []string{"resume", "--run", first.ID, "--json"}, "invalid_arguments", 2, ""},
		{"unknown option", []string{"list", "--unknown", "--json"}, "invalid_arguments", 2, ""},
		{"invalid backend", []string{"resume", "--run", first.ID, "--detach", "--backend", "other", "--json"}, "invalid_arguments", 2, ""},
		{"foreground backend", []string{"resume", "--run", first.ID, "--backend", "tmux", "--json"}, "invalid_arguments", 2, ""},
		{"inspection backend", []string{"status", "--run", first.ID, "--backend", "auto", "--json"}, "invalid_arguments", 2, ""},
		{"invalid color", []string{"list", "--color", "other", "--json"}, "invalid_arguments", 2, ""},
		{"invalid output", []string{"resume", "--run", first.ID, "--detach", "--output", "other", "--json"}, "invalid_arguments", 2, ""},
		{"inspection output", []string{"list", "--output", "jsonl", "--json"}, "invalid_arguments", 2, ""},
		{"global resume", []string{"resume", "--all", "--detach", "--json"}, "invalid_arguments", 2, ""},
		{"global project", []string{"list", "--all", "--project", first.Project, "--json"}, "invalid_arguments", 2, ""},
		{"json forced color", []string{"status", "--run", first.ID, "--color", "always", "--json"}, "ok", 0, first.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, code, diagnostic := recoveryCLI(tc.args...)
			if code != tc.code || r.Status != tc.status || diagnostic != "" || r.SchemaVersion != 1 {
				t.Fatalf("unexpected result: %+v exit=%d stderr=%q", r, code, diagnostic)
			}
			if tc.id != "" && (r.Run == nil || r.Run.ID != tc.id) {
				t.Fatal("selected the wrong run")
			}
			if tc.status == "selection_required" && len(r.Runs) != 2 {
				t.Fatal("ambiguity did not return both sets from the selected project")
			}
		})
	}
	if _, err := os.Stat(filepath.Join(a.dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("selection mutated the delivery checkpoint")
	}
}

// Discovery includes old standard-directory runs and leaves all files untouched.
func TestRunDiscovery(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	empty, code, diagnostic := recoveryCLI("list", "--all", "--json")
	if code != 0 || diagnostic != "" || empty.Runs == nil || len(empty.Runs) != 0 {
		t.Fatalf("empty discovery must return an array: %+v %d %s", empty, code, diagnostic)
	}
	root, err := registryRoot()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "legacy.json")
	if err := writeJSON(path, a.config); err != nil {
		t.Fatal(err)
	}
	r, code, diagnostic := recoveryCLI("list", "--all", "--json")
	if code != 0 || diagnostic != "" || len(r.Runs) != 1 || r.Runs[0].Config != path {
		t.Fatalf("old configuration was not discovered: %+v %d %s", r, code, diagnostic)
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only discovery created a registry")
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	r, code, _ = recoveryCLI("list", "--all", "--json")
	if code != 0 || len(r.Runs) != 1 || r.Runs[0].Status != "invalid" || r.Runs[0].Blocker == nil {
		t.Fatal("invalid configuration disappeared from discovery")
	}
	if err := writeJSON(path, a.config); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(root, "runs", "broken.json"), []byte("{")); err != nil {
		t.Fatal(err)
	}
	r, code, diagnostic = recoveryCLI("list", "--all", "--json")
	if code != 0 || len(r.Runs) != 2 || diagnostic != "" {
		t.Fatalf("broken index hid other projects: %+v %d %s", r, code, diagnostic)
	}
	valid, invalid := 0, 0
	for _, run := range r.Runs {
		if run.Status == "ready" {
			valid++
		} else if run.Blocker != nil && run.Blocker.Code == "invalid_registry_entry" {
			invalid++
		}
	}
	if valid != 1 || invalid != 1 {
		t.Fatal("broken index was not isolated and reported")
	}
}

// Live locks, orphan workers and exhausted budgets must never launch a second worker.
func TestResumeGuards(t *testing.T) {
	for _, mode := range []string{"controller", "repository", "worker", "invalid ownership", "validation", "budget", "no progress", "changed config", "corrupt state", "pending merge"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, fixturePath := setupLoop(t, "normal")
			e, err := registerRun(a)
			if err != nil {
				t.Fatal(err)
			}
			a.state.Session = "saved-conversation"
			a.state.Status = "blocked"
			switch mode {
			case "controller", "repository":
				path := filepath.Join(a.dir, "run.lock")
				if mode == "repository" {
					path, err = repositoryLock(a)
					if err != nil {
						t.Fatal(err)
					}
				}
				lease := &runLease{}
				if err := lease.acquire(path); err != nil {
					t.Fatal(err)
				}
				defer lease.close()
			case "worker", "validation":
				owner, err := identifyProcess(os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				var saveErr error
				if mode == "worker" {
					saveErr = writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Worker: owner})
				} else {
					saveErr = writeJSON(filepath.Join(a.dir, "validation.json"), owner)
				}
				if saveErr != nil {
					t.Fatal(saveErr)
				}
			case "invalid ownership":
				if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Worker: processIdentity{PID: os.Getpid()}}); err != nil {
					t.Fatal(err)
				}
			case "budget":
				a.state.Turns = a.config.MaxTurns
			case "no progress":
				a.state.Stalled = a.config.MaxStalled
				a.state.Fingerprint, err = a.fingerprint()
				if err != nil {
					t.Fatal(err)
				}
			case "changed config":
				a.config.FullGate = "false"
				if err := writeJSON(a.path, a.config); err != nil {
					t.Fatal(err)
				}
			case "pending merge":
				a.state.Pending = &pendingMerge{PR: 1, Head: strings.Repeat("a", 40)}
			}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			if mode == "corrupt state" {
				if err := os.WriteFile(filepath.Join(a.dir, "state.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			r, code, diagnostic := recoveryCLI("resume", "--run", e.ID, "--detach", "--json")
			if mode == "controller" {
				if code != 0 || r.Status != "already_running" {
					t.Fatalf("active controller was not idempotent: %+v %d", r, code)
				}
			} else if code != 4 || r.Status != "blocked" || r.Error == nil {
				t.Fatalf("unsafe resume accepted: %+v %d %s", r, code, diagnostic)
			}
			after, _ := os.ReadFile(filepath.Join(a.dir, "state.json"))
			var f fixture
			if err := readJSON(fixturePath, &f); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || f.Calls != 0 || f.Merges != 0 {
				t.Fatal("guard changed progress or executed product work")
			}
		})
	}
}

// Older workers have no runtime file; their exact conversation or schema still fences recovery.
func TestLegacyOrphan(t *testing.T) {
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, "hold-only")
	a.state.Session = "legacy-conversation"
	a.state.Status = "running"
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	e, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("codex", "exec", "resume", a.state.Session)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = os.WriteFile(fixturePath+".release", []byte("finish"), 0600)
		if err := worker.Wait(); err != nil {
			t.Error(err)
		}
	}()
	r, code, diagnostic := recoveryCLI("resume", "--run", e.ID, "--detach", "--json")
	if code != 4 || r.Error == nil || r.Error.Code != "worker_not_idle" || diagnostic != "" {
		t.Fatalf("orphan worker was duplicated: %+v %d %s", r, code, diagnostic)
	}
}

// PID reuse cannot turn stale metadata into ownership of an unrelated process.
func TestProcessOwnership(t *testing.T) {
	p, err := identifyProcess(os.Getpid())
	if err != nil || !processAlive(p) {
		t.Fatalf("current process is not observable: %v", err)
	}
	p.Start += "0"
	if processAlive(p) {
		t.Fatal("stale process identity accepted")
	}
	path := filepath.Join(t.TempDir(), "run.lock")
	l := &runLease{}
	defer l.close()
	if err := l.acquire(path); err != nil {
		t.Fatal(err)
	}
	if busy, err := lockBusy(path); err != nil || !busy {
		t.Fatalf("held OS lock not detected: %v", err)
	}
	if err := syscall.Flock(int(l.files[0].Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if busy, err := lockBusy(path); err != nil || busy {
		t.Fatalf("released OS lock still marked busy: %v", err)
	}
}

// Two agents racing to resume share one persistent controller and keep the original checkpoint.
func TestDetachedResume(t *testing.T) {
	for _, backend := range []string{"herdr", "zellij", "tmux"} {
		for _, format := range []outputFormat{outputJSONL, outputHuman} {
			t.Run(backend+"/"+string(format), func(t *testing.T) { testDetachedResume(t, backend, format) })
		}
	}
}

func testDetachedResume(t *testing.T, backend string, format outputFormat) {
	t.Helper()
	if _, err := exec.LookPath(backend); err != nil {
		t.Skip(backend + " is not installed")
	}
	recoveryEnvironment(t)
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TMUX_TMPDIR", t.TempDir())
	t.Setenv("TMUX", "")
	a, fixturePath := setupLoop(t, "paused-worker")
	a.path = filepath.Join(filepath.Dir(a.path), "run 'quoted' $(false).json")
	a.dir = a.path + ".state"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	a.state.Status = "blocked"
	a.state.Session = "11111111-1111-4111-8111-111111111111"
	a.state.Turns = 2
	a.state.Stalled = 2
	fingerprint, err := a.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	a.state.Fingerprint = fingerprint
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	e, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	// Releasing the owned fixture lets every test controller finish normally, including on failure.
	t.Cleanup(func() {
		_ = os.WriteFile(fixturePath+".release", []byte("continue"), 0600)
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			current, err := loadApp(a.path)
			busy, lockErr := lockBusy(filepath.Join(a.dir, "run.lock"))
			if err == nil && lockErr == nil && !busy && !launching(current) {
				if backend == "herdr" {
					terminal := &terminalRef{Backend: backend, Session: "loop-" + strings.TrimPrefix(e.ID, "run-")}
					if _, err := terminal.command(a.config.Repo, "session", "stop", terminal.Session, "--json"); err != nil {
						t.Error(err)
					}
				}
				if backend == "zellij" {
					terminal := &terminalRef{Backend: backend, Session: "loop-" + strings.TrimPrefix(e.ID, "run-")}
					if _, err := terminal.command(a.config.Repo, "kill-session", terminal.Session); err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) || exit.ExitCode() != 1 {
							t.Error(err)
						}
					}
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("owned detached fixture did not finish")
	})
	type result struct {
		response agentResponse
		code     int
		stderr   string
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			r, code, stderr := recoveryCLI("resume", "--run", e.ID, "--detach", "--backend", backend, "--activity", "--output", string(format), "--json")
			results <- result{r, code, stderr}
		}()
	}
	started := 0
	for range 2 {
		r := <-results
		if r.code != 0 || r.stderr != "" || (r.response.Status != "started" && r.response.Status != "already_running") {
			t.Fatalf("concurrent launch failed: %+v error=%+v exit=%d stderr=%s", r.response, r.response.Error, r.code, r.stderr)
		}
		if r.response.Status == "started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("expected exactly one new controller, got %d", started)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(fixturePath + ".entered"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached worker never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	r := inspectRun(e)
	if r.Status != "running" || r.Terminal == nil || r.Terminal.Backend != backend || r.Terminal.Session == "" || r.ViewCommand == "" || r.Session != a.state.Session {
		t.Fatalf("detached ownership or conversation changed: %+v", r)
	}
	if r.WorkerTurns != 3 || r.StalledTurns != 2 {
		t.Fatalf("resume reset progress counters or failed to reserve its active turn: %+v", r)
	}
	var screen string
	switch backend {
	case "herdr":
		screen, err = r.Terminal.command(a.config.Repo, "pane", "read", r.Terminal.Pane, "--format", "ansi")
	case "zellij":
		screen, err = r.Terminal.command(a.config.Repo, "--session", r.Terminal.Session, "action", "dump-screen", "--pane-id", "terminal_0", "--full", "--ansi")
	case "tmux":
		screen, err = r.Terminal.command(a.config.Repo, "-L", "slice-loop", "capture-pane", "-e", "-J", "-p", "-t", "="+r.Terminal.Session+":")
	}
	if err != nil {
		t.Fatal(err)
	}
	if format == outputHuman && !strings.Contains(screen, "\x1b[") {
		t.Fatalf("forced controller colors did not reach the native pane: %q", screen)
	}
	if format == outputJSONL && (!strings.Contains(screen, `"schema_version":1`) || !strings.Contains(screen, `"type":"worker.started"`)) {
		t.Fatalf("JSONL did not reach the native pane: %q", screen)
	}
	if err := os.WriteFile(fixturePath+".release", []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		current, err := loadApp(a.path)
		busy, lockErr := lockBusy(filepath.Join(a.dir, "run.lock"))
		if err == nil && current.state.Status == "complete" && lockErr == nil && !busy && !launching(current) {
			if current.state.Turns != 7 || current.state.Session != a.state.Session || len(current.state.Merged) != 1 {
				t.Fatalf("resume reset or duplicated checkpoint progress: %+v", current.state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("detached delivery did not complete: %+v %v", current, err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	rsp, code, diagnostic := recoveryCLI("resume", "--run", e.ID, "--detach", "--backend", backend, "--color", "always", "--activity", "--json")
	if code != 0 || rsp.Status != "started" || diagnostic != "" {
		t.Fatalf("completed checkpoint could not be safely verified: %+v %d %s", rsp, code, diagnostic)
	}
	deadline = time.Now().Add(15 * time.Second)
	for {
		current, err := loadApp(a.path)
		busy, lockErr := lockBusy(filepath.Join(a.dir, "run.lock"))
		if err == nil && lockErr == nil && !busy && !launching(current) {
			var f fixture
			if err := readJSON(fixturePath, &f); err != nil {
				t.Fatal(err)
			}
			if current.state.Status != "complete" || current.state.Turns != 7 || f.Calls != 5 || f.Merges != 1 {
				t.Fatalf("repeated resume duplicated completed work: %+v %+v", current.state, f)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed-run verification did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
