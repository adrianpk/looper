package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Context follows Git identity rather than basename, includes linked worktrees and never falls back to global selection.
func TestProjectContext(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	b, _ := setupLoop(t, "normal")
	first, err := registerRun(a)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := registerRun(b)
	if err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(t.TempDir(), "history.json")
	if err := writeJSON(cfg, a.config); err != nil {
		t.Fatal(err)
	}
	history, err := loadApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := registerRun(history)
	if err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(a.config.Repo, "src")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(t.TempDir(), "linked")
	if _, err := git(a.config.Repo, "worktree", "add", "--detach", worktree); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(subdir, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, dir string }{
		{"root", a.config.Repo}, {"subdirectory", subdir}, {"worktree", worktree}, {"symlink", alias},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(tc.dir)
			list, code, diagnostic := recoveryCLI("list", "--json")
			if code != 0 || diagnostic != "" || len(list.Runs) != 2 {
				t.Fatalf("local discovery failed: %+v %d %s", list, code, diagnostic)
			}
			for _, run := range list.Runs {
				if run.ID != first.ID && run.ID != second.ID {
					t.Fatalf("another project leaked into local discovery: %+v", run)
				}
			}
			status, code, diagnostic := recoveryCLI("status", "--json")
			if code != 3 || status.Status != "selection_required" || len(status.Runs) != 2 || diagnostic != "" {
				t.Fatalf("local ambiguity failed: %+v %d %s", status, code, diagnostic)
			}
			selected, code, diagnostic := recoveryCLI("status", "--set", second.Set, "--json")
			if code != 0 || diagnostic != "" || selected.Run == nil || selected.Run.ID != second.ID {
				t.Fatalf("named local selection failed: %+v %d %s", selected, code, diagnostic)
			}
		})
	}
	t.Chdir(a.config.Repo)
	lease, err := acquireRun(a)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.close()
	for _, args := range [][]string{
		{"status", "--json"}, {"resume", "--detach", "--json"}, {"run", "--detach", "--json"},
	} {
		r, code, diagnostic := recoveryCLI(args...)
		if code != 0 || diagnostic != "" || r.Run == nil || r.Run.ID != first.ID {
			t.Fatalf("active owner did not supersede local history: %+v %d %s", r, code, diagnostic)
		}
	}
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"list", "--json"}, {"status", "--json"}, {"resume", "--detach", "--json"}} {
		r, code, diagnostic := recoveryCLI(args...)
		if code != 2 || r.Status != "project_required" || diagnostic != "" {
			t.Fatalf("outside-directory selection fell back globally: %+v %d %s", r, code, diagnostic)
		}
	}
	global, code, diagnostic := recoveryCLI("list", "--all", "--json")
	if code != 0 || diagnostic != "" || len(global.Runs) != 3 {
		t.Fatalf("explicit global discovery failed: %+v %d %s", global, code, diagnostic)
	}
	exact, code, diagnostic := recoveryCLI("status", "--run", foreign.ID, "--json")
	if code != 0 || diagnostic != "" || exact.Run == nil || exact.Run.ID != foreign.ID {
		t.Fatalf("explicit run selection stopped working: %+v %d %s", exact, code, diagnostic)
	}
}

// Old registry entries retain their exact run identity and checkpoint; registering again writes only the new index.
func TestLegacyRegistry(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	root, err := registryRoot()
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(filepath.Dir(root), "slice-loop")
	a.path = filepath.Join(legacy, "previous.json")
	a.dir = a.path + ".state"
	if err := writeJSON(a.path, a.config); err != nil {
		t.Fatal(err)
	}
	a.state.Session = "11111111-1111-4111-8111-111111111111"
	a.state.Turns = 2
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := entryFor(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(legacy, "runs", e.ID+".json"), e); err != nil {
		t.Fatal(err)
	}
	runs, err := knownRuns()
	if err != nil || len(runs) != 1 || runs[0].ID != e.ID || runs[0].Config != a.path {
		t.Fatalf("legacy identity changed: %+v %v", runs, err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery wrote or moved legacy state")
	}
	registered, err := registerRun(a)
	if err != nil || registered.ID != e.ID {
		t.Fatalf("new registration changed identity: %+v %v", registered, err)
	}
	runs, err = knownRuns()
	if err != nil || len(runs) != 1 {
		t.Fatalf("legacy/new index was not deduplicated: %+v %v", runs, err)
	}
	after, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("discovery or registration changed the active checkpoint")
	}
}
