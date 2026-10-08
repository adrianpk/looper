package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The same binary opens the exact project's native view without shell parsing, worker input or checkpoint writes.
func TestWatch(t *testing.T) {
	const session = "loop 'quoted' $(false)"
	for _, tc := range []struct {
		backend string
		args    []string
	}{
		{"herdr", []string{"session", "attach", "--", session}},
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
			t.Setenv("HERDR_SOCKET_PATH", "/outer/socket")
			t.Setenv("TMUX", "outer")
			dir := t.TempDir()
			stub := "#!/bin/sh\n[ -z \"${HERDR_SOCKET_PATH-}${TMUX-}\" ] || exit 78\nprintf '%s\\n' \"$@\"\n"
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
