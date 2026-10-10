package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func finishDetachedFixture(t *testing.T, a *app, fixturePath, backend string) {
	t.Helper()
	_ = os.WriteFile(fixturePath+".release", nil, 0600)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		busy, err := lockBusy(filepath.Join(a.dir, "run.lock"))
		if err == nil && !busy && !launching(a) {
			var launch launchIntent
			if readJSON(filepath.Join(a.dir, "launch.json"), &launch) == nil && backend == "herdr" && launch.Terminal != nil {
				_, err := launch.Terminal.command(a.config.Repo, "session", "stop", launch.Terminal.Session, "--json")
				if err != nil {
					t.Error(err)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("isolated detached fixture did not finish")
}

// A real terminal backend cannot acknowledge a launch whose detached provider check fails.
func TestDetachedPreflightFailure(t *testing.T) {
	for _, backend := range []string{"herdr", "zellij", "tmux"} {
		t.Run(backend, func(t *testing.T) {
			if _, err := exec.LookPath(backend); err != nil {
				t.Skip("backend unavailable")
			}
			recoveryEnvironment(t)
			a, path := setupLoop(t, "normal")
			t.Cleanup(func() { finishDetachedFixture(t, a, path, backend) })
			probe := filepath.Join(t.TempDir(), "provider")
			if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 77\n"), 0700); err != nil {
				t.Fatal(err)
			}
			testHostContext(t, executionContext{Version: 1, ProviderProbe: probe})
			r, code, diagnostic := recoveryCLI("resume", "--config", a.path, "--detach", "--backend", backend, "--json")
			if code != 5 || diagnostic != "" || r.Status != "blocked" || r.Error == nil || r.Error.Class != "authentication" || r.Error.Component != "provider" || r.Run == nil || r.Run.WorkerTurns != 0 {
				t.Fatalf("preflight was accepted: %+v exit=%d stderr=%s", r, code, diagnostic)
			}
			var f fixture
			if err := readJSON(path, &f); err != nil || f.Calls != 0 || f.Merges != 0 {
				t.Fatal("failed launch started delivery", err)
			}
		})
	}
}

// This launcher has a service-like HOME/PATH and no agent socket; the detached child resolves the host profile.
func TestDetachedServiceContext(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	recoveryEnvironment(t)
	a, path := setupLoop(t, "paused-worker")
	t.Cleanup(func() { finishDetachedFixture(t, a, path, "tmux") })
	home := t.TempDir()
	probe := filepath.Join(t.TempDir(), "provider")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\ntest \"$SSH_AUTH_SOCK\" = '/fixture/agent.sock' || exit 77\n"), 0700); err != nil {
		t.Fatal(err)
	}
	locations := map[string]string{"HOME": home, "PATH": os.Getenv("PATH"), "SSH_AUTH_SOCK": "/fixture/agent.sock"}
	for _, key := range []string{"CODEX_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "HERDR_CONFIG_PATH"} {
		locations[key] = os.Getenv(key)
	}
	if err := writeJSON(filepath.Join(home, ".config", "looper", "context.json"), executionContext{Version: 1, Environment: locations, ProviderProbe: probe}); err != nil {
		t.Fatal(err)
	}
	looper := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "looper")
	c := exec.Command(looper, "resume", "--config", a.path, "--detach", "--backend", "tmux", "--json")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if key != "HOME" && key != "PATH" && key != "SSH_AUTH_SOCK" && key != "LOOPER_CONTEXT" {
			c.Env = append(c.Env, item)
		}
	}
	c.Env = append(c.Env, "HOME="+home, "PATH=/usr/bin:/bin", "SSH_AUTH_SOCK=")
	var out, diagnostic bytes.Buffer
	c.Stdout = &out
	c.Stderr = &diagnostic
	if err := c.Run(); err != nil {
		t.Fatalf("service launch failed: %v %s %s", err, out.String(), diagnostic.String())
	}
	var response agentResponse
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Status != "started" || response.Error != nil {
		t.Fatalf("service handshake failed: %+v %v", response, err)
	}
	waitForFile(t, path+".entered")
	if response.Run == nil || response.Run.Status != "running" {
		t.Fatalf("service controller not active: %+v", response)
	}
}

