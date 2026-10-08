package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	Repo      string       `json:"repo"`
	Worktree  string       `json:"worktree"`
	Mode      string       `json:"mode"`
	Calls     int          `json:"calls"`
	Implement int          `json:"implement"`
	Prepare   int          `json:"prepare"`
	Repairs   int          `json:"repairs"`
	Merges    int          `json:"merges"`
	Closes    int          `json:"closes"`
	PR        *pullRequest `json:"pr"`
}

func TestMain(m *testing.M) {
	if os.Getenv("SLICE_LOOP_TEST_HELPER") == "1" {
		var err error
		if len(os.Args) > 1 && os.Args[1] == "_resume-detached" {
			err = execute(os.Args[1:])
		} else {
			err = fixtureCommand(filepath.Base(os.Args[0]), os.Args[1:])
		}
		if err != nil {
			os.Exit(reportError(err))
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixtureCommand(tool string, args []string) error {
	path := os.Getenv("SLICE_LOOP_TEST_FIXTURE")
	var f fixture
	if err := readJSON(path, &f); err != nil {
		return err
	}
	save := func() error { return writeJSON(path, f) }
	switch tool {
	case "looper":
		return execute(args)
	case "acta":
		return nil
	case "tea":
		endpoint := args[len(args)-1]
		method := "GET"
		payload := ""
		for i, arg := range args {
			if arg == "--method" {
				method = args[i+1]
			}
			if arg == "--data" {
				payload = args[i+1]
			}
		}
		if method == "POST" {
			var body map[string]any
			if err := json.Unmarshal([]byte(payload), &body); err != nil {
				return err
			}
			if body["Do"] != "rebase" || body["force_merge"] != false || body["head_commit_id"] != f.PR.Head.SHA {
				return fmt.Errorf("invalid merge request: %s", payload)
			}
			f.Merges++
			if f.Mode == "unknown-merge" {
				if err := save(); err != nil {
					return err
				}
				return fmt.Errorf("transport failure before confirmed merge")
			}
			if _, err := git(f.Worktree, "push", "origin", "HEAD:dev"); err != nil {
				return err
			}
			f.PR.Merged = true
			f.PR.State = "closed"
			f.PR.MergedCommit = f.PR.Head.SHA
			if err := save(); err != nil {
				return err
			}
			if f.Mode == "lost-response" {
				return fmt.Errorf("response lost after merge")
			}
			return nil
		}
		if strings.Contains(endpoint, "/pulls?") {
			if f.PR == nil {
				return printJSON([]pullRequest{})
			}
			return printJSON([]pullRequest{*f.PR})
		}
		if strings.Contains(endpoint, "/pulls/") {
			return printJSON(f.PR)
		}
		return fmt.Errorf("unexpected fixture endpoint")
	case "codex":
		if f.Mode == "paused-worker" || f.Mode == "hold-only" {
			if err := os.WriteFile(path+".entered", []byte("ready"), 0600); err != nil {
				return err
			}
			for {
				if _, err := os.Stat(path + ".release"); err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if f.Mode == "hold-only" {
				return nil
			}
		}
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		prompt := string(input)
		phase := ""
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Phase: ") {
				phase = strings.TrimPrefix(line, "Phase: ")
			}
		}
		f.Calls++
		result := workerResult{Status: "continue", Message: "More work remains"}
		if f.Mode == "early-complete" {
			result.Status = "closed"
		} else {
			switch phase {
			case "implement":
				f.Implement++
				if f.Implement == 1 {
					if _, err := git(f.Repo, "worktree", "add", "-b", "feat/flow", f.Worktree); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(f.Worktree, "runtime.txt"), []byte("delivered behavior\n"), 0600); err != nil {
						return err
					}
					if err := fixtureCommit(f.Worktree, "feat: deliver behavior", false); err != nil {
						return err
					}
				} else {
					report := "ops/default/report/slice-1.md"
					if err := os.MkdirAll(filepath.Dir(filepath.Join(f.Worktree, report)), 0700); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(f.Worktree, report), []byte("Status: reviewing\n"), 0600); err != nil {
						return err
					}
					if err := fixtureCommit(f.Worktree, "docs: introduce slice report", false); err != nil {
						return err
					}
					head, err := cleanHead(f.Worktree)
					if err != nil {
						return err
					}
					base, err := git(f.Repo, "rev-parse", "origin/dev")
					if err != nil {
						return err
					}
					p := pullRequest{Number: 1, Title: "feat(slice-1): deliver flow", Body: strings.Replace(canonicalBody(), strings.Repeat("a", 40), head, 1), HTMLURL: "https://forge.example.org/team/app/pulls/1", State: "open", Mergeable: true}
					p.Body = strings.Replace(p.Body, "go test ./...", "test -f runtime.txt", 1)
					p.Head.Ref = "feat/flow"
					p.Head.SHA = head
					p.Base.Ref = "dev"
					p.Base.SHA = base
					p.Head.Repo.FullName = "team/app"
					p.Base.Repo.FullName = "team/app"
					p.Base.Repo.HTMLURL = "https://forge.example.org/team/app"
					if f.Mode == "repair-body" {
						p.Body += "\n\n## Improvised\n\nNo"
					}
					f.PR = &p
					result.Status = "pr_ready"
				}
			case "repair":
				f.Repairs++
				if f.Mode != "repair-body" {
					return fmt.Errorf("unexpected repair: %s", prompt)
				}
				f.PR.Body = strings.Split(f.PR.Body, "\n\n## Improvised")[0]
				result.Status = "pr_ready"
			case "prepare_gate":
				f.Prepare++
				if f.Prepare == 1 {
					if err := os.WriteFile(filepath.Join(f.Repo, "ops/default/report/slice-1.md"), []byte("Status: delivered\n"), 0600); err != nil {
						return err
					}
					if err := fixtureCommit(f.Repo, "docs: deliver slice", true); err != nil {
						return err
					}
				} else {
					result.Status = "ready_for_gate"
				}
			case "close":
				f.Closes++
				for _, p := range []string{"ops/default/plan/flow.md", "ops/default/tracker/flow.md"} {
					b, err := os.ReadFile(filepath.Join(f.Repo, p))
					if err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(f.Repo, p), []byte(strings.Replace(string(b), "Status: Approved", "Status: Completed", 1)), 0600); err != nil {
						return err
					}
				}
				if err := fixtureCommit(f.Repo, "docs: close set", true); err != nil {
					return err
				}
				result.Status = "closed"
			default:
				return fmt.Errorf("unexpected phase %q", phase)
			}
		}
		resultPath := ""
		for i, arg := range args {
			if arg == "-o" {
				resultPath = args[i+1]
			}
		}
		if err := writeJSON(resultPath, result); err != nil {
			return err
		}
		if err := save(); err != nil {
			return err
		}
		fmt.Println(`{"type":"thread.started","thread_id":"11111111-1111-4111-8111-111111111111"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"Preparing the approved delivery."}}`)
		fmt.Println(`{"type":"item.started","item":{"id":"command","type":"command_execution","command":"go test ./... token=private"}}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"command","type":"command_execution","command":"go test ./...","exit_code":0,"aggregated_output":"private output"}}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"reasoning","type":"reasoning","text":"private reasoning"}}`)
		return nil
	}
	return fmt.Errorf("unexpected fixture tool %q", tool)
}

