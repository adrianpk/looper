package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestActivityText prevents raw code, control characters and common credentials from becoming display text.
func TestActivityText(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"paragraph", "Preparing validation.\nChecking fixtures.\n\nMore details.", "Preparing validation. Checking fixtures."},
		{"code", "```go\nsecret code\n```\nRunning `go test ./...`.\n    hidden code", "Running [code]."},
		{"result", `{"status":"continue","message":"internal result"}`, ""},
		{"credentials", "token=private password='hidden' Authorization: Bearer credential", "token=[redacted] password=[redacted] Authorization=[redacted] [redacted]"},
		{"controls", "Checking\x00 fixtures\x1b.", "Checking fixtures."},
		{"bounded", strings.Repeat("á", 9000), strings.Repeat("á", 360) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activityText(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestActivityColors preserves explicit status labels and resets ANSI styling on each colored line.
func TestActivityColors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style activityStyle
		emit  func(*activityMonitor)
	}{
		{"message", activityMessage, func(m *activityMonitor) {
			m.event([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"Working."}}`))
		}},
		{"running", activityRunning, func(m *activityMonitor) {
			m.event([]byte(`{"type":"item.started","item":{"type":"command_execution","id":"check","command":"go test ./..."}}`))
		}},
		{"success", activitySuccess, func(m *activityMonitor) {
			m.event([]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","exit_code":0}}`))
		}},
		{"failure", activityFailure, func(m *activityMonitor) {
			m.event([]byte(`{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","exit_code":1}}`))
		}},
		{"error", activityFailure, func(m *activityMonitor) { m.event([]byte(`{"type":"turn.failed"}`)) }},
		{"warning", activityWarning, func(m *activityMonitor) { m.heartbeat(m.lastEvent.Add(6 * time.Minute)) }},
		{"heartbeat", activityQuiet, func(m *activityMonitor) { m.heartbeat(m.lastEvent.Add(15 * time.Second)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, color := range []bool{false, true} {
				var out bytes.Buffer
				mode := colorNever
				if color {
					mode = colorAlways
				}
				m := &activityMonitor{output: newOutput(&out, mode), enabled: true}
				m.begin("Test worker", "worker.started")
				out.Reset()
				tc.emit(m)
				got := out.String()
				if got == "" {
					t.Fatal("status label disappeared")
				}
				if color && (!strings.Contains(got, string(tc.style)) || !strings.HasSuffix(got, "\x1b[0m\n")) {
					t.Fatalf("incorrect color or missing reset: %q", got)
				}
				if !color && strings.Contains(got, "\x1b") {
					t.Fatal("plain output contains terminal styling")
				}
			}
		})
	}
}

// TestActivityTerminal keeps redirected output plain and honors explicit color suppression.
func TestActivityTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var out bytes.Buffer
	if activityTerminal(&out) {
		t.Fatal("buffer was treated as a terminal")
	}
	file, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if activityTerminal(file) {
		t.Fatal("log file was treated as a terminal")
	}
	tty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	if !activityTerminal(tty) {
		t.Fatal("pseudo-terminal did not enable color")
	}
	t.Setenv("NO_COLOR", "1")
	if activityTerminal(tty) {
		t.Fatal("NO_COLOR was ignored")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	if activityTerminal(tty) {
		t.Fatal("TERM=dumb was ignored")
	}
}

// TestActivityEvents checks the public event allowlist without exposing commands, payloads or reasoning.
func TestActivityEvents(t *testing.T) {
	for _, tc := range []struct {
		name, kind, itemType, want string
		fields                     map[string]any
	}{
		{"message", "item.completed", "agent_message", "Worker: Preparing validation.", map[string]any{"text": "Preparing validation."}},
		{"command", "item.started", "command_execution", "Started: go test", map[string]any{"command": "GOWORK=off go test ./... token=private", "aggregated_output": "private output"}},
		{"unknown command", "item.started", "command_execution", "Started: shell command", map[string]any{"command": "curl private-url"}},
		{"exit", "item.completed", "command_execution", "Failed: go test (exit 9)", map[string]any{"command": "go test ./...", "exit_code": 9}},
		{"tool", "item.completed", "mcp_tool_call", "Completed: tool exec_command (shell)", map[string]any{"tool": "exec_command", "server": "shell", "arguments": "private payload"}},
		{"tool failure", "item.completed", "mcp_tool_call", "Failed: tool", map[string]any{"status": "failed", "tool": "bad\nname"}},
		{"files", "item.completed", "file_change", "Completed: file changes", map[string]any{"changes": "private diff"}},
		{"search", "item.started", "web_search", "Started: web search", map[string]any{"query": "private query"}},
		{"reasoning", "item.completed", "reasoning", "", map[string]any{"text": "private reasoning"}},
		{"future item", "item.completed", "future_type", "", map[string]any{"text": "private unknown"}},
		{"error", "turn.failed", "", "Worker reported an error", map[string]any{"text": "private diagnostic"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			m := &activityMonitor{output: newOutput(&out, colorNever), enabled: true}
			m.begin("Test worker", "worker.started")
			out.Reset()
			item := map[string]any{"id": "item-1", "type": tc.itemType}
			for key, value := range tc.fields {
				item[key] = value
			}
			raw, err := json.Marshal(map[string]any{"type": tc.kind, "item": item})
			if err != nil {
				t.Fatal(err)
			}
			m.event(raw)
			got := out.String()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) || strings.Contains(got, "private") {
				t.Fatalf("unexpected display: %q", got)
			}
		})
	}
}