// Detached controllers print their final outcome to the native pane, with history retained only for this fixture.
func TestDetachedLoopOutcome(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	for _, format := range []outputFormat{outputJSONL, outputHuman} {
		for _, failed := range []bool{false, true} {
			name, message, status, closures := "complete", "Loop completed successfully.", "complete", 1
			if failed {
				name, message, status, closures = "failed", "Loop failed: ", "blocked", 0
			}
			t.Run(string(format)+"/"+name, func(t *testing.T) {
				recoveryEnvironment(t)
				a, path := setupLoop(t, "paused-worker")
				if failed {
					a.config.FullGate = "exit 9"
					if err := writeJSON(a.path, a.config); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					finishDetachedFixture(t, a, path, "tmux")
					var launch launchIntent
					if readJSON(filepath.Join(a.dir, "launch.json"), &launch) == nil && launch.Terminal != nil {
						terminal := launch.Terminal
						dead, err := terminal.command(a.config.Repo, "-L", "slice-loop", "display-message", "-p", "-t", "="+terminal.Session+":", "#{pane_dead}")
						if err == nil && dead == "1" {
							if _, err := terminal.command(a.config.Repo, "-L", "slice-loop", "kill-session", "-t", "="+terminal.Session); err != nil {
								t.Error(err)
							}
						}
					}
				})
				r, code, diagnostic := recoveryCLI("resume", "--config", a.path, "--detach", "--backend", "tmux", "--output", string(format), "--color", "always", "--json")
				if code != 0 || diagnostic != "" || r.Status != "started" || r.Run == nil || r.Run.Terminal == nil {
					t.Fatalf("detached launch failed: %+v exit=%d stderr=%s", r, code, diagnostic)
				}
				waitFixtureWorker(t, path)
				if _, err := r.Run.Terminal.command(a.config.Repo, "-L", "slice-loop", "set-option", "-t", "="+r.Run.Terminal.Session+":", "remain-on-exit", "on"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+".release", nil, 0600); err != nil {
					t.Fatal(err)
				}
				var screen string
				deadline := time.Now().Add(15 * time.Second)
				for {
					var err error
					screen, err = r.Run.Terminal.command(a.config.Repo, "-L", "slice-loop", "capture-pane", "-J", "-p", "-t", "="+r.Run.Terminal.Session+":", "-S", "-")
					if err != nil {
						t.Fatal(err)
					}
					var runtime runRuntime
					if err := readJSON(filepath.Join(a.dir, "runtime.json"), &runtime); err != nil {
						t.Fatal(err)
					}
					alive, err := processState(runtime.Controller)
					if err == nil && !alive && strings.Contains(screen, message) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("detached outcome missing or controller still active: %s", screen)
					}
					time.Sleep(20 * time.Millisecond)
				}
				var readable bytes.Buffer
				if _, err := writeWatchSnapshot(&readable, nil, screen); err != nil || strings.Count(readable.String(), message) != 1 {
					t.Fatalf("watch lost or duplicated final outcome: %s %v", readable.String(), err)
				}
				current, err := loadApp(a.path)
				if err != nil || current.state.Status != status {
					t.Fatalf("outcome disagrees with checkpoint: %+v %v", current, err)
				}
				var f fixture
				if err := readJSON(path, &f); err != nil || f.Merges != 1 || f.Closes != closures {
					t.Fatalf("outcome changed delivery: %+v %v", f, err)
				}
			})
		}
	}
}

// The real Git boundary retries temporary transport failures and stops immediately for rejected credentials.
func TestGitAccessFailures(t *testing.T) {
	for _, mode := range []string{"transient", "authentication"} {
		t.Run(mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "normal")
			realGit, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			counter := filepath.Join(root, "attempts")
			message := "connection reset by peer"
			if mode == "authentication" {
				message = "Permission denied (publickey)"
			}
			script := "#!/bin/sh\nif test \"$1\" = ls-remote; then\n n=0; test ! -f " + shellQuote(counter) + " || read n < " + shellQuote(counter) + "\n n=$((n+1)); printf '%s\\n' \"$n\" > " + shellQuote(counter) + "\n if test \"$n\" -lt 3; then printf '%s' " + shellQuote(message) + " >&2; exit 128; fi\nfi\nexec " + shellQuote(realGit) + " \"$@\"\n"
			if err := os.WriteFile(filepath.Join(root, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			testHostContext(t, executionContext{Version: 1, Environment: map[string]string{"PATH": root + ":" + os.Getenv("PATH")}})
			err = a.preflight()
			count, readErr := os.ReadFile(counter)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "transient" && (err != nil || strings.TrimSpace(string(count)) != "3") {
				t.Fatalf("Git recovery failed: %v attempts=%s", err, count)
			}
			if mode == "authentication" && (err == nil || strings.TrimSpace(string(count)) != "1") {
				t.Fatalf("Git auth retried: %v attempts=%s", err, count)
			}
		})
	}
}