func fixtureCommit(repo, message string, push bool) error {
	if _, err := git(repo, "add", "."); err != nil {
		return err
	}
	if _, err := git(repo, "commit", "-m", message); err != nil {
		return err
	}
	if push {
		_, err := git(repo, "push", "origin", "dev")
		return err
	}
	return nil
}

func setupLoop(t *testing.T, mode string) (*app, string) {
	t.Helper()
	root := t.TempDir()
	repo := newGitFixture(t)
	table := "| Slice | Short name | Exact branch | Expected PR title | Report |\n| --- | --- | --- | --- | --- |\n| Slice 1 | Flow | `feat/flow` | `feat(slice-1): deliver flow` | `ops/default/report/slice-1.md` |\n"
	for _, p := range []string{"ops/default/plan/flow.md", "ops/default/tracker/flow.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, p)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte("Status: Approved\nApproved: 2026-10-07\n\n"+table), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixtureCommit(repo, "docs: approve slice map", false); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "origin.git")
	if _, err := command(root, "git", "init", "--bare", remote); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"remote", "add", "origin", remote}, {"push", "-u", "origin", "dev"}} {
		if _, err := git(repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	f := fixture{Repo: repo, Worktree: filepath.Join(root, "slice"), Mode: mode}
	fixturePath := filepath.Join(root, "fixture.json")
	if err := writeJSON(fixturePath, f); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tea", "codex", "acta", "looper"} {
		if err := os.Symlink(testBinary, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SLICE_LOOP_TEST_HELPER", "1")
	t.Setenv("SLICE_LOOP_TEST_FIXTURE", fixturePath)
	// Every fixture process is waited for; avoid the race runtime's exit delay in each child.
	t.Setenv("GORACE", os.Getenv("GORACE")+" atexit_sleep_ms=0")
	gate := "test -f runtime.txt"
	if mode == "gate-failure" {
		gate = "exit 9"
	}
	c := config{Version: 1, Repo: repo, Origin: remote, Forge: "https://forge.example.org", Repository: "team/app", Plan: "ops/default/plan/flow.md", Tracker: "ops/default/tracker/flow.md", FullGate: gate, MaxStalled: 5, MaxTurns: 20, Slices: []slice{{Number: 1, Branch: "feat/flow", Title: "feat(slice-1): deliver flow", Report: "ops/default/report/slice-1.md"}}, ClosurePaths: []string{"ops/default/plan/flow.md", "ops/default/tracker/flow.md", "ops/default/report/slice-1.md"}}
	path := filepath.Join(root, "run.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	a, err := loadApp(path)
	if err != nil {
		t.Fatal(err)
	}
	return a, fixturePath
}

// TestLoop drives actual child processes and Git integration across premature turns, repair and lost responses.
func TestLoop(t *testing.T) {
	for _, mode := range []string{"normal", "repair-body", "lost-response", "unknown-merge", "early-complete", "gate-failure"} {
		t.Run(mode, func(t *testing.T) {
			a, path := setupLoop(t, mode)
			var activity bytes.Buffer
			if mode == "normal" {
				monitor, stop, err := startActivity(a.path, true, newOutput(&activity, colorAuto))
				if err != nil {
					t.Fatal(err)
				}
				a.activity = monitor
				defer stop()
			}
			err := a.run()
			var f fixture
			if readErr := readJSON(path, &f); readErr != nil {
				t.Fatal(readErr)
			}
			switch mode {
			case "unknown-merge":
				if err == nil || a.state.Pending == nil || f.Merges != 1 {
					t.Fatalf("uncertain mutation not checkpointed: err=%v merges=%d", err, f.Merges)
				}
				if err := a.run(); err == nil {
					t.Fatal("uncertain mutation silently retried")
				}
				if readErr := readJSON(path, &f); readErr != nil {
					t.Fatal(readErr)
				}
				if f.Merges != 1 {
					t.Fatal("uncertain merge submitted twice")
				}
			case "early-complete":
				if err == nil || a.state.Status == "complete" || f.Merges != 0 || f.Calls != 5 {
					t.Fatalf("premature worker completion accepted: %v", err)
				}
			case "gate-failure":
				if err == nil || a.state.Gate != nil || f.Closes != 0 || a.state.Status == "complete" {
					t.Fatal("failed full gate permitted closure")
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
				if a.state.Status != "complete" || f.Merges != 1 || f.Implement != 2 || f.Prepare != 2 || f.Closes != 1 || a.state.Gate == nil {
					t.Fatalf("incomplete cycle: %+v", f)
				}
				if mode == "repair-body" && f.Repairs != 1 {
					t.Fatal("noncanonical body was not repaired")
				}
				if mode == "normal" {
					// The external worker stream and controller gate both produce activity without changing delivery.
					a.activity.mu.Lock()
					output := activity.String()
					a.activity.mu.Unlock()
					for _, want := range []string{"Worker: Preparing the approved delivery.", "Started: go test", "Completed: go test (exit 0)", "Controller validation"} {
						if !strings.Contains(output, want) {
							t.Fatalf("missing activity %q", want)
						}
					}
					if strings.Contains(output, "private") {
						t.Fatal("worker payload or reasoning leaked into activity")
					}
				}
				calls := f.Calls
				if err := a.run(); err != nil {
					t.Fatal(err)
				}
				if err := readJSON(path, &f); err != nil {
					t.Fatal(err)
				}
				if f.Calls != calls || f.Merges != 1 {
					t.Fatal("completed set repeated work")
				}
			}
		})
	}
}
