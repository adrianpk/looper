package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testHostContext(t *testing.T, c executionContext) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_CONTEXT", path)
	return path
}

// A service resolves host references itself, even when its parent lacks PATH or the agent socket.
func TestHostExecutionContext(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	path := os.Getenv("PATH")
	stateHome := t.TempDir()
	testHostContext(t, executionContext{Version: 1, Environment: map[string]string{"PATH": path, "SSH_AUTH_SOCK": "/host/agent.sock", "XDG_STATE_HOME": stateHome}})
	t.Setenv("PATH", "/missing")
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("TMUX", "outer-session")
	if err := a.preflight(); err != nil {
		t.Fatal(err)
	}
	env, err := executionEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if environmentValue(env, "SSH_AUTH_SOCK") != "/host/agent.sock" || environmentValue(env, "PATH") != path || environmentValue(env, "HERDR_ENV") != "" || environmentValue(env, "TMUX") != "" {
		t.Fatal("execution did not use the shared host context")
	}
	forwarded, err := launchEnvironment()
	if err != nil || environmentValue(forwarded, "SSH_AUTH_SOCK") != "/host/agent.sock" || environmentValue(forwarded, "LOOPER_CONTEXT") == "" {
		t.Fatal("launcher lost host references", err)
	}
	if a.state.Turns != 0 || a.state.Session != "" {
		t.Fatal("preflight spent worker state")
	}
	if root, err := registryRoot(); err != nil || root != filepath.Join(stateHome, "looper") {
		t.Fatalf("launcher and controller discovery diverged: %s %v", root, err)
	}
}

// Git keeps its SSH transport when a gate narrows PATH, without requiring SSH for local remotes.
func TestGitSSHEnvironment(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	realShell, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, toolDir   string
		explicit, local bool
	}{
		{"default", "tools", false, false},
		{"quoted path", "tools with 'quotes", false, false},
		{"explicit command", "custom tools", true, false},
		{"local without ssh", "tools", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tools := filepath.Join(root, tc.toolDir)
			restricted := filepath.Join(root, "restricted")
			for _, dir := range []string{tools, restricted} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
				for name, executable := range map[string]string{"git": realGit, "sh": realShell} {
					if err := os.Symlink(executable, filepath.Join(dir, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
			sshPath := filepath.Join(tools, "ssh")
			if tc.explicit {
				sshPath = filepath.Join(tools, "custom ssh")
			}
			arguments := filepath.Join(root, "ssh-arguments")
			if !tc.local {
				// An empty upload-pack advertisement exercises real Git without a network or credentials.
				script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(arguments) + "\nprintf '0000'\n"
				if err := os.WriteFile(sshPath, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			explicit := ""
			if tc.explicit {
				explicit = shellQuote(sshPath) + " --fixture-option 'kept value'"
			}
			t.Setenv("PATH", restricted)
			t.Setenv("GIT_SSH_COMMAND", explicit)
			t.Setenv("GIT_SSH_VARIANT", "ssh")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			testHostContext(t, executionContext{Version: 1, Environment: map[string]string{
				"HOME": root, "PATH": tools, "SSH_AUTH_SOCK": "",
				"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_SYSTEM": "/dev/null",
			}})
			remote := "ssh://fixture.invalid/repo"
			if tc.local {
				remote = filepath.Join(root, "remote.git")
				child, err := executionCommand(context.Background(), root, "git", "init", "--bare", "--quiet", remote)
				if err != nil {
					t.Fatal(err)
				}
				if out, err := child.CombinedOutput(); err != nil {
					t.Fatalf("create local remote: %v\n%s", err, out)
				}
			}
			gate := "PATH=" + shellQuote(restricted) + "; export PATH; git ls-remote " + shellQuote(remote)
			child, err := executionCommand(context.Background(), root, "sh", "-ec", gate)
			if err != nil {
				t.Fatal(err)
			}
			if tc.explicit && environmentValue(child.Env, "GIT_SSH_COMMAND") != explicit {
				t.Fatal("explicit SSH command changed")
			}
			if out, err := child.CombinedOutput(); err != nil {
				t.Fatalf("gate Git access after PATH replacement: %v\n%s", err, out)
			}
			if !tc.local {
				data, err := os.ReadFile(arguments)
				if err != nil {
					t.Fatal(err)
				}
				prefix := "-o\nBatchMode=yes\n-o\nConnectTimeout=10\n"
				if tc.explicit {
					prefix = "--fixture-option\nkept value\n"
				}
				if !strings.HasPrefix(string(data), prefix) {
					t.Fatalf("unexpected SSH arguments: %s", data)
				}
			}
		})
	}
}

// Context files accept location references, never arbitrary secret environment values.
func TestContextConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    executionContext
	}{
		{"secret", executionContext{Version: 1, Environment: map[string]string{"OPENAI_API_KEY": "private-value"}}},
		{"relative path", executionContext{Version: 1, Environment: map[string]string{"PATH": ".:/usr/bin"}}},
		{"relative socket", executionContext{Version: 1, Environment: map[string]string{"SSH_AUTH_SOCK": "agent.sock"}}},
		{"probe", executionContext{Version: 1, ProviderProbe: "relative-script"}},
		{"relative executable", executionContext{Version: 1, Executables: []string{"./tools/check"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testHostContext(t, tc.c)
			_, err := executionEnvironment()
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Class != "configuration" || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("unsafe context result: %v", err)
			}
		})
	}
}

// Account metadata alone is insufficient: online refusal, exhausted quota and unsupported auth all block.
func TestProviderAccess(t *testing.T) {
	for _, tc := range []struct {
		mode, class string
		attempts    int
	}{
		{"provider-auth", "authentication", 1}, {"provider-missing", "authentication", 1},
		{"provider-quota", "resource", 1}, {"provider-api", "configuration", 1},
		{"provider-transient", "transient", 3},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, tc.mode)
			err := a.providerAccess()
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Class != tc.class || failure.Blocker.Attempts != tc.attempts || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("provider result: %v", err)
			}
		})
	}
}

