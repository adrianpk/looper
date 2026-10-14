package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	Repo                 string          `json:"repo"`
	Worktree             string          `json:"worktree"`
	Mode                 string          `json:"mode"`
	Calls                int             `json:"calls"`
	Implement            int             `json:"implement"`
	Prepare              int             `json:"prepare"`
	Repairs              int             `json:"repairs"`
	Merges               int             `json:"merges"`
	Closes               int             `json:"closes"`
	CloseTask            string          `json:"close_task,omitempty"`
	WorkerPhases         []string        `json:"worker_phases,omitempty"`
	WorkerSessions       []string        `json:"worker_sessions,omitempty"`
	Failures             int             `json:"failures,omitempty"`
	ValidationCommands   []string        `json:"validation_commands,omitempty"`
	ValidationAck        string          `json:"validation_ack,omitempty"`
	Validator            processIdentity `json:"validator,omitempty"`
	ValidationRepairPath string          `json:"validation_repair_path,omitempty"`
	PR                   *pullRequest    `json:"pr"`
	History              []pullRequest   `json:"history,omitempty"`
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
		if len(args) > 0 && args[0] == "_hold-validation" {
			return fixtureValidationOwner(args[1:])
		}
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
		if method == "GET" && endpoint == "user" {
			return printJSON(map[string]any{"id": 1})
		}
		if method == "GET" && endpoint == "repos/team/app" {
			return printJSON(map[string]any{"allow_rebase": true, "permissions": map[string]bool{"push": true}})
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
			if f.Mode == "unknown-merge" || f.Mode == "gate-unknown-merge" && strings.HasPrefix(f.PR.Head.Ref, "fix/gate-") {
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
			if strings.HasPrefix(f.PR.Head.Ref, "fix/gate-") {
				f.PR.Base.SHA = f.PR.Head.SHA
			}
			if err := save(); err != nil {
				return err
			}
			if f.Mode == "lost-response" || f.Mode == "gate-lost-response" && strings.HasPrefix(f.PR.Head.Ref, "fix/gate-") {
				return fmt.Errorf("response lost after merge")
			}
			return nil
		}
		if strings.Contains(endpoint, "/pulls?") {
			parsed, _ := url.Parse(endpoint)
			prs := []pullRequest{}
			candidates := append([]pullRequest{}, f.History...)
			if f.PR != nil {
				candidates = append(candidates, *f.PR)
			}
			for _, p := range candidates {
				if p.Head.Ref == parsed.Query().Get("head") {
					prs = append(prs, p)
				}
			}
			return printJSON(prs)
		}
		if strings.Contains(endpoint, "/pulls/") {
			if f.Mode == "closure-auth" {
				return fmt.Errorf("401 Unauthorized token=private-value")
			}
			if f.Mode == "closure-transient" {
				return fmt.Errorf("503 Service Unavailable")
			}
			number, _ := strconv.Atoi(filepath.Base(endpoint))
			for _, p := range f.History {
				if p.Number == number {
					return printJSON(p)
				}
			}
			return printJSON(f.PR)
		}
		return fmt.Errorf("unexpected fixture endpoint")
	case "codex":
		if len(args) != 0 && args[0] == "app-server" {
			if strings.Join(args, " ") != "app-server --listen stdio://" {
				return fmt.Errorf("unexpected provider probe arguments")
			}
			scanner := bufio.NewScanner(os.Stdin)
			methods := []string{"initialize", "initialized", "account/read", "account/rateLimits/read"}
			for _, method := range methods {
				if !scanner.Scan() {
					return fmt.Errorf("provider probe closed before %s", method)
				}
				var request struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &request); err != nil || request.Method != method {
					return fmt.Errorf("unexpected provider RPC; expected %s", method)
				}
				if method == "initialized" {
					continue
				}
				result := map[string]any{}
				if method == "account/read" {
					result["account"] = map[string]string{"type": "chatgpt"}
					if f.Mode == "provider-api" {
						result["account"] = map[string]string{"type": "apiKey"}
					}
					if f.Mode == "provider-missing" {
						result["account"] = nil
					}
				}
				if method == "account/rateLimits/read" {
					result["rateLimits"] = map[string]any{"primary": map[string]int{"usedPercent": 10}}
					if f.Mode == "provider-quota" {
						result["rateLimits"] = map[string]any{"primary": map[string]int{"usedPercent": 100}}
					}
					if f.Mode == "provider-auth" || f.Mode == "provider-transient" {
						message := "401 Unauthorized private-value"
						if f.Mode == "provider-transient" {
							message = "503 Service Unavailable private-value"
						}
						return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": message}})
					}
				}
				if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"id": request.ID, "result": result}); err != nil {
					return err
				}
			}
			return nil
		}
		if f.Mode == "worker-auth" {
			return fmt.Errorf("401 Unauthorized token=private-value")
		}
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
		task := ""
		configPath := ""
		var cfg config
		for _, line := range strings.Split(prompt, "\n") {
			if path, ok := strings.CutPrefix(line, "Configuration: "); ok {
				configPath = path
				if err := readJSON(path, &cfg); err != nil {
					return err
				}
			}
			if strings.HasPrefix(line, "Phase: ") {
				phase = strings.TrimPrefix(line, "Phase: ")
			}
			if value, ok := strings.CutPrefix(line, "Current controller instruction: "); ok {
				task = value
			}
		}
		f.Calls++
		failed := f.Mode == "capacity-always" || (f.Mode == "capacity-once" || f.Mode == "capacity-partial") && f.Calls == 1 || f.Mode == "capacity-gate" && phase == "repair_gate" && f.Failures == 0 || f.Mode == "capacity-close" && phase == "close" && f.Failures == 0 || f.Mode == "capacity-prepare" && phase == "prepare_gate" && f.Prepare == 1 && f.Failures == 0 || f.Mode == "unknown-json" || f.Mode == "authentication-json"
		if strings.HasPrefix(f.Mode, "capacity-") || strings.HasPrefix(f.Mode, "validation-") || strings.HasSuffix(f.Mode, "-json") {
			f.WorkerPhases = append(f.WorkerPhases, phase)
			session := "new"
			if len(args) > 1 && args[1] == "resume" {
				session = args[len(args)-2]
			}
			f.WorkerSessions = append(f.WorkerSessions, session)
		}
		result := workerResult{Status: "continue", Message: "More work remains"}
		if f.Mode == "early-complete" {
			result.Status = "closed"
		} else if !failed || f.Mode == "capacity-partial" {
			switch phase {
			case "implement":
				f.Implement++
				if f.Implement%2 == 1 {
					if f.PR != nil {
						f.History = append(f.History, *f.PR)
						f.PR = nil
						f.Worktree += "-next"
					}
					if _, err := git(f.Repo, "worktree", "add", "-b", cfg.Slices[0].Branch, f.Worktree); err != nil {
						return err
					}
					if err := os.WriteFile(filepath.Join(f.Worktree, "runtime.txt"), []byte("delivered behavior: "+cfg.Slices[0].Branch+"\n"), 0600); err != nil {
						return err
					}
					if err := fixtureCommit(f.Worktree, "feat: deliver behavior", false); err != nil {
						return err
					}
				} else {
					report := cfg.Slices[0].Report
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
					p := pullRequest{Number: len(f.History) + 1, Title: cfg.Slices[0].Title, Body: strings.Replace(canonicalBody(), strings.Repeat("a", 40), head, 1), HTMLURL: "https://forge.example.org/team/app/pulls/1", State: "open", Mergeable: true}
					p.HTMLURL = fmt.Sprintf("https://forge.example.org/team/app/pulls/%d", p.Number)
					p.Body = strings.ReplaceAll(p.Body, "ops/default/report/slice-1.md", cfg.Slices[0].Report)
					p.Body = strings.Replace(p.Body, "go test ./...", "test -f runtime.txt", 1)
					p.Head.Ref = cfg.Slices[0].Branch
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
				if f.Mode == "validation-failure" {
					if err := os.WriteFile(f.ValidationRepairPath, []byte("restored\n"), 0600); err != nil {
						return err
					}
					result.Status = "pr_ready"
					break
				}
				if f.Mode != "repair-body" {
					return fmt.Errorf("unexpected repair: %s", prompt)
				}
				f.PR.Body = strings.Split(f.PR.Body, "\n\n## Improvised")[0]
				result.Status = "pr_ready"
			case "repair_gate":
				f.Repairs++
				if f.Mode == "gate-blocked" {
					result = workerResult{Status: "blocked", Message: "Correction requires an external decision"}
					break
				}
				if f.Mode == "gate-stalled" {
					break
				}
				a, err := loadApp(configPath)
				if err != nil {
					return err
				}
				r := a.activeGateRepair()
				if r == nil {
					return fmt.Errorf("repair worker has no pinned identity")
				}
				if f.PR != nil && !f.PR.Merged {
					return fmt.Errorf("fixture cannot repair an existing PR")
				}
				f.History = append(f.History, *f.PR)
				if _, err := git(f.Worktree, "switch", "-c", r.Identity.Branch, "origin/dev"); err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(f.Worktree, "runtime.txt"), []byte("repaired\n"+r.Identity.Branch+"\n"), 0600); err != nil {
					return err
				}
				if f.Mode == "gate-unauthorized" {
					if err := os.WriteFile(filepath.Join(f.Worktree, "unrelated.txt"), []byte("outside repair authority\n"), 0600); err != nil {
						return err
					}
				}
				if err := fixtureCommit(f.Worktree, "fix: correct gate regression", false); err != nil {
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
				intro, err := git(f.Worktree, "log", "--diff-filter=A", "--format=%H", "--", r.Identity.Report)
				if err != nil {
					return err
				}
				p := *f.PR
				p.Number = len(f.History) + 1
				p.Title = r.Identity.Title
				p.HTMLURL = fmt.Sprintf("https://forge.example.org/team/app/pulls/%d", p.Number)
				p.Head.Ref, p.Head.SHA, p.Base.SHA = r.Identity.Branch, head, base
				p.Merged, p.MergedCommit, p.State = false, "", "open"
				p.Body = strings.Replace(canonicalBody(), strings.Repeat("a", 40), intro, 1)
				p.Body = strings.Replace(p.Body, "go test ./...", "test -f runtime.txt", 1)
				f.PR = &p
				result.Status = "pr_ready"
			case "prepare_gate":
				f.Prepare++
				if f.Prepare%2 == 1 {
					if err := os.WriteFile(filepath.Join(f.Repo, cfg.Slices[0].Report), []byte("Status: delivered\n"), 0600); err != nil {
						return err
					}
					if status, err := git(f.Repo, "status", "--porcelain"); err != nil {
						return err
					} else if status != "" {
						if err := fixtureCommit(f.Repo, "docs: deliver slice", true); err != nil {
							return err
						}
					}
				} else {
					result.Status = "ready_for_gate"
				}
			case "close":
				f.Closes++
				f.CloseTask = task
				if f.Mode == "closure-feedback" {
					result = workerResult{Status: "blocked", Message: "Closure instruction captured"}
					break
				}
				if f.Mode == "closure-stalled" {
					break
				}
				for _, p := range []string{cfg.Plan, cfg.Tracker} {
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
		if failed {
			f.Failures++
			if err := save(); err != nil {
				return err
			}
			session := cfg.Session
			if session == "" {
				session = "11111111-1111-4111-8111-111111111111"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"type": "thread.started", "thread_id": session})
			message := "Selected model is at capacity. Please try a different model."
			if f.Mode == "unknown-json" {
				message = "Unknown execution failure token=private-value"
			}
			if f.Mode == "authentication-json" {
				message = "401 Unauthorized token=private-value"
			}
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "error", "message": message})
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "turn.failed", "error": map[string]string{"message": message}})
			os.Exit(1) // Match Codex exec: error events on stdout, no failure diagnostic on stderr.
		}
		if strings.HasPrefix(f.Mode, "validation-") && phase == "implement" && f.Implement%2 == 0 {
			prefix, _, _ := strings.Cut(f.PR.Body, "\n\n## Validation\n\n")
			var checks []string
			for _, command := range f.ValidationCommands {
				checks = append(checks, "- `"+command+"`")
			}
			f.PR.Body = prefix + "\n\n## Validation\n\n" + strings.Join(checks, "\n")
			if err := save(); err != nil {
				return err
			}
			validationArgs := []string{"validate", "--config", configPath, "--worktree", f.Worktree}
			for _, command := range f.ValidationCommands {
				validationArgs = append(validationArgs, "--command", command)
			}
			validator := exec.Command("looper", validationArgs...)
			validatorLog, err := os.Create(configPath + ".validator-output")
			if err != nil {
				return err
			}
			validator.Stdout, validator.Stderr = validatorLog, validatorLog
			if err := validator.Start(); err != nil {
				_ = validatorLog.Close()
				return err
			}
			_ = validatorLog.Close()
			ack, err := os.Open(f.ValidationAck)
			if err != nil {
				return err
			}
			scanner := bufio.NewScanner(ack)
			if !scanner.Scan() {
				_ = ack.Close()
				return fmt.Errorf("validation did not acknowledge startup: %v", scanner.Err())
			}
			_ = ack.Close()
			f.Validator, err = identifyProcess(validator.Process.Pid)
			if err != nil {
				return err
			}
			if f.Mode == "validation-finished" {
				if err := validator.Wait(); err != nil {
					return err
				}
			} else {
				_ = validator.Process.Release()
			}
			result.Status = "continue"
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
		session := cfg.Session
		if session == "" {
			session = "11111111-1111-4111-8111-111111111111"
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{"type": "thread.started", "thread_id": session}); err != nil {
			return err
		}
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
	// Fixtures must never resolve synthetic conversations against the host daemon or credential store.
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	profile := filepath.Join(root, "host-context.json")
	if err := writeJSON(profile, executionContext{Version: 1}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_CONTEXT", profile)
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
