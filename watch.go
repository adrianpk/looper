package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func watchCommand(id string) string { return "looper --watch --run " + shellQuote(id) }

func (t *terminalRef) watchArgs() ([]string, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	switch t.Backend {
	case "herdr":
		return []string{"herdr", "session", "attach", t.Session}, nil
	case "zellij":
		return []string{"zellij", "watch", "--", t.Session}, nil
	default:
		return []string{"tmux", "-L", "slice-loop", "attach-session", "-r", "-t", "=" + t.Session}, nil
	}
}

// Herdr panes use a text reader; native clients replace only this viewing process.
func watchRun(r runInfo) error {
	if r.Terminal == nil {
		return errors.New("the active loop has no detached terminal view; its foreground output remains in the original terminal")
	}
	if r.Terminal.Backend == "herdr" && os.Getenv("HERDR_ENV") == "1" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return watchHerdr(ctx, r, os.Stdout)
	}
	args, err := r.Terminal.watchArgs()
	if err != nil {
		return err
	}
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, terminalEnvironment())
}

// Poll only the recorded pane. Reads and retained snapshots are bounded; cancellation belongs to the viewer.
func watchHerdr(ctx context.Context, r runInfo, out io.Writer) error {
	t := r.Terminal
	if err := t.validate(); err != nil {
		return err
	}
	if t.Pane == "" {
		return errors.New("the active Herdr loop has no recorded pane")
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var previous []string
	for {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		c := exec.CommandContext(readCtx, "herdr", "--session", t.Session, "pane", "read", t.Pane,
			"--source", "recent-unwrapped", "--lines", "120", "--format", "text")
		c.Dir, c.Env = r.Repo, terminalEnvironment()
		var snapshot, diagnostic capture
		snapshot.limit, diagnostic.limit = 8<<20, 4096
		c.Stdout, c.Stderr = &snapshot, &diagnostic
		err := c.Run()
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			var response struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if !diagnostic.truncated && json.Unmarshal(diagnostic.b.Bytes(), &response) == nil && response.Error.Code == "pane_not_found" {
				return watchCheckpoint(r, out)
			}
			return fmt.Errorf("herdr failed: %w: %s", err, strings.TrimSpace(diagnostic.b.String()))
		}
		if snapshot.truncated {
			return errors.New("herdr output exceeds 8 MiB")
		}
		previous, err = writeWatchSnapshot(out, previous, strings.TrimSpace(snapshot.b.String()))
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Read the authoritative checkpoint again: the controller can exit after view selection.
func watchCheckpoint(r runInfo, out io.Writer) error {
	a, err := loadApp(r.Config)
	if err != nil {
		return fmt.Errorf("Herdr pane is unavailable; checkpoint could not be read: %w", err)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "state.json")); err != nil {
		return fmt.Errorf("Herdr pane is unavailable; checkpoint could not be read: %w", err)
	}
	entry, err := entryFor(a)
	if err != nil || entry.ID != r.ID {
		return errors.New("Herdr pane is unavailable; registered checkpoint identity changed")
	}
	if _, err := fmt.Fprintf(out, "Herdr pane is unavailable.\nCheckpoint: %s · %s / %s · %s · %d/%d integrated\n", a.state.Status, entry.Project, entry.Set, entry.ID, a.state.Next, len(a.config.Slices)); err != nil {
		return err
	}
	if b := a.state.Blocker; b != nil {
		if _, err := fmt.Fprintf(out, "%s: %s\n", b.Code, b.Message); err != nil {
			return err
		}
		if b.NextAction != nil && b.NextAction.Type == "command" && len(b.NextAction.Argv) != 0 {
			if _, err := fmt.Fprintln(out, actionCommand(b.NextAction)); err != nil {
				return err
			}
		}
	} else if a.state.Status == "blocked" && a.state.Feedback != "" {
		_, err = fmt.Fprintln(out, activityText(a.state.Feedback))
	}
	return err
}

// Join overlapping scrollback snapshots without losing repeated events or an unfinished JSONL line.
func writeWatchSnapshot(out io.Writer, previous []string, snapshot string) ([]string, error) {
	if snapshot == "" {
		return nil, nil
	}
	lines := strings.Split(snapshot, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if strings.HasPrefix(last, `{"schema_version":`) && !json.Valid([]byte(last)) {
		lines = lines[:len(lines)-1]
	}
	start := 0
	for n := min(len(previous), len(lines)); n > 0; n-- {
		if slices.Equal(previous[len(previous)-n:], lines[:n]) {
			start = n
			break
		}
	}
	for _, line := range lines[start:] {
		var event logEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.SchemaVersion == 1 && event.Type != "" {
			line = event.Message
			if event.Label != "" {
				line = event.Label + " · " + line
			}
			if event.Time != "" {
				line = "[" + event.Time + "] " + line
			}
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return nil, err
		}
	}
	return lines, nil
}