// Opt-in host validation reads the real provider account without creating a delivery conversation.
func TestProviderAccessLive(t *testing.T) {
	if os.Getenv("LOOPER_TEST_LIVE_PROVIDER") != "1" {
		t.Skip("set LOOPER_TEST_LIVE_PROVIDER=1 for the host account check")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	a := &app{config: config{Repo: dir}}
	if err := a.providerAccess(); err != nil {
		t.Fatal(err)
	}
}

// Recovery cannot redirect an existing conversation to another credential or conversation store.
func TestWorkerHomeRecovery(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	original := os.Getenv("CODEX_HOME")
	if err := writeJSON(filepath.Join(a.dir, "runtime.json"), runRuntime{WorkerHome: original}); err != nil {
		t.Fatal(err)
	}
	profile := testHostContext(t, executionContext{Version: 1, Environment: map[string]string{"CODEX_HOME": t.TempDir()}})
	err := workerIdle(a)
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Class != "configuration" || a.state.Turns != 0 {
		t.Fatalf("conversation store changed silently: %v", err)
	}
	if err := a.work("implement", "Continue approved work"); err == nil || a.state.Turns != 0 {
		t.Fatal("active continuation spent a turn in a different worker store")
	}
	if err := writeJSON(profile, executionContext{Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := workerIdle(a); err != nil {
		t.Fatal(err)
	}
}

// Authentication can fail after preflight; a failed worker turn stays spent and retains its conversation.
func TestWorkerAuthenticationFailure(t *testing.T) {
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "worker-auth")
	a.state.Session = "11111111-1111-4111-8111-111111111111"
	a.state.Turns = 2
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	err := runController(a, controllerOptions{})
	var failure *executionFailure
	if !errors.As(err, &failure) || failure.Blocker.Component != "worker" || failure.Blocker.Class != "authentication" {
		t.Fatalf("worker failure misclassified: %v", err)
	}
	retained, err := loadApp(a.path)
	if err != nil || retained.state.Turns != 3 || retained.state.Session != a.state.Session {
		t.Fatal("worker failure reset budget or conversation", err)
	}
	data, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
	if err != nil || strings.Contains(string(data), "private-value") {
		t.Fatal("worker diagnostic leaked", err)
	}
}

// Only recognized transient reads retry. Exit 128 alone does not establish a transient failure.
func TestAccessRetryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, message, class string
		attempts             int
	}{
		{"temporary", "connection reset by peer", "transient", 3},
		{"deadline", "context deadline exceeded", "transient", 3},
		{"authentication", "Permission denied (publickey): private-value", "authentication", 1},
		{"configuration", "fatal: exit status 128", "access", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err := accessRead("git", func(context.Context) (string, error) { calls++; return "", errors.New(tc.message) })
			var failure *executionFailure
			if !errors.As(err, &failure) || calls != tc.attempts || failure.Blocker.Attempts != calls || failure.Blocker.Class != tc.class || strings.Contains(err.Error(), "private-value") {
				t.Fatalf("unexpected retry outcome: %v calls=%d", err, calls)
			}
		})
	}
	calls := 0
	value, err := accessRead("forge", func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("503 service unavailable")
		}
		return "recovered", nil
	})
	if err != nil || value != "recovered" || calls != 3 {
		t.Fatalf("transient recovery: %q %v %d", value, err, calls)
	}
}

