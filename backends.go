package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const backendAuto = "auto"

type terminalRef struct {
	Backend string           `json:"backend"`
	Session string           `json:"session"`
	Pane    string           `json:"pane,omitempty"`
	Socket  string           `json:"socket,omitempty"`
	Process *processIdentity `json:"process,omitempty"`
}

func validBackend(name string) bool {
	return name == backendAuto || name == "herdr" || name == "zellij" || name == "tmux"
}

func (t *terminalRef) validate() error {
	if t == nil || !validBackend(t.Backend) || t.Backend == backendAuto || t.Session == "" || len(t.Session) > 128 || strings.ContainsAny(t.Session, "/\x00\r\n") {
		return errors.New("invalid terminal ownership metadata")
	}
	return nil
}

type backendError struct{ message string }

func (e *backendError) Error() string { return e.message }

// Selection only probes capabilities. Never fall back after creating a session or sending a command.
func selectBackend(requested string) (string, error) {
	names := []string{requested}
	if requested == backendAuto {
		names = []string{"herdr", "zellij", "tmux"}
	}
	var failures []string
	for _, name := range names {
		if err := probeBackend(name); err != nil {
			failures = append(failures, name+": "+err.Error())
			continue
		}
		return name, nil
	}
	return "", &backendError{message: "No usable terminal backend: " + strings.Join(failures, "; ")}
}

func probeBackend(name string) error {
	env, err := executionEnvironment()
	if err != nil {
		return err
	}
	if _, err := resolveExecutable(name, env); err != nil {
		return err
	}
	if name == "tmux" {
		return nil
	}
	args := []string{"api", "schema"}
	if name == "zellij" {
		args = []string{"attach", "--help"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := executionCommand(ctx, "", name, args...)
	if err != nil {
		return err
	}
	value, err := commandOutput(c)
	if err != nil {
		return err
	}
	if name == "herdr" && !strings.Contains(value, "protocol:") {
		return errors.New("the installed Herdr must expose its socket API schema")
	}
	if name == "zellij" && (!strings.Contains(value, "--create-background") || !strings.Contains(value, "INITIAL_COMMAND")) {
		return errors.New("the installed Zellij must support a background session with an initial command")
	}
	return nil
}

// Native session managers must not inherit the launcher's outer pane or socket identity.
func terminalEnvironment() []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "HERDR_") && key != "HERDR_CONFIG_PATH" || strings.HasPrefix(key, "ZELLIJ") || key == "TMUX" || key == "TMUX_PANE" {
			continue
		}
		env = append(env, item)
	}
	return env
}

func (t *terminalRef) command(dir string, args ...string) (string, error) {
	if err := t.validate(); err != nil {
		return "", err
	}
	if t.Backend == "herdr" {
		args = append([]string{"--session", t.Session}, args...)
	}
	c, err := executionCommand(context.Background(), dir, t.Backend, args...)
	if err != nil {
		return "", err
	}
	return commandOutput(c)
}

func (t *terminalRef) alive(dir string) bool {
	if t.validate() != nil {
		return true
	}
	if t.Process != nil {
		alive, err := processState(*t.Process)
		return alive || err != nil // Unreadable ownership must never authorize a duplicate launch.
	}
	switch t.Backend {
	case "herdr":
		status, err := t.herdrStatus(dir)
		// A prepared server without a pinned pane is an uncertain launch, not permission to create another workspace.
		return err != nil || status.Server.Running
	case "tmux":
		value, err := t.command(dir, "-L", "slice-loop", "display-message", "-p", "-t", "="+t.Session+":", "#{pane_dead}")
		return err == nil && value == "0"
	case "zellij":
		value, err := t.command(dir, "--session", t.Session, "action", "list-panes", "--json", "--state")
		if err != nil {
			return false
		}
		var panes []struct {
			Plugin bool `json:"is_plugin"`
			Exited bool `json:"exited"`
		}
		if json.Unmarshal([]byte(value), &panes) != nil {
			return true // Preserve an uncertain launch for inspection.
		}
		for _, pane := range panes {
			if !pane.Plugin && !pane.Exited {
				return true
			}
		}
	}
	return false
}

type herdrStatus struct {
	Server struct {
		Running    bool   `json:"running"`
		Compatible bool   `json:"compatible"`
		Socket     string `json:"socket"`
	} `json:"server"`
}

func (t *terminalRef) herdrStatus(dir string) (herdrStatus, error) {
	var status herdrStatus
	value, err := t.command(dir, "status", "--json")
	if err == nil {
		err = json.Unmarshal([]byte(value), &status)
	}
	return status, err
}

