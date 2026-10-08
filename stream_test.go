package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodeLog(t *testing.T, line, runID string) logEvent {
	t.Helper()
	var event logEvent
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		t.Fatalf("invalid stream record %q: %v", line, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, event.Time); err != nil {
		t.Fatalf("invalid event time: %+v", event)
	}
	if event.SchemaVersion != 1 || event.RunID != runID || event.Type == "" || event.Level == "" || event.Message == "" || strings.Contains(line, "\x1b") {
		t.Fatalf("incomplete or colored event: %+v", event)
	}
	return event
}

// Concurrent activity and controller records keep their framing, escaping and identity even with forced colors.
func TestConcurrentStream(t *testing.T) {
	var out bytes.Buffer
	o := newStreamOutput(&out, colorAlways, outputJSONL, "run-test")
	var wg sync.WaitGroup
	for writer := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 10 {
				o.emit(time.Now(), "worker.message", "Worker", fmt.Sprintf("writer %d event %d\n\"quoted\"", writer, n), activityRunning)
				o.setColor(colorAlways)
			}
		}()
	}
	wg.Wait()
	if err := o.error(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	seen := make(map[string]bool)
	for _, line := range lines {
		event := decodeLog(t, line, "run-test")
		if event.Label != "Worker" || event.Level != "info" || seen[event.Message] {
			t.Fatalf("record was changed or duplicated: %+v", event)
		}
		seen[event.Message] = true
	}
	if len(seen) != 80 {
		t.Fatalf("lost records: got %d", len(seen))
	}
}

func waitFixtureWorker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path + ".entered"); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fixture worker did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// jq receives default JSONL before a worker finishes; successful and blocked runs keep stdout machine-readable.
func TestLiveStream(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	for _, tc := range []struct {
		name, command, mode, final string
	}{
		{"live run", "run", "paused-worker", "controller.completed"},
		{"blocked resume", "resume", "gate-failure", "controller.error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recoveryEnvironment(t)
			a, fixturePath := setupLoop(t, tc.mode)
			e, err := entryFor(a)
			if err != nil {
				t.Fatal(err)
			}
			controller := exec.Command("looper", tc.command, "--config", a.path, "--activity")
			var diagnostic, jqDiagnostic bytes.Buffer
			controller.Stderr = &diagnostic
			stream, err := controller.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			jq := exec.Command("jq", "--unbuffered", "-c", ".")
			jq.Stdin, jq.Stderr = stream, &jqDiagnostic
			parsed, err := jq.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := jq.Start(); err != nil {
				t.Fatal(err)
			}
			if err := controller.Start(); err != nil {
				_ = stream.Close()
				_ = jq.Wait()
				t.Fatal(err)
			}
			lines := make(chan string, 128)
			scanError := make(chan error, 1)
			go func() {
				scanner := bufio.NewScanner(parsed)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				scanError <- scanner.Err()
				close(lines)
			}()
			waited := false
			t.Cleanup(func() {
				_ = os.WriteFile(fixturePath+".release", []byte("continue"), 0600)
				if !waited {
					for range lines {
					}
					_ = controller.Wait()
					_ = jq.Wait()
				}
			})
			seen := make(map[string]bool)
			if tc.mode == "paused-worker" {
				waitFixtureWorker(t, fixturePath)
				deadline := time.After(3 * time.Second)
				for !seen["worker.started"] {
					select {
					case line, ok := <-lines:
						if !ok {
							t.Fatal("stream closed before the paused worker finished")
						}
						seen[decodeLog(t, line, e.ID).Type] = true
					case <-deadline:
						t.Fatal("jq did not receive live records while the worker was paused")
					}
				}
				if _, err := activityRequest(a.path, "color:always"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixturePath+".release", []byte("continue"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			for line := range lines {
				event := decodeLog(t, line, e.ID)
				seen[event.Type] = true
				for _, private := range []string{"token=private", "private output", "private reasoning", "thread.started"} {
					if strings.Contains(line, private) {
						t.Fatalf("raw worker payload reached the log: %q", line)
					}
				}
			}
			runErr, jqErr := controller.Wait(), jq.Wait()
			waited = true
			if err := <-scanError; err != nil || jqErr != nil || jqDiagnostic.Len() != 0 {
				t.Fatalf("jq stream failed: %v %v %s", err, jqErr, jqDiagnostic.String())
			}
			if (runErr != nil) != (tc.mode == "gate-failure") || (diagnostic.Len() != 0) != (tc.mode == "gate-failure") {
				t.Fatalf("unexpected controller result: %v %s", runErr, diagnostic.String())
			}
			for _, kind := range []string{"controller.started", "worker.message", "tool.started", "tool.completed", "validation.started", "validation.check", tc.final} {
				if !seen[kind] {
					t.Errorf("missing stream record %q", kind)
				}
			}
		})
	}
}

// Closing a viewer triggers an actual SIGPIPE without abandoning the owned worker or changing its delivery result.
func TestClosedStream(t *testing.T) {
	recoveryEnvironment(t)
	a, fixturePath := setupLoop(t, "paused-worker")
	controller := exec.Command("looper", "run", "--config", a.path, "--activity")
	var diagnostic bytes.Buffer
	controller.Stderr = &diagnostic
	stream, err := controller.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stream.Close()
		_ = os.WriteFile(fixturePath+".release", []byte("continue"), 0600)
		if !waited {
			_ = controller.Wait()
		}
	})
	waitFixtureWorker(t, fixturePath)
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if response, err := activityRequest(a.path, "off"); err != nil || response != "Activity: off" {
		t.Fatalf("controller did not survive the closed consumer: %q %v", response, err)
	}
	if busy, err := lockBusy(filepath.Join(a.dir, "run.lock")); err != nil || !busy {
		t.Fatalf("closed output released ownership of the active worker: %v %v", busy, err)
	}
	if err := os.WriteFile(fixturePath+".release", []byte("continue"), 0600); err != nil {
		t.Fatal(err)
	}
	runErr := controller.Wait()
	waited = true
	current, stateErr := loadApp(a.path)
	if runErr == nil || !strings.Contains(diagnostic.String(), "broken pipe") || stateErr != nil || current.state.Status != "complete" {
		t.Fatalf("closed viewer changed delivery or hid the output error: %v %v %s", runErr, stateErr, diagnostic.String())
	}
}
