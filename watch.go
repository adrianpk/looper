package main

import (
	"errors"
	"os/exec"
	"syscall"
)

func watchCommand(id string) string { return "looper --watch --run " + shellQuote(id) }

func (t *terminalRef) watchArgs() ([]string, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	switch t.Backend {
	case "herdr":
		return []string{"herdr", "session", "attach", "--", t.Session}, nil
	case "zellij":
		return []string{"zellij", "watch", "--", t.Session}, nil
	default:
		return []string{"tmux", "-L", "slice-loop", "attach-session", "-r", "-t", "=" + t.Session}, nil
	}
}

// Replace only this viewing process; native attach commands never launch a controller or worker.
func watchRun(r runInfo) error {
	if r.Terminal == nil {
		return errors.New("the active loop has no detached terminal view; its foreground output remains in the original terminal")
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
