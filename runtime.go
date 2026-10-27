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
	Phase       string          `json:"phase,omitempty"`
	WorkerHome  string          `json:"worker_home,omitempty"`
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
	return l.acquireMode(path, syscall.LOCK_EX)
}

func (l *runLease) acquireMode(path string, mode int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	l.files = append(l.files, f)
	return syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
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

// An open lease descriptor proves this controller still holds the repository lock.
func (a *app) requireRepositoryOwnership() error {
	path, err := repositoryLock(a)
	if err == nil && a.lease != nil {
		var target os.FileInfo
		target, err = os.Stat(path)
		if err == nil {
			for _, file := range a.lease.files {
				if held, err := file.Stat(); err == nil && os.SameFile(target, held) {
					return nil
				}
			}
		}
	}
	return contextFailure("controller", "ownership", "Continuation requires the retained repository execution lease", 1)
}

// Check both persisted ownership and old workers that predate runtime metadata.
func workerIdle(a *app) error {
	_, active, err := a.validationState()
	if err != nil {
		return err
	}
	if active {
		return errors.New("validation still owns this execution; wait for it to finish")
	}
	return workerProcessIdle(a)
}

func workerProcessIdle(a *app) error {
	var runtime runRuntime
	if err := readJSON(filepath.Join(a.dir, "runtime.json"), &runtime); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	home, err := workerHome()
	if err != nil {
		return err
	}
	if runtime.WorkerHome != "" && runtime.WorkerHome != home {
		return contextFailure("context", "configuration", "Worker home changed; restore the original conversation and credential store before resuming", 1)
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

func requireIdleWorker(a *app) error {
	err := workerIdle(a)
	var failure *executionFailure
	if err != nil && !errors.As(err, &failure) {
		return contextFailure("worker", "ownership", activityText(err.Error()), 1)
	}
	return err
}

func requireIdleWorkerProcess(a *app) error {
	err := workerProcessIdle(a)
	var failure *executionFailure
	if err != nil && !errors.As(err, &failure) {
		return contextFailure("worker", "ownership", activityText(err.Error()), 1)
	}
	return err
}

type controllerOptions struct {
	Activity   bool
	Color      colorMode
	Output     outputFormat
	Terminal   *terminalRef
	LaunchID   string
	RetryClock workerRetryClock
}

func runController(a *app, options controllerOptions) (result error) {
	entry, err := entryFor(a)
	if err != nil {
		return err
	}
	output := newStreamOutput(os.Stdout, options.Color, options.Output, entry.ID)
	a.output = output
	// The final outcome belongs to the whole loop, after set monitors and leases are released.
	defer func() {
		if a != nil && a.output != nil {
			output = a.output
		}
		kind, message, style := "loop.completed", "Loop completed successfully.", activitySuccess
		if result != nil {
			reason := activityText(result.Error())
			if reason == "" {
				reason = "Execution stopped; inspect the checkpoint."
			}
			kind, message, style = "loop.failed", "Loop failed: "+reason, activityFailure
		}
		if options.Output == outputHuman {
			message = "\n" + message + "\n"
		}
		output.emit(time.Now(), kind, "", message, style)
		result = errors.Join(result, output.error())
	}()
	dir := a.dir
	var owner *processIdentity
	acknowledged := false
	ready := func(err error) error {
		if options.LaunchID != "" && !acknowledged {
			r := launchReady{ID: options.LaunchID, Status: "started", Controller: owner}
			if err != nil {
				r.Status, r.Error = "blocked", err.Error()
				var failure *executionFailure
				if errors.As(err, &failure) {
					r.Blocker = &failure.Blocker
				}
			}
			if writeErr := writeJSON(filepath.Join(dir, "launch-ready.json"), r); writeErr != nil {
				result = errors.Join(result, writeErr)
			}
		}
		acknowledged = true
		return result
	}
	var parent sequenceOwner
	if err := readJSON(filepath.Join(a.dir, "sequence-owner.json"), &parent); err == nil {
		err := contextFailure("sequence", "ownership", "Resume the sequence entry instead of its successor", 1)
		ready(err)
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		ready(err)
		return err
	}
	lease, err := acquireRun(a)
	if err != nil {
		err = contextFailure("controller", "ownership", "Repository or run ownership cannot be acquired", 1)
		ready(err)
		return errors.Join(result, err)
	}
	defer lease.close()
	// Reload under the lock: a concurrently finishing owner may have advanced the checkpoint.
	a, err = loadApp(a.path)
	if err == nil {
		a.output = output
		a.lease = lease
		a.retryClock = options.RetryClock
		err = requireIdleWorkerProcess(a)
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
	// A closed log consumer must not terminate a controller while its worker or gate is active.
	pipeSignals := make(chan os.Signal, 1)
	signal.Notify(pipeSignals, syscall.SIGPIPE)
	defer signal.Stop(pipeSignals)
	s, err := loadSequence(a)
	if err == nil && s != nil {
		err = claimSequence(a, s)
	}
	if err == nil {
		err = runOwnedSequence(a, s, options, identity, ready)
	}
	if err != nil {
		ready(err)
		if s != nil {
			s.Phase = "blocked"
			var failure *executionFailure
			if errors.As(err, &failure) {
				s.Blocker = &failure.Blocker
			} else {
				s.Blocker = &contextFailure("controller", "execution", "Execution stopped; inspect the current set checkpoint before resuming", 1).Blocker
			}
			err = errors.Join(err, s.save(a))
		}
	}
	return errors.Join(result, err)
}

func runOwnedSet(root, a *app, options controllerOptions, identity processIdentity, ready func(error) error) (result error) {
	a.lease, a.retryClock = root.lease, options.RetryClock
	if a.path != root.path {
		l := &runLease{}
		defer l.close()
		if err := l.acquire(filepath.Join(a.dir, "run.lock")); err != nil {
			return contextFailure("controller", "ownership", "Successor controller ownership cannot be acquired", 1)
		}
	}
	if err := a.requireAvailableWorker(); err != nil {
		return err
	}
	entry, err := registerRun(a)
	if err != nil {
		return err
	}
	home, err := workerHome()
	if err != nil {
		return err
	}
	a.runtime = &runRuntime{Phase: "preflight", WorkerHome: home, Controller: identity, Terminal: options.Terminal}
	if options.Terminal != nil && options.Terminal.Backend == "tmux" {
		a.runtime.TmuxSession = options.Terminal.Session
	}
	a.output = newStreamOutput(os.Stdout, options.Color, options.Output, entry.ID)
	root.output = a.output
	if err := a.saveRuntime(); err != nil {
		return err
	}
	if a.path != root.path {
		if err := writeJSON(filepath.Join(root.dir, "runtime.json"), a.runtime); err != nil {
			return err
		}
	}
	defer func() {
		if result != nil && a.state.Status != "complete" {
			result = a.block(result)
		}
	}()
	if err := a.preflight(); err != nil {
		return err
	}
	if err := a.fetch(); err != nil {
		return err
	}
	if err := a.verifyIdentity(); err != nil {
		return err
	}
	a.runtime.Phase = "running"
	if err := a.saveRuntime(); err != nil {
		return err
	}
	if a.path != root.path {
		if err := writeJSON(filepath.Join(root.dir, "runtime.json"), a.runtime); err != nil {
			return err
		}
	}
	a.state.Blocker = nil
	monitor, stop, err := startActivity(a.path, options.Activity, a.output)
	if err != nil {
		return err
	}
	a.activity = monitor
	defer stop()
	if err := ready(nil); err != nil {
		return err
	}
	a.output.emit(time.Now(), "controller.started", "", "Controller started", activityRunning)
	advance := func() error {
		if err := a.waitValidation(); err != nil {
			return err
		}
		if r := a.state.ProviderRetry; r == nil || r.Phase != "diagnose" {
			if err := a.resumeProviderRetry(); err != nil {
				return err
			}
		}
		return a.run()
	}
	if err := a.supervise(advance); err != nil {
		a.output.emit(time.Now(), "controller.error", "", err.Error(), activityFailure)
		return err
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