// One named server per run bounds persistent Herdr resources across repeated resume requests.
func (t *terminalRef) prepareHerdr(a *app) error {
	status, err := t.herdrStatus(a.config.Repo)
	if err != nil {
		return err
	}
	if !status.Server.Running {
		c, err := executionCommand(context.Background(), a.config.Repo, "herdr", "--session", t.Session, "server")
		if err != nil {
			return err
		}
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			return err
		}
		exited := make(chan error, 1)
		go func() { exited <- c.Wait() }()
		deadline := time.Now().Add(5 * time.Second)
		for !status.Server.Running {
			select {
			case err := <-exited:
				return fmt.Errorf("Herdr server exited before becoming ready: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				return errors.New("Herdr server startup is unconfirmed; inspect the named session before retrying")
			}
			time.Sleep(50 * time.Millisecond)
			status, err = t.herdrStatus(a.config.Repo)
			if err != nil {
				return err
			}
		}
	}
	if !status.Server.Compatible || status.Server.Socket == "" {
		return errors.New("the named Herdr server is not compatible with the installed CLI")
	}
	t.Socket = status.Server.Socket
	args := []string{"workspace", "create", "--cwd", a.config.Repo, "--label", t.Session, "--no-focus"}
	env, err := launchEnvironment()
	if err != nil {
		return err
	}
	for _, item := range env {
		args = append(args, "--env", item)
	}
	value, err := t.command(a.config.Repo, args...)
	if err != nil {
		return err
	}
	var created struct {
		Result struct {
			Pane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(value), &created); err != nil {
		return err
	}
	t.Pane = created.Result.Pane.ID
	if t.Pane == "" {
		return errors.New("Herdr did not identify the new pane")
	}
	value, err = t.command(a.config.Repo, "pane", "process-info", "--pane", t.Pane)
	if err != nil {
		return err
	}
	var info struct {
		Result struct {
			Process struct {
				PID        int `json:"shell_pid"`
				Foreground []struct {
					PID  int    `json:"pid"`
					Name string `json:"name"`
				} `json:"foreground_processes"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(value), &info); err != nil {
		return err
	}
	owner, err := identifyProcess(info.Result.Process.PID)
	if err != nil {
		return err
	}
	t.Process = &owner
	foreground := info.Result.Process.Foreground
	if len(foreground) != 1 || foreground[0].PID != info.Result.Process.PID {
		return errors.New("the new Herdr pane is not an idle shell; inspect it before retrying")
	}
	switch filepath.Base(foreground[0].Name) {
	case "sh", "bash", "dash", "zsh", "ksh", "fish":
	default:
		return errors.New("the new Herdr pane requires a shell supporting exec and single-quoted arguments")
	}
	return nil
}

func (t *terminalRef) start(a *app, argv []string) error {
	env, err := launchEnvironment()
	if err != nil {
		return err
	}
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		quoted = append(quoted, shellQuote(arg))
	}
	cmd := "exec " + strings.Join(quoted, " ")
	switch t.Backend {
	case "herdr":
		_, err = t.command(a.config.Repo, "pane", "run", t.Pane, cmd)
	case "zellij":
		processEnv, envErr := executionEnvironment()
		if envErr != nil {
			return envErr
		}
		envExecutable, envErr := resolveExecutable("env", processEnv)
		if envErr != nil {
			return envErr
		}
		argv = append(append([]string{envExecutable}, env...), argv...)
		value, listErr := t.command(a.config.Repo, "list-sessions", "--short", "--no-formatting")
		if listErr != nil {
			var exit *exec.ExitError
			if !errors.As(listErr, &exit) || exit.ExitCode() != 1 {
				return listErr
			}
		}
		exists := false
		for _, name := range strings.Split(value, "\n") {
			exists = exists || name == t.Session
		}
		args := []string{"attach", "--create-background", "--close-on-exit", t.Session, "--"}
		if exists {
			args = []string{"--session", t.Session, "action", "new-pane", "--close-on-exit", "--cwd", a.config.Repo, "--"}
		}
		args = append(args, argv...)
		_, err = t.command(a.config.Repo, args...)
	case "tmux":
		args := []string{"-L", "slice-loop", "new-session", "-d", "-s", t.Session, "-c", a.config.Repo}
		for _, item := range env {
			args = append(args, "-e", item)
		}
		_, err = t.command(a.config.Repo, append(args, cmd)...)
	default:
		err = errors.New("unknown terminal backend")
	}
	return err
}
