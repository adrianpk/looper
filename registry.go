package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const registryLimit = 1024

type runEntry struct {
	Version int    `json:"version"`
	ID      string `json:"run_id"`
	Config  string `json:"config"`
	Project string `json:"project"`
	Set     string `json:"set"`
	problem string
}

type runBlocker struct {
	Component  string     `json:"component,omitempty"`
	Class      string     `json:"class,omitempty"`
	Attempts   int        `json:"attempts,omitempty"`
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	Retryable  bool       `json:"retryable"`
	NextAction *runAction `json:"next_action,omitempty"`
}

type runAction struct {
	Type   string   `json:"type"`
	Argv   []string `json:"argv,omitempty"`
	Target string   `json:"target,omitempty"`
}

func commandAction(args ...string) *runAction {
	return &runAction{Type: "command", Argv: args}
}

func inspectAction(kind, target string) *runAction {
	return &runAction{Type: kind, Target: target}
}

type runInfo struct {
	runEntry
	Repo             string        `json:"repo,omitempty"`
	Status           string        `json:"status"`
	CheckpointStatus string        `json:"checkpoint_status,omitempty"`
	Slice            int           `json:"slice,omitempty"`
	TotalSlices      int           `json:"total_slices,omitempty"`
	CompletedSlices  int           `json:"completed_slices"`
	WorkerTurns      int           `json:"worker_turns"`
	MaxWorkerTurns   int           `json:"max_worker_turns"`
	StalledTurns     int           `json:"stalled_turns"`
	MaxStalledTurns  int           `json:"max_stalled_turns"`
	Session          string        `json:"worker_session,omitempty"`
	TmuxSession      string        `json:"tmux_session,omitempty"`
	Terminal         *terminalRef  `json:"terminal,omitempty"`
	ViewCommand      string        `json:"view_command,omitempty"`
	Blocker          *runBlocker   `json:"blocker,omitempty"`
	Sequence         *sequenceInfo `json:"sequence,omitempty"`
}

// Registry entries are an index. Configuration and checkpoints remain authoritative.
func registryRoot() (string, error) {
	env, err := executionEnvironment()
	if err != nil {
		return "", err
	}
	base := environmentValue(env, "XDG_STATE_HOME")
	if base == "" {
		home := environmentValue(env, "HOME")
		base = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(base) {
		return "", errors.New("XDG_STATE_HOME must be absolute")
	}
	return filepath.Join(base, "looper"), nil
}

func entryFor(a *app) (runEntry, error) {
	p, err := filepath.EvalSymlinks(a.path)
	if err != nil {
		return runEntry{}, err
	}
	// Include the immutable configuration identity so replacement cannot select an old run.
	id := "run-" + digest([]byte(p + "\x00" + a.state.ConfigHash))[:24]
	return runEntry{Version: 1, ID: id, Config: p, Project: filepath.Base(a.config.Repo), Set: strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))}, nil
}

func registerRun(a *app) (runEntry, error) {
	e, err := entryFor(a)
	if err != nil {
		return e, err
	}
	root, err := registryRoot()
	if err != nil {
		return e, err
	}
	return e, writeJSON(filepath.Join(root, "runs", e.ID+".json"), e)
}

