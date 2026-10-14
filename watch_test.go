package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The same binary opens the exact project's native view without shell parsing, worker input or checkpoint writes.
func TestWatch(t *testing.T) {
	const session = "loop 'quoted' $(false)"
	for _, tc := range []struct {
		backend string
		args    []string
	}{
		{"herdr", []string{"session", "attach", session}},
		{"zellij", []string{"watch", "--", session}},
		{"tmux", []string{"-L", "slice-loop", "attach-session", "-r", "-t", "=" + session}},
	} {
		t.Run(tc.backend, func(t *testing.T) {
			recoveryEnvironment(t)
			a, fixturePath := setupLoop(t, "normal")
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := registerRun(a); err != nil {
				t.Fatal(err)
			}
			lease, err := acquireRun(a)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			terminal := &terminalRef{Backend: tc.backend, Session: session}
			a.runtime = &runRuntime{Terminal: terminal}
			if err := a.saveRuntime(); err != nil {
				t.Fatal(err)
			}
			t.Chdir(a.config.Repo)
			t.Setenv("HERDR_ENV", "")
			t.Setenv("HERDR_SOCKET_PATH", "/outer/socket")
			t.Setenv("TMUX", "outer")
			dir := t.TempDir()
			stub := "#!/bin/sh\n[ -z \"${HERDR_SOCKET_PATH-}${TMUX-}\" ] || exit 78\nprintf '%s\\n' \"$@\"\n"
			if tc.backend == "herdr" {
				stub = "#!/bin/sh\n[ -z \"${HERDR_ENV-}${HERDR_SOCKET_PATH-}${TMUX-}\" ] || exit 78\n[ \"$#\" -eq 3 ] && [ \"$1\" = session ] && [ \"$2\" = attach ] && [ \"$3\" != -- ] || exit 64\nprintf '%s\\n' \"$@\"\n"
			}
			if err := os.WriteFile(filepath.Join(dir, tc.backend), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			for _, args := range [][]string{{"--watch"}, {"watch"}} {
				command := exec.Command("looper", args...)
				var out, diagnostic bytes.Buffer
				command.Stdout, command.Stderr = &out, &diagnostic
				if err := command.Run(); err != nil || diagnostic.Len() != 0 {
					t.Fatalf("watch failed: %v %s", err, diagnostic.String())
				}
				actual := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
				if !reflect.DeepEqual(actual, tc.args) {
					t.Fatalf("native view arguments changed: %q, want %q", actual, tc.args)
				}
			}
			after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			var f fixture
			fixtureErr := readJSON(fixturePath, &f)
			if err != nil || string(before) != string(after) || fixtureErr != nil || f.Calls != 0 || f.Merges != 0 {
				t.Fatalf("watch advanced the run: %v %v %+v", err, fixtureErr, f)
			}
		})
	}
}

// The text view retains event context, follows overlapping snapshots and waits for complete JSONL records.
func TestWatchText(t *testing.T) {
	const event = `{"schema_version":1,"time":"2026-10-09T10:00:00Z","type":"worker.message","label":"Slice 2/3 · implement · turn 4","message":"Task: preserve progress"}`
	const rendered = "[2026-10-09T10:00:00Z] Slice 2/3 · implement · turn 4 · Task: preserve progress\n"
	for _, tc := range []struct {
		name      string
		snapshots []string
		want      string
	}{
		{"event context", []string{event}, rendered},
		{"human output", []string{"Slice 2/3 · implement · Task: preserve progress"}, "Slice 2/3 · implement · Task: preserve progress\n"},
		{"other JSON", []string{`{"schema_version":1,"result":"diagnostic"}`}, "{\"schema_version\":1,\"result\":\"diagnostic\"}\n"},
		{"unchanged", []string{event, event}, rendered},
		{"scrollback", []string{"first\nsecond", "second\nthird"}, "first\nsecond\nthird\n"},
		{"repeated lines", []string{"repeat", "repeat\nrepeat"}, "repeat\nrepeat\n"},
		{"partial event", []string{"first\n" + event[:80], "first\n" + event}, "first\n" + rendered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var previous []string
			for _, snapshot := range tc.snapshots {
				var err error
				previous, err = writeWatchSnapshot(&out, previous, snapshot)
				if err != nil {
					t.Fatal(err)
				}
			}
			if out.String() != tc.want {
				t.Fatalf("text view = %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// Real Herdr unwraps JSONL from the recorded pane; watching from another pane never attaches or changes the run.
func TestHerdrWatch(t *testing.T) {
	if _, err := exec.LookPath("herdr"); err != nil {
		t.Skip("herdr is not installed")
	}
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, "normal")
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil {
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
	terminal := &terminalRef{Backend: "herdr", Session: "watch-" + entry.ID}
	t.Cleanup(func() {
		if _, err := terminal.command(a.config.Repo, "session", "stop", terminal.Session, "--json"); err != nil {
			t.Error(err)
		}
	})
	if err := terminal.prepareHerdr(a); err != nil {
		t.Fatal(err)
	}
	a.runtime = &runRuntime{Terminal: terminal}
	if err := a.saveRuntime(); err != nil {
		t.Fatal(err)
	}
	label := "Slice 2/3 · implement · turn 4"
	message := "Task: preserve " + strings.Repeat("wrapped progress ", 24) + "and its message"
	event, err := json.Marshal(logEvent{SchemaVersion: 1, Type: "worker.message", Label: label, Message: message})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, append(event, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(filepath.Dir(path), "next.jsonl")
	event, err = json.Marshal(logEvent{SchemaVersion: 1, Type: "worker.message", Label: label, Message: "Task: next update"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(next, append(event, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(filepath.Dir(path), "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0600) })
	producer := "cat " + shellQuote(path) + "; while [ ! -f " + shellQuote(release) + " ]; do sleep 0.05; done; cat " + shellQuote(next)
	if _, err := terminal.command(a.config.Repo, "pane", "run", terminal.Pane, producer); err != nil {
		t.Fatal(err)
	}
	t.Chdir(a.config.Repo)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "outer-pane")
	t.Setenv("HERDR_SOCKET_PATH", "/outer/socket")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "looper", "--watch")
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	found, followed := 0, false
	for scanner.Scan() {
		if scanner.Text() == label+" · "+message {
			found++
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Error(err)
			}
		}
		if scanner.Text() == label+" · Task: next update" {
			followed = true
			if err := command.Process.Signal(os.Interrupt); err != nil {
				t.Error(err)
			}
			break
		}
	}
	waitErr := command.Wait()
	if found != 1 || !followed || waitErr != nil || diagnostic.Len() != 0 {
		t.Fatalf("recorded pane view failed: found=%d followed=%v scan=%v wait=%v diagnostic=%s", found, followed, scanner.Err(), waitErr, diagnostic.String())
	}
	after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	var f fixture
	fixtureErr := readJSON(fixturePath, &f)
	if err != nil || !bytes.Equal(before, after) || fixtureErr != nil || f.Calls != 0 || f.Merges != 0 {
		t.Fatalf("text watch advanced the run: %v %v %+v", err, fixtureErr, f)
	}
}

// Missing pane metadata must fail instead of reading the caller's current pane or opening another client.
func TestHerdrWatchMissingPane(t *testing.T) {
	err := watchHerdr(context.Background(), runInfo{Terminal: &terminalRef{Backend: "herdr", Session: "loop"}}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no recorded pane") {
		t.Fatalf("missing pane was not rejected: %v", err)
	}
}

// A vanished recorded pane exposes the latest checkpoint without changing execution or treating other errors as closure.
func TestWatchCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name, status, diagnostic  string
		invalid, missing, wrongID bool
		ok                        bool
	}{
		{name: "blocked", status: "blocked", diagnostic: `{"id":"cli:pane:read","error":{"code":"pane_not_found","message":"Pane missing"}}`, ok: true},
		{name: "complete", status: "complete", diagnostic: `{"id":"cli:pane:read","error":{"code":"pane_not_found","message":"Pane missing"}}`, ok: true},
		{name: "other error", status: "blocked", diagnostic: `{"error":{"code":"permission_denied","message":"pane_not_found in unrelated text"}}`},
		{name: "unstructured error", status: "blocked", diagnostic: "pane_not_found"},
		{name: "invalid checkpoint", status: "blocked", diagnostic: `{"error":{"code":"pane_not_found"}}`, invalid: true},
		{name: "missing checkpoint", status: "blocked", diagnostic: `{"error":{"code":"pane_not_found"}}`, missing: true},
		{name: "changed identity", status: "blocked", diagnostic: `{"error":{"code":"pane_not_found"}}`, wrongID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recoveryEnvironment(t)
			a, fixturePath := setupLoop(t, "normal")
			a.state.Status = "running"
			if err := a.save(); err != nil {
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
			t.Cleanup(lease.close)
			a.runtime = &runRuntime{Terminal: &terminalRef{Backend: "herdr", Session: "fixture-loop", Pane: "recorded-pane"}}
			if err := a.saveRuntime(); err != nil {
				t.Fatal(err)
			}
			r := inspectRun(entry)
			if r.Status != "running" {
				t.Fatalf("fixture did not capture a live view: %+v", r)
			}
			lease.close()
			a.state.Status = tc.status
			if tc.status == "blocked" {
				a.state.Blocker = &runBlocker{Code: "candidate_documents", Message: "Tracker slice identities changed", NextAction: commandAction("looper", "resume", "--run", entry.ID, "--detach")}
			}
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(a.dir, "state.json")
			if tc.invalid {
				if err := os.WriteFile(statePath, []byte("broken JSON"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.missing {
				if err := os.Remove(statePath); err != nil {
					t.Fatal(err)
				}
			}
			if tc.wrongID {
				r.ID = "run-another-configuration"
			}
			bin := t.TempDir()
			stub := "#!/bin/sh\n[ \"$#\" -eq 11 ] && [ \"$1\" = --session ] && [ \"$2\" = fixture-loop ] && [ \"$3\" = pane ] && [ \"$4\" = read ] && [ \"$5\" = recorded-pane ] || exit 64\nprintf '%s\\n' " + shellQuote(tc.diagnostic) + " >&2\nexit 1\n"
			if err := os.WriteFile(filepath.Join(bin, "herdr"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			snapshot := func() map[string]string {
				t.Helper()
				files := make(map[string]string)
				root, err := registryRoot()
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{a.dir, a.path, fixturePath, filepath.Join(root, "runs", entry.ID+".json")} {
					if err := filepath.WalkDir(path, func(path string, d os.DirEntry, err error) error {
						if err != nil || d.IsDir() {
							return err
						}
						b, err := os.ReadFile(path)
						files[path] = string(b)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				return files
			}
			before := snapshot()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var out bytes.Buffer
			err = watchHerdr(ctx, r, &out)
			if (err == nil) != tc.ok {
				t.Fatalf("fallback accepted=%v, want %v: %v; output=%s", err == nil, tc.ok, err, out.String())
			}
			if tc.ok {
				for _, want := range []string{"Checkpoint: " + tc.status, entry.ID} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("retained state missing %q: %s", want, out.String())
					}
				}
				if tc.status == "blocked" && (!strings.Contains(out.String(), "candidate_documents: Tracker slice identities changed") || !strings.Contains(out.String(), actionCommand(a.state.Blocker.NextAction))) {
					t.Fatalf("saved blocker or recovery action missing: %s", out.String())
				}
			} else if out.Len() != 0 {
				t.Fatalf("unverified checkpoint was displayed: %s", out.String())
			}
			if !reflect.DeepEqual(before, snapshot()) {
				t.Fatal("watch changed checkpoint, configuration, registry or worker fixture")
			}
		})
	}
}

// A foreign active loop never becomes a local view, and ambiguous local owners require only a local set selector.
func TestWatchSelection(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	b, _ := setupLoop(t, "normal")
	for _, run := range []*app{a, b} {
		if _, err := registerRun(run); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := acquireRun(b)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	t.Chdir(a.config.Repo)
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--watch"}, "not_running"},
		{[]string{"watch", "--all"}, "invalid_arguments"},
		{[]string{"watch", "--json"}, "invalid_arguments"},
	} {
		err := execute(tc.args)
		var failure *agentError
		if !errors.As(err, &failure) || failure.Response.Status != tc.code || len(failure.Response.Runs) != 0 {
			t.Fatalf("watch selected another project or ignored its contract: %v", err)
		}
	}
	secondConfig := filepath.Join(t.TempDir(), "second.json")
	if err := writeJSON(secondConfig, a.config); err != nil {
		t.Fatal(err)
	}
	second, err := loadApp(secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registerRun(second); err != nil {
		t.Fatal(err)
	}
	// Hold distinct run locks to model conflicting legacy metadata without bypassing repository ownership in production.
	for _, run := range []*app{a, second} {
		if err := os.MkdirAll(run.dir, 0700); err != nil {
			t.Fatal(err)
		}
		owned := &runLease{}
		if err := owned.acquire(filepath.Join(run.dir, "run.lock")); err != nil {
			t.Fatal(err)
		}
		defer owned.close()
	}
	err = execute([]string{"--watch"})
	var failure *agentError
	if !errors.As(err, &failure) || failure.Response.Status != "selection_required" || len(failure.Response.Runs) != 2 {
		t.Fatalf("local ambiguity was not bounded to this project: %v", err)
	}
	for _, r := range failure.Response.Runs {
		if r.Repo != a.config.Repo {
			t.Fatal("foreign active loop leaked into watch selection")
		}
	}
}