// TestActivityHeartbeat distinguishes quiet work from confirmed failure and ends when the phase ends.
func TestActivityHeartbeat(t *testing.T) {
	var out bytes.Buffer
	m := &activityMonitor{output: newOutput(&out, colorNever), enabled: true}
	m.begin("Test worker", "worker.started")
	m.event([]byte(`{"type":"item.started","item":{"type":"command_execution","id":"check","command":"make check"}}`))
	now := m.lastEvent
	m.heartbeat(now.Add(15 * time.Second))
	if !strings.Contains(out.String(), "tracked tools in progress: 1") {
		t.Fatal("running tool was not reported")
	}
	out.Reset()
	m.heartbeat(now.Add(6 * time.Minute))
	if !strings.Contains(out.String(), "No activity events for 6m0s") || !strings.Contains(out.String(), "No interruption") || !m.active {
		t.Fatal("silence was treated as a completed or interrupted phase")
	}
	m.event([]byte(`{"type":"item.completed","item":{"type":"command_execution","id":"check","command":"make check","exit_code":0}}`))
	if len(m.tools) != 0 {
		t.Fatal("completed tool still reported as running")
	}
	m.end()
	out.Reset()
	m.heartbeat(now.Add(time.Hour))
	m.event([]byte(`{"type":"item.started","item":{"type":"command_execution","id":"late"}}`))
	if out.Len() != 0 {
		t.Fatal("inactive phase emitted activity")
	}
}

// TestActivityControl exercises the real same-user socket and CLI without reading or mutating delivery state.
func TestActivityControl(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	checkpointPath := configPath + ".state"
	for _, path := range []string{configPath, checkpointPath} {
		if err := os.WriteFile(path, []byte("unchanged"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	m, stop, err := startActivity(configPath, false, newOutput(&out, colorAuto))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	m.begin("Test worker", "worker.started")
	m.event([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"Preparing fixtures."}}`))
	m.mu.Lock()
	if out.Len() != 0 {
		m.mu.Unlock()
		t.Fatal("quiet mode printed activity")
	}
	m.mu.Unlock()
	for _, tc := range []struct{ mode, want string }{
		{"status", "Activity: off"}, {"on", "Activity: on"}, {"toggle", "Activity: off"},
		{"toggle", "Activity: on"}, {"off", "Activity: off"},
	} {
		got, err := activityRequest(configPath, tc.mode)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q, %v", tc.mode, got, err)
		}
	}
	m.mu.Lock()
	output := out.String()
	m.mu.Unlock()
	if !strings.Contains(output, "Activity enabled; latest: Preparing fixtures.") {
		t.Fatal("enabling mid-turn lost the latest public event")
	}
	if err := execute([]string{"activity", "--config", configPath, "--on"}); err != nil {
		t.Fatal(err)
	}
	if err := execute([]string{"activity", "--config", configPath, "--color", "always"}); err != nil {
		t.Fatal(err)
	}
	if _, err := activityRequest(configPath, "status"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configPath, checkpointPath} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "unchanged" {
			t.Fatalf("display control changed state: %s", path)
		}
	}
	for _, args := range [][]string{
		{"activity", "--config", configPath, "--on", "--off"},
		{"status", "--config", configPath, "--activity"},
		{"run", "--config", configPath, "--toggle"},
	} {
		if err := execute(args); err == nil {
			t.Fatalf("invalid flags accepted: %v", args)
		}
	}
}

// TestActivityLifetime checks endpoint cleanup, exclusivity and independence from filesystem path length.
func TestActivityLifetime(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), strings.Repeat("long-segment/", 30), "not-created.json")
	if _, err := activityRequest(configPath, "on"); err == nil {
		t.Fatal("missing controller accepted a command")
	}
	_, stop, err := startActivity(configPath, false, newOutput(io.Discard, colorAuto))
	if err != nil {
		t.Fatal(err)
	}
	if _, otherStop, err := startActivity(configPath, true, newOutput(io.Discard, colorAuto)); err == nil {
		otherStop()
		stop()
		t.Fatal("two listeners own the same configuration")
	}
	if _, err := activityRequest(configPath, "toggle"); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	if _, err := activityRequest(configPath, "on"); err == nil {
		t.Fatal("stopped controller still accepts commands")
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatal("display control created configuration files")
	}
}

// TestActivityConcurrent checks event consumption and runtime toggling under the race detector.
func TestActivityConcurrent(t *testing.T) {
	m := &activityMonitor{output: newOutput(io.Discard, colorNever)}
	m.begin("Test worker", "worker.started")
	var wg sync.WaitGroup
	for _, action := range []func(){
		func() { m.event([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"Working."}}`)) },
		func() { m.control("toggle") },
		func() { m.heartbeat(time.Now()) },
	} {
		wg.Go(func() {
			for range 100 {
				action()
			}
		})
	}
	wg.Wait()
	m.end()
}