func knownRuns() ([]runEntry, error) {
	root, err := registryRoot()
	if err != nil {
		return nil, err
	}
	entries := make(map[string]runEntry)
	var files, legacy []string
	// Existing Slice Loop indices and configurations remain readable without moving active state.
	for _, dir := range []string{filepath.Join(filepath.Dir(root), "slice-loop"), root} {
		indexed, err := discoveryFiles(filepath.Join(dir, "runs"))
		if err != nil {
			return nil, err
		}
		configs, err := discoveryFiles(dir)
		if err != nil {
			return nil, err
		}
		files = append(files, indexed...)
		legacy = append(legacy, configs...)
	}
	if len(files)+len(legacy) > registryLimit {
		return nil, fmt.Errorf("run discovery exceeds %d files", registryLimit)
	}
	for _, p := range files {
		var e runEntry
		if err := readJSON(p, &e); err != nil {
			e.problem = err.Error()
		} else if e.Version != 1 || !filepath.IsAbs(e.Config) || filepath.Base(p) != e.ID+".json" {
			e.problem = "invalid registry identity"
		}
		if e.problem != "" {
			// A broken index must remain visible without preventing recovery of other projects.
			e.ID = strings.TrimSuffix(filepath.Base(p), ".json")
			e.Config, e.Project, e.Set = p, "", ""
		}
		entries[e.ID] = e
	}
	// Existing configurations in the standard state directory are discoverable without writes.
	for _, p := range legacy {
		a, err := loadApp(p)
		if err != nil {
			// Keep invalid legacy configurations visible instead of silently losing their recovery path.
			id := "invalid-" + digest([]byte(p))[:24]
			entries[id] = runEntry{Version: 1, ID: id, Config: p, Set: strings.TrimSuffix(filepath.Base(p), ".json")}
			continue
		}
		e, err := entryFor(a)
		if err != nil {
			return nil, err
		}
		entries[e.ID] = e
	}
	result := make([]runEntry, 0, len(entries))
	for _, e := range entries {
		result = append(result, e)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// Bound discovery and report unreadable directories rather than returning an empty registry.
func discoveryFiles(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(registryLimit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > registryLimit {
		return nil, fmt.Errorf("run discovery exceeds %d entries in %s", registryLimit, dir)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

func inspectRun(e runEntry) (r runInfo) {
	r = runInfo{runEntry: e, Status: "invalid"}
	if e.problem != "" {
		r.Blocker = &runBlocker{Code: "invalid_registry_entry", Message: e.problem, NextAction: inspectAction("inspect_registry", e.Config)}
		return r
	}
	a, err := loadApp(e.Config)
	if err != nil {
		r.Blocker = &runBlocker{Code: "invalid_checkpoint", Message: err.Error(), NextAction: inspectAction("inspect_checkpoint", e.Config)}
		return r
	}
	defer sequenceStatus(a, &r)
	actual, err := entryFor(a)
	if err != nil || actual.ID != e.ID {
		r.Blocker = &runBlocker{Code: "identity_changed", Message: "The registered configuration identity changed; inspect and register it explicitly", NextAction: commandAction("looper", "register", "--config", e.Config, "--json")}
		return r
	}
	r.Repo, r.CheckpointStatus = a.config.Repo, a.state.Status
	r.TotalSlices, r.Session = len(a.config.Slices), a.state.Session
	r.CompletedSlices = a.state.Next
	r.WorkerTurns, r.MaxWorkerTurns = a.state.Turns, a.config.MaxTurns
	r.StalledTurns, r.MaxStalledTurns = a.state.Stalled, a.config.MaxStalled
	if a.state.Next < r.TotalSlices {
		r.Slice = a.state.Next + 1
	}
	var runtime runRuntime
	if err := readJSON(filepath.Join(a.dir, "runtime.json"), &runtime); err == nil {
		r.TmuxSession = runtime.TmuxSession
		r.Terminal = runtime.Terminal
		if r.Terminal == nil && r.TmuxSession != "" {
			r.Terminal = &terminalRef{Backend: "tmux", Session: r.TmuxSession}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		r.Blocker = &runBlocker{Code: "invalid_runtime", Message: err.Error(), NextAction: inspectAction("inspect_runtime", filepath.Join(a.dir, "runtime.json"))}
		return r
	}
	if r.Terminal != nil {
		if err := r.Terminal.validate(); err != nil {
			r.Blocker = &runBlocker{Code: "invalid_runtime", Message: err.Error(), NextAction: inspectAction("inspect_runtime", filepath.Join(a.dir, "runtime.json"))}
			return r
		}
		r.ViewCommand = watchCommand(r.ID)
	}
	busy, err := lockBusy(filepath.Join(a.dir, "run.lock"))
	if err != nil {
		r.Blocker = &runBlocker{Code: "ownership_unknown", Message: err.Error(), NextAction: inspectAction("inspect_ownership", e.Config)}
		return r
	}
	if busy {
		r.Status = "running"
		if runtime.Phase == "preflight" {
			r.Status = "starting"
		}
		return r
	}
	if launching(a) {
		r.Status = "starting"
		var launch launchIntent
		if readJSON(filepath.Join(a.dir, "launch.json"), &launch) == nil && launch.terminal() != nil && launch.terminal().validate() == nil {
			r.Terminal = launch.terminal()
			r.ViewCommand = watchCommand(r.ID)
			r.TmuxSession = ""
			if r.Terminal.Backend == "tmux" {
				r.TmuxSession = r.Terminal.Session
			}
		}
		return r
	}
	// A successor can own the repository without changing this run's delivered result.
	// Same-run ownership and checkpoint identity were still checked above; resume reacquires its lease.
	if a.state.Status == "complete" {
		r.Status = "complete"
		return r
	}
	repoLock, err := repositoryLock(a)
	if err == nil {
		busy, err = lockBusy(repoLock)
	}
	if err != nil || busy {
		r.Status = "blocked"
		r.Blocker = &runBlocker{Code: "repository_busy", Message: "Another execution owns the repository, or repository ownership cannot be established", NextAction: inspectAction("inspect_repository_owner", a.config.Repo)}
		return r
	}
	if err := workerIdle(a); err != nil {
		r.Status = "blocked"
		target := a.state.Session
		if target == "" {
			target = e.Config
		}
		r.Blocker = &runBlocker{Code: "worker_not_idle", Message: err.Error(), NextAction: inspectAction("inspect_worker", target)}
		return r
	}
	r.Status = a.state.Status
	if r.Status == "running" {
		r.Status = "interrupted"
	}
	if a.state.Pending != nil {
		r.Blocker = &runBlocker{Code: "pending_merge", Message: "Reconcile the canonical PR before another merge submission", Retryable: true, NextAction: commandAction("looper", "resume", "--run", e.ID, "--detach", "--json")}
	} else if a.state.Turns >= a.config.MaxTurns && a.state.Status != "complete" {
		r.Status = "blocked"
		r.Blocker = &runBlocker{Code: "turn_limit", Message: "The approved worker-turn budget is exhausted; explicit reconfiguration is required", NextAction: inspectAction("review_turn_budget", e.Config)}
	} else if r.Status == "blocked" && a.state.Blocker != nil {
		r.Blocker = a.state.Blocker
		if r.Blocker.Class == "ownership" {
			copy := *r.Blocker
			copy.Retryable = true // Live ownership was checked above; resume checks it again under the lease.
			r.Blocker = &copy
		}
	} else if r.Status == "blocked" {
		r.Blocker = &runBlocker{Code: "previous_failure", Message: activityText(a.state.Feedback), Retryable: true, NextAction: inspectAction("resolve_blocker_then_resume", e.ID)}
	}
	if a.state.Pending == nil && a.state.Status != "complete" && a.state.Turns < a.config.MaxTurns && a.state.Stalled >= a.config.MaxStalled {
		current, err := a.fingerprint()
		if err != nil || current == a.state.Fingerprint {
			r.Status = "blocked"
			r.Blocker = &runBlocker{Code: "progress_required", Message: "The worker stopped after repeated turns without progress; inspect and resolve the workspace blocker before resuming", NextAction: inspectAction("resolve_workspace_blocker", a.config.Repo)}
		}
	}
	return r
}
