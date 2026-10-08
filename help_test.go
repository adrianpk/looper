package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Agents can discover preferences and recovery capabilities without loading or advancing a run.
func TestPreferredHelp(t *testing.T) {
	recoveryEnvironment(t)
	_, fixturePath := setupLoop(t, "normal")
	for _, args := range [][]string{
		{"--help"}, {"-h"}, {"help"},
		{"init", "--help"}, {"run", "--help"}, {"activity", "--help"},
		{"status", "--help"}, {"list", "--help"}, {"register", "--help"},
		{"resume", "--help"}, {"check", "--help"}, {"validate", "--help"},
		{"watch", "--help"}, {"--watch", "--help"},
		{"recover", "--help"}, {"resume", "--run", "missing", "-h"},
		{"version", "--help"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			c := exec.Command("looper", args...)
			var out, diagnostic bytes.Buffer
			c.Stdout, c.Stderr = &out, &diagnostic
			if err := c.Run(); err != nil || diagnostic.Len() != 0 {
				t.Fatalf("help failed: %v stdout=%q stderr=%q", err, out.String(), diagnostic.String())
			}
			for _, capability := range []string{"looper", "looper --watch", "list --all", "current Git worktree", "backend: auto", "herdr > zellij > tmux", "color: always", "output: jsonl", "--output jsonl|human", "jq --unbuffered .", "--color never", "--activity", "--json", "view_command", "selection_required", "already_running"} {
				if !strings.Contains(out.String(), capability) {
					t.Errorf("help omits %q", capability)
				}
			}
			if strings.Contains(out.String(), "\x1b") {
				t.Fatal("help must remain copyable plain text")
			}
		})
	}
	var f fixture
	if err := readJSON(fixturePath, &f); err != nil || f.Calls != 0 || f.Merges != 0 {
		t.Fatalf("help performed delivery work: %+v %v", f, err)
	}
}

// CLI defaults force human colors through both flag parsers, while explicit overrides and JSON stay plain.
func TestPreferredColors(t *testing.T) {
	recoveryEnvironment(t)
	t.Setenv("NO_COLOR", "1")
	a, _ := setupLoop(t, "normal")
	t.Chdir(a.config.Repo)
	e, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		colored bool
	}{
		{"list default", []string{"list"}, true},
		{"status default", []string{"status", "--run", e.ID}, true},
		{"disabled", []string{"list", "--color", "never"}, false},
		{"automatic", []string{"list", "--color", "auto"}, false},
		{"json default", []string{"list", "--json"}, false},
		{"foreground default", []string{"run", "--config", a.path, "--activity", "--output", "human"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := exec.Command("looper", tc.args...).CombinedOutput()
			if err != nil {
				t.Fatalf("command failed: %v %s", err, out)
			}
			if strings.Contains(string(out), "\x1b[") != tc.colored {
				t.Fatalf("unexpected color preference: %q", out)
			}
		})
	}
}
