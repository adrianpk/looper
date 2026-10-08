package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type processIdentity struct {
	PID   int    `json:"pid"`
	Start string `json:"start_ticks"`
}

type runRuntime struct {
	Controller  processIdentity `json:"controller"`
	Worker      processIdentity `json:"worker"`
	TmuxSession string          `json:"tmux_session,omitempty"`
	Terminal    *terminalRef    `json:"terminal,omitempty"`
}

func identifyProcess(pid int) (processIdentity, error) {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	// The command name may contain spaces and parentheses; fields begin after its final ')'.
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return processIdentity{}, errors.New("invalid process identity")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) < 20 || fields[0] == "Z" {
		return processIdentity{}, os.ErrNotExist
	}
	return processIdentity{PID: pid, Start: fields[19]}, nil
}

func processAlive(p processIdentity) bool {
	alive, _ := processState(p)
	return alive
}

func processState(p processIdentity) (bool, error) {
	if p.PID == 0 && p.Start == "" {
		return false, nil
	}
	if p.PID < 1 || p.Start == "" {
		return false, errors.New("invalid process ownership metadata")
	}
	current, err := identifyProcess(p.PID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && current == p, err
}

func lockBusy(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, nil
	}
	return false, err
}

type runLease struct{ files []*os.File }

func (l *runLease) close() {
	for _, f := range l.files {
		_ = f.Close()
	}
}

func (l *runLease) acquire(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	l.files = append(l.files, f)
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func acquireRun(a *app) (*runLease, error) {
	l := &runLease{}
	if err := l.acquire(filepath.Join(a.dir, "run.lock")); err != nil {
		l.close()
		return nil, fmt.Errorf("another controller owns this run or ownership cannot be established: %w", err)
	}
	path, err := repositoryLock(a)
	if err == nil {
		err = l.acquire(path)
	}
	if err != nil {
		l.close()
		return nil, fmt.Errorf("repository ownership could not be acquired: %w", err)
	}
	return l, nil
}

func repositoryLock(a *app) (string, error) {
	common, err := git(a.config.Repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(a.config.Repo, common)
	}
	// Share the lock with older Slice Loop controllers to prevent cross-version duplicate ownership.
	return filepath.Join(common, "slice-loop.lock"), nil
}

// Check both persisted ownership and old workers that predate runtime metadata.
func workerIdle(a *app) error {
	busy, err := lockBusy(filepath.Join(a.dir, "validate.lock"))
	if err != nil {
		return err
	}
	if busy {
		return errors.New("validation still owns this execution; wait for it to finish")
	}
	var validation processIdentity
	if err := readJSON(filepath.Join(a.dir, "validation.json"), &validation); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if alive, err := processState(validation); err != nil {
		return err
	} else if alive {
		return fmt.Errorf("validation process %d is still active; do not repeat its checks", validation.PID)
	}
	var runtime runRuntime
	if err := readJSON(filepath.Join(a.dir, "runtime.json"), &runtime); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if alive, err := processState(runtime.Worker); err != nil {
		return err
	} else if alive {
		return fmt.Errorf("worker process %d is still active; do not launch another owner", runtime.Worker.PID)
	}
	processes, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	if len(processes) > 65536 {
		return errors.New("process inventory exceeds the safety limit")
	}
	for _, p := range processes {
		pid, err := strconv.Atoi(p.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", p.Name(), "cmdline"))
		if err != nil || len(b) > 65536 {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) < 2 || filepath.Base(args[0]) != "codex" || args[1] != "exec" {
			continue
		}
		for _, arg := range args[2:] {
			if arg == filepath.Join(a.dir, "worker-schema.json") || (a.state.Session != "" && arg == a.state.Session) {
				if _, err := identifyProcess(pid); err == nil {
					return fmt.Errorf("worker process %d still owns this execution", pid)
				}
			}
		}
	}
	if a.state.Session != "" {
		return daemonWorkerIdle(a.state.Session)
	}
	return nil
}

type controllerOptions struct {
	Activity bool
	Color    colorMode
	Output   outputFormat
	Terminal *terminalRef
	LaunchID string
}

func runController(a *app, options controllerOptions) (result error) {
	dir := a.dir
	var owner *processIdentity
	ready := func(err error) {
		if options.LaunchID != "" {
			r := launchReady{ID: options.LaunchID, Status: "started", Controller: owner}
			if err != nil {
				r.Status, r.Error = "blocked", err.Error()
			}
			if writeErr := writeJSON(filepath.Join(dir, "launch-ready.json"), r); writeErr != nil {
				result = errors.Join(result, writeErr)
			}
		}
	}
	lease, err := acquireRun(a)
	if err != nil {
		ready(err)
		return errors.Join(result, err)
	}
	defer lease.close()
	// Reload under the lock: a concurrently finishing owner may have advanced the checkpoint.
	a, err = loadApp(a.path)
	var entry runEntry
	if err == nil {
		err = workerIdle(a)
	}
	if err == nil {
		entry, err = registerRun(a)
	}
	if err != nil {
		ready(err)
		return errors.Join(result, err)
	}
	identity, err := identifyProcess(os.Getpid())
	if err != nil {
		ready(err)
		return errors.Join(result, err)
	}
	owner = &identity
	a.runtime = &runRuntime{Controller: identity, Terminal: options.Terminal}
	if options.Terminal != nil && options.Terminal.Backend == "tmux" {
		a.runtime.TmuxSession = options.Terminal.Session
	}
	a.output = newStreamOutput(os.Stdout, options.Color, options.Output, entry.ID)
	// A closed log consumer must not terminate a controller while its worker or gate is active.
	pipeSignals := make(chan os.Signal, 1)
	signal.Notify(pipeSignals, syscall.SIGPIPE)
	defer signal.Stop(pipeSignals)
	if err := a.saveRuntime(); err != nil {
		ready(err)
		return errors.Join(result, err)
	}
	monitor, stop, err := startActivity(a.path, options.Activity, a.output)
	if err != nil {
		ready(err)
		return errors.Join(result, err)
	}
	a.activity = monitor
	defer stop()
	ready(nil)
	if result != nil {
		return result
	}
	a.output.emit(time.Now(), "controller.started", "", "Controller started", activityRunning)
	if err := a.run(); err != nil {
		a.state.Status, a.state.Feedback = "blocked", err.Error()
		a.output.emit(time.Now(), "controller.error", "", err.Error(), activityFailure)
		return errors.Join(err, a.save(), a.output.error())
	}
	a.output.emit(time.Now(), "controller.completed", "", "Controller completed", activitySuccess)
	return a.output.error()
}

func (a *app) saveRuntime() error {
	if a.runtime == nil {
		return nil
	}
	return writeJSON(filepath.Join(a.dir, "runtime.json"), a.runtime)
}