// Startup acknowledgement follows the provider check, and failures preserve conversation and budgets.
func TestPreflightAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exit     int
		class    string
		attempts int
	}{
		{"auth", 77, "authentication", 1}, {"config", 78, "configuration", 1}, {"temporary", 75, "transient", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recoveryEnvironment(t)
			a, _ := setupLoop(t, "normal")
			a.state.Session = "retained-conversation"
			a.state.Turns = 3
			a.state.Stalled = 1
			if err := a.save(); err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(t.TempDir(), "provider")
			if err := os.WriteFile(probe, []byte(fmt.Sprintf("#!/bin/sh\nprintf 'private-value' >&2\nexit %d\n", tc.exit)), 0700); err != nil {
				t.Fatal(err)
			}
			testHostContext(t, executionContext{Version: 1, ProviderProbe: probe})
			if err := runController(a, controllerOptions{LaunchID: "preflight-test"}); err == nil {
				t.Fatal("failed preflight accepted")
			}
			var ready launchReady
			if err := readJSON(filepath.Join(a.dir, "launch-ready.json"), &ready); err != nil {
				t.Fatal(err)
			}
			if ready.Status != "blocked" || ready.Blocker == nil || ready.Blocker.Class != tc.class || ready.Blocker.Attempts != tc.attempts {
				t.Fatalf("invalid handshake: %+v", ready)
			}
			retained, err := loadApp(a.path)
			if err != nil || retained.state.Turns != 3 || retained.state.Stalled != 1 || retained.state.Session != "retained-conversation" || retained.state.Pending != nil || len(retained.state.Merged) != 0 {
				t.Fatalf("preflight changed progress: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(a.dir, "state.json"))
			if err != nil || strings.Contains(string(data), "private-value") {
				t.Fatal("probe output persisted", err)
			}
		})
	}
}

// Real SSH-agent protocol distinguishes a reachable socket from a usable identity.
func TestSSHAgentAccess(t *testing.T) {
	for _, name := range []string{"ssh-agent", "ssh-add", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable")
		}
	}
	recoveryEnvironment(t)
	a, _ := setupLoop(t, "normal")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	socket := filepath.Join(root, "agent.sock")
	agent := exec.Command("ssh-agent", "-D", "-a", socket)
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Process.Kill(); _ = agent.Wait() })
	key := filepath.Join(root, "identity")
	if _, err := command(root, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key); err != nil {
		t.Fatal(err)
	}
	// ssh-add waits for this fixture's own agent to become ready.
	waitForFile(t, socket)
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in ls-remote|fetch) ssh-add -l >/dev/null 2>&1 || { printf 'Permission denied (publickey)' >&2; exit 128; };; esac\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	contextPath := testHostContext(t, executionContext{Version: 1, Environment: map[string]string{"PATH": bin + ":" + os.Getenv("PATH"), "SSH_AUTH_SOCK": socket}})
	assertAuth := func() {
		t.Helper()
		err := a.preflight()
		var failure *executionFailure
		if !errors.As(err, &failure) || failure.Blocker.Class != "authentication" || failure.Blocker.Attempts != 1 {
			t.Fatalf("agent failure misclassified: %v", err)
		}
	}
	assertAuth() // An empty agent is reachable but cannot authenticate.
	if _, err := command(root, "ssh-add", key); err != nil {
		t.Fatal(err)
	}
	if err := a.preflight(); err != nil {
		t.Fatal(err)
	}
	if _, err := command(root, "ssh-add", "-D"); err != nil {
		t.Fatal(err)
	}
	assertAuth() // Covers identities removed or expired while the socket remains available.
	if _, err := command(root, "ssh-add", "-t", "1", key); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := command(root, "ssh-add", "-l"); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	assertAuth() // The agent itself expires this identity; its socket remains live.
	if err := writeJSON(contextPath, executionContext{Version: 1, Environment: map[string]string{"PATH": bin + ":" + os.Getenv("PATH"), "SSH_AUTH_SOCK": filepath.Join(root, "missing.sock")}}); err != nil {
		t.Fatal(err)
	}
	assertAuth()
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fixture did not become ready")
}
