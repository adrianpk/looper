package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Capability failures may select the next backend; an explicit selection never does.
func TestBackendSelection(t *testing.T) {
	for _, tc := range []struct {
		name, requested, herdr, zellij, tmux, want string
	}{
		{"preferred", "auto", "printf 'protocol: 20'", "exit 0", "exit 0", "herdr"},
		{"second", "auto", "exit 1", "printf '%s' '--create-background INITIAL_COMMAND'", "exit 0", "zellij"},
		{"third", "auto", "exit 1", "exit 1", "exit 0", "tmux"},
		{"explicit", "tmux", "printf 'protocol: 20'", "exit 0", "exit 0", "tmux"},
		{"unavailable", "herdr", "exit 1", "printf '%s' '--create-background INITIAL_COMMAND'", "exit 0", ""},
		{"none", "auto", "exit 1", "exit 1", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{"herdr": tc.herdr, "zellij": tc.zellij, "tmux": tc.tmux} {
				if body != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Setenv("PATH", dir)
			got, err := selectBackend(tc.requested)
			var unavailable *backendError
			if got != tc.want || (tc.want == "" && !errors.As(err, &unavailable)) || (tc.want != "" && err != nil) {
				t.Fatalf("selected %q, error %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A failure after selection must not launch a second backend or advance a checkpoint.
func TestBackendLaunchFailure(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	herdr := `#!/bin/sh
case "$*" in
  'api schema') printf 'protocol: 20';;
  *'status --json') printf '%s' '{"server":{"running":true,"compatible":true,"socket":"/unused"}}';;
  *'workspace create'*) exit 1;;
  *) exit 9;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(herdr), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "unexpected")
	for _, name := range []string{"zellij", "tmux"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf called > "+shellQuote(marker)+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, code, diagnostic := recoveryCLI("resume", "--config", a.path, "--detach", "--backend", "auto", "--json", "--color", "always")
	if code != 5 || diagnostic != "" || r.Error == nil || r.Error.Code != "launch_failed" {
		t.Fatalf("unexpected failure: %+v %d %s", r.Error, code, diagnostic)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launch fell back after a backend had been selected")
	}
	after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("backend failure advanced delivery progress")
	}
}

// Old tmux metadata remains readable while new backend metadata cannot name arbitrary executables.
func TestTerminalMetadata(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	e, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{TmuxSession: "old-session"}); err != nil {
		t.Fatal(err)
	}
	r := inspectRun(e)
	if r.Terminal == nil || r.Terminal.Backend != "tmux" || r.ViewCommand != watchCommand(e.ID) {
		t.Fatalf("old tmux view was lost: %+v", r)
	}
	old := launchIntent{Session: "old-session"}.terminal()
	if old == nil || old.Backend != "tmux" || old.Session != "old-session" {
		t.Fatal("legacy launch metadata was lost")
	}
	if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{Terminal: &terminalRef{Backend: "unexpected", Session: "session"}}); err != nil {
		t.Fatal(err)
	}
	r = inspectRun(e)
	if r.Status != "invalid" || r.Blocker == nil || r.Blocker.Code != "invalid_runtime" {
		t.Fatal("invalid backend metadata accepted")
	}
}

// Launching from an outer multiplexer must target only the explicitly named new session.
func TestTerminalEnvironment(t *testing.T) {
	t.Setenv("HERDR_SOCKET", "/outer")
	t.Setenv("HERDR_PANE_ID", "outer-pane")
	t.Setenv("HERDR_CONFIG_PATH", "/settings.toml")
	t.Setenv("ZELLIJ_SESSION_NAME", "outer-session")
	t.Setenv("TMUX", "outer-socket")
	t.Setenv("NO_COLOR", "1")
	env := strings.Join(terminalEnvironment(), "\n")
	for _, key := range []string{"HERDR_SOCKET=", "HERDR_PANE_ID=", "ZELLIJ_SESSION_NAME=", "TMUX="} {
		if strings.Contains(env, key) {
			t.Fatalf("inherited outer identity %s", key)
		}
	}
	if !strings.Contains(env, "HERDR_CONFIG_PATH=/settings.toml") || !strings.Contains(env, "NO_COLOR=1") {
		t.Fatal("lost configuration or display preference")
	}
}
