package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
)

type agentResponse struct {
	SchemaVersion int         `json:"schema_version"`
	Status        string      `json:"status"`
	Run           *runInfo    `json:"run,omitempty"`
	Runs          []runInfo   `json:"runs,omitempty"`
	Error         *runBlocker `json:"error,omitempty"`
}

type agentError struct {
	Response agentResponse
	JSON     bool
	Exit     int
}

func (e *agentError) Error() string { return e.Response.Error.Message }

func reportError(err error) int {
	var agent *agentError
	if errors.As(err, &agent) {
		if agent.JSON {
			if writeErr := printJSON(agent.Response); writeErr != nil {
				fmt.Fprintln(os.Stderr, "looper:", writeErr)
				return 1
			}
		} else {
			fmt.Fprintln(os.Stderr, "looper:", agent.Error())
			for _, r := range agent.Response.Runs {
				fmt.Fprintf(os.Stderr, "  %s / %s  %s  (--set %s; run_id %s)\n", r.Project, r.Set, r.Status, shellQuote(r.Set), r.ID)
			}
		}
		return agent.Exit
	}
	fmt.Fprintln(os.Stderr, "looper:", err)
	return 1
}

func agentInvocation(args []string) bool {
	switch args[0] {
	case "list", "register", "resume", "watch", "_resume-detached":
		return true
	case "run":
		for _, arg := range args[1:] {
			if arg == "--config" || strings.HasPrefix(arg, "--config=") {
				return false
			}
		}
		return true
	case "status":
		configChosen := false
		for _, arg := range args[1:] {
			configChosen = configChosen || arg == "--config" || strings.HasPrefix(arg, "--config=")
			for _, prefix := range []string{"--run", "--project", "--set", "--json", "--all"} {
				if arg == prefix || strings.HasPrefix(arg, prefix+"=") {
					return true
				}
			}
		}
		return !configChosen
	}
	return false
}

func executeAgent(args []string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	config := fs.String("config", "", "exact configuration path")
	id := fs.String("run", "", "stable run identity")
	project := fs.String("project", "", "project name or absolute repository path")
	set := fs.String("set", "", "exact configuration name")
	all := fs.Bool("all", false, "list runs across all projects")
	machine := fs.Bool("json", false, "emit one structured JSON result")
	detach := fs.Bool("detach", false, "run persistently in the selected terminal backend")
	activity := fs.Bool("activity", false, "show activity in the controller view")
	backend := fs.String("backend", backendAuto, "terminal backend: auto, herdr, zellij, tmux")
	color := fs.String("color", string(defaultColor), "terminal colors: auto, always, never")
	output := fs.String("output", string(defaultOutput), "stream output: jsonl, human")
	session := fs.String("session", "", "internal terminal session")
	launchID := fs.String("launch", "", "internal launch identity")
	var successors configPaths
	fs.Var(&successors, "then", "explicitly approved successor config; repeat in order")
	failure := func(code, message string, exit int, candidates []runInfo) error {
		// Detect --json even when parsing stopped before reaching it.
		jsonOutput := *machine
		for _, arg := range args[1:] {
			jsonOutput = jsonOutput || arg == "--json" || arg == "--json=true"
		}
		return &agentError{JSON: jsonOutput, Exit: exit, Response: agentResponse{SchemaVersion: 1, Status: code, Runs: candidates, Error: &runBlocker{Code: code, Message: message}}}
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return printHelp()
		}
		return failure("invalid_arguments", err.Error(), 2, nil)
	}
	if !validBackend(*backend) || !validColor(colorMode(*color)) {
		return failure("invalid_arguments", "--backend must be auto, herdr, zellij, or tmux; --color must be auto, always, or never", 2, nil)
	}
	outputChosen := false
	fs.Visit(func(f *flag.Flag) { outputChosen = outputChosen || f.Name == "output" })
	if !validOutput(outputFormat(*output)) || outputChosen && args[0] != "resume" && args[0] != "_resume-detached" {
		return failure("invalid_arguments", "--output jsonl|human belongs to run or resume", 2, nil)
	}
	backendChosen := false
	fs.Visit(func(f *flag.Flag) { backendChosen = backendChosen || f.Name == "backend" })
	if backendChosen && (args[0] != "resume" || !*detach) {
		return failure("invalid_arguments", "--backend belongs to resume --detach", 2, nil)
	}
	if fs.NArg() != 0 || (*detach && args[0] != "resume") || (*activity && args[0] != "resume" && args[0] != "_resume-detached") {
		return failure("invalid_arguments", "Unexpected arguments or flags for this command", 2, nil)
	}
	if len(successors) != 0 && args[0] != "resume" {
		return failure("invalid_arguments", "--then belongs to resume", 2, nil)
	}
	if args[0] == "_resume-detached" {
		if *config == "" || *session == "" || *launchID == "" {
			return errors.New("detached controller requires its launch identity")
		}
		a, err := loadApp(*config)
		if err != nil {
			return err
		}
		var launch launchIntent
		if err := readJSON(filepath.Join(a.dir, "launch.json"), &launch); err != nil || launch.ID != *launchID || launch.terminal() == nil || launch.terminal().Session != *session {
			return errors.New("detached launch identity does not match")
		}
		if err := launch.terminal().validate(); err != nil {
			return err
		}
		return runController(a, controllerOptions{Activity: *activity, Color: colorMode(*color), Output: outputFormat(*output), Terminal: launch.terminal(), LaunchID: *launchID})
	}
	if *session != "" || *launchID != "" {
		return failure("invalid_arguments", "Internal launch flags are not accepted", 2, nil)
	}
	if args[0] == "resume" && *machine && !*detach {
		return failure("invalid_arguments", "resume --json requires --detach so worker output cannot corrupt the response", 2, nil)
	}
	if *all && (args[0] != "list" || *config != "" || *id != "" || *project != "" || *set != "") {
		return failure("invalid_arguments", "--all belongs to list and cannot be combined with selectors", 2, nil)
	}
	if args[0] == "watch" && *machine {
		return failure("invalid_arguments", "watch opens the native terminal view; use status --json for view metadata", 2, nil)
	}
	identity := ""
	if args[0] != "register" && *config == "" && *id == "" && *project == "" && !*all {
		var err error
		identity, err = currentProject()
		if err != nil {
			return failure("project_required", "Cannot determine the current project; run inside its Git worktree or select --project, --run, or --config explicitly. Use list --all for global discovery", 2, nil)
		}
	}
	var entries []runEntry
	if *config != "" {
		if *id != "" || *project != "" || *set != "" || args[0] == "list" {
			return failure("invalid_arguments", "--config cannot be combined with other selectors or list", 2, nil)
		}
		a, err := loadApp(*config)
		if err != nil {
			return failure("invalid_checkpoint", err.Error(), 4, nil)
		}
		e, err := entryFor(a)
		if err != nil {
			return failure("invalid_checkpoint", err.Error(), 4, nil)
		}
		entries = []runEntry{e}
	} else {
		if args[0] == "register" || (*id != "" && (*project != "" || *set != "")) {
			return failure("invalid_arguments", "--run cannot be combined with project/set selectors; register requires --config", 2, nil)
		}
		var err error
		entries, err = knownRuns()
		if err != nil {
			return failure("registry_error", err.Error(), 4, nil)
		}
	}
	var runs []runInfo
	for _, e := range entries {
		if (*id != "" && e.ID != *id) || (*set != "" && e.Set != *set) {
			continue
		}
		if identity != "" && !belongsToProject(e, identity) {
			continue
		}
		r := inspectRun(e)
		if *project != "" && e.Project != *project && r.Repo != *project {
			continue
		}
		runs = append(runs, r)
	}
	if args[0] == "list" {
		if runs == nil {
			runs = []runInfo{}
		}
		// The list always includes an array, including when no runs are registered.
		if *machine {
			return printJSON(struct {
				SchemaVersion int       `json:"schema_version"`
				Status        string    `json:"status"`
				Runs          []runInfo `json:"runs"`
			}{1, "ok", runs})
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		out := newOutput(w, colorNever)
		out.color = colorEnabled(os.Stdout, colorMode(*color))
		fmt.Fprintln(w, "PROJECT\tSET\tINTEGRATED\tSTATUS\tRUN ID")
		for _, r := range runs {
			fmt.Fprintf(w, "%s\t%s\t%d/%d\t", r.Project, r.Set, r.CompletedSlices, r.TotalSlices)
			out.printf(statusStyle(r.Status), "%s", r.Status)
			fmt.Fprintf(w, "\t%s\n", r.ID)
		}
		return w.Flush()
	}
	if len(runs) == 0 {
		return failure("not_found", "No matching run; use list --json or register --config FILE --json", 2, nil)
	}
	if args[0] == "watch" {
		runs = activeRuns(runs)
		if len(runs) == 0 {
			return failure("not_running", "No active loop matches in the selected project; use status to inspect its checkpoint", 4, nil)
		}
	} else if *id == "" && *config == "" && *set == "" {
		runs = currentRuns(runs)
	}
	if len(runs) != 1 {
		return failure("selection_required", "Several sets match in this project; select --set NAME, or --run ID when set names repeat", 3, runs)
	}
	r := runs[0]
	if args[0] == "watch" {
		if err := watchRun(r); err != nil {
			return failure("view_unavailable", err.Error(), 4, nil)
		}
		return nil
	}
	out := newOutput(os.Stdout, colorMode(*color))
	respond := func(status string) error {
		if *machine {
			return printJSON(agentResponse{SchemaVersion: 1, Status: status, Run: &r})
		}
		out.printf(statusStyle(r.Status), "%s: %s / %s · %s · %d/%d integrated\n", status, r.Project, r.Set, r.ID, r.CompletedSlices, r.TotalSlices)
		if r.Blocker != nil {
			out.printf(activityFailure, "%s: %s\n", r.Blocker.Code, r.Blocker.Message)
		}
		if r.Sequence != nil {
			fmt.Printf("Sequence: %d/%d completed · %s · current %s · %d/%d slices integrated\n", r.Sequence.Completed, r.Sequence.Total, r.Sequence.Phase, filepath.Base(r.Sequence.Current), r.Sequence.CompletedSlices, r.Sequence.TotalSlices)
		}
		if r.ViewCommand != "" {
			fmt.Println(r.ViewCommand)
		}
		return nil
	}
	if args[0] == "status" {
		return respond("ok")
	}
	if r.Status == "invalid" {
		return &agentError{JSON: *machine, Exit: 4, Response: agentResponse{SchemaVersion: 1, Status: "blocked", Run: &r, Error: r.Blocker}}
	}
	a, err := loadApp(r.Config)
	if err != nil {
		return failure("invalid_checkpoint", err.Error(), 4, nil)
	}
	if _, err := registerRun(a); err != nil {
		return failure("registry_error", err.Error(), 4, nil)
	}
	if args[0] == "register" {
		return respond("registered")
	}
	if r.Status == "running" || r.Status == "starting" {
		if len(successors) != 0 {
			return failure("sequence_active", "An active execution cannot accept a changed continuation list", 4, nil)
		}
		return respond("already_running")
	}
	if r.Blocker != nil && !r.Blocker.Retryable {
		return &agentError{JSON: *machine, Exit: 4, Response: agentResponse{SchemaVersion: 1, Status: "blocked", Run: &r, Error: r.Blocker}}
	}
	if a.state.Pending != nil {
		p, err := a.pull(a.state.Pending.PR)
		if err != nil || !p.Merged {
			url := fmt.Sprintf("%s/%s/pulls/%d", a.config.Forge, a.config.Repository, a.state.Pending.PR)
			b := &runBlocker{Code: "merge_confirmation_required", Message: "The pending merge is unconfirmed; inspect the canonical PR before explicitly clearing the intent", NextAction: inspectAction("confirm_merge_outcome", url)}
			if err != nil {
				b.Message = "The pending merge cannot be queried; restore forge access before resuming"
				b.NextAction = inspectAction("restore_forge_access", a.config.Forge)
			}
			return &agentError{JSON: *machine, Exit: 4, Response: agentResponse{SchemaVersion: 1, Status: "blocked", Run: &r, Error: b}}
		}
	}
	if len(successors) != 0 {
		if err := prepareSequence(a, successors); err != nil {
			return failure("sequence_blocked", err.Error(), 4, nil)
		}
	}
	if !*detach {
		return runController(a, controllerOptions{Activity: *activity, Color: colorMode(*color), Output: outputFormat(*output)})
	}
	status, err := launchDetached(a, *activity, *backend, colorMode(*color), outputFormat(*output))
	r = inspectRun(r.runEntry)
	if err != nil {
		b := &runBlocker{Code: "launch_failed", Message: err.Error(), Retryable: true, NextAction: commandAction("looper", "status", "--run", r.ID, "--json")}
		var access *executionFailure
		if errors.As(err, &access) {
			b = &access.Blocker
		}
		var unavailable *backendError
		if errors.As(err, &unavailable) {
			b.Code, b.Retryable, b.NextAction = "backend_unavailable", false, inspectAction("check_backend", *backend)
			b.Component, b.Class = "launcher", "configuration"
		}
		if b.Component == "" {
			b.Component, b.Class = "launcher", "execution"
		}
		if saveErr := blockSequenceLaunch(a, b); saveErr != nil {
			return failure("sequence_checkpoint_failed", saveErr.Error(), 5, nil)
		}
		r = inspectRun(r.runEntry)
		return &agentError{JSON: *machine, Exit: 5, Response: agentResponse{SchemaVersion: 1, Status: "blocked", Run: &r, Error: b}}
	}
	return respond(status)
}

type launchIntent struct {
	ID       string       `json:"launch_id"`
	Session  string       `json:"tmux_session,omitempty"`
	Terminal *terminalRef `json:"terminal,omitempty"`
}

func (l launchIntent) terminal() *terminalRef {
	if l.Terminal != nil {
		return l.Terminal
	}
	if l.Session != "" {
		return &terminalRef{Backend: "tmux", Session: l.Session}
	}
	return nil
}

type launchReady struct {
	ID         string           `json:"launch_id"`
	Status     string           `json:"status"`
	Error      string           `json:"error,omitempty"`
	Controller *processIdentity `json:"controller,omitempty"`
	Blocker    *runBlocker      `json:"blocker,omitempty"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func launching(a *app) bool {
	var launch launchIntent
	if readJSON(filepath.Join(a.dir, "launch.json"), &launch) != nil || launch.terminal() == nil {
		return false
	}
	var ready launchReady
	if readJSON(filepath.Join(a.dir, "launch-ready.json"), &ready) == nil && ready.ID == launch.ID {
		if ready.Status != "started" {
			return false
		}
		if ready.Controller != nil {
			alive, err := processState(*ready.Controller)
			return alive || err != nil
		}
	}
	return launch.terminal().alive(a.config.Repo)
}

func launchDetached(a *app, activity bool, requested string, color colorMode, output outputFormat) (string, error) {
	l := &runLease{}
	defer l.close()
	if err := l.acquire(filepath.Join(a.dir, "launch.lock")); err != nil {
		// Another launcher owns the handshake. It is never safe to start a second one.
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "already_running", nil
		}
		return "", err
	}
	e, err := entryFor(a)
	if err != nil {
		return "", err
	}
	r := inspectRun(e)
	if r.Status == "running" || r.Status == "starting" {
		return "already_running", nil
	}
	if r.Blocker != nil && !r.Blocker.Retryable {
		return "", errors.New(r.Blocker.Message)
	}
	backend, err := selectBackend(requested)
	if err != nil {
		return "", err
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce)
	t := &terminalRef{Backend: backend, Session: "loop-" + strings.TrimPrefix(e.ID, "run-") + "-" + id}
	if backend == "herdr" || backend == "zellij" {
		t.Session = "loop-" + strings.TrimPrefix(e.ID, "run-")
	}
	launch := launchIntent{ID: id, Terminal: t}
	if backend == "tmux" {
		launch.Session = t.Session
	}
	if err := writeJSON(filepath.Join(a.dir, "launch.json"), launch); err != nil {
		return "", err
	}
	if backend == "herdr" {
		prepareErr := t.prepareHerdr(a)
		if err := writeJSON(filepath.Join(a.dir, "launch.json"), launch); err != nil {
			return "", errors.Join(prepareErr, err)
		}
		if prepareErr != nil {
			return "", prepareErr
		}
	}
	argv := []string{executable, "_resume-detached", "--config", a.path, "--session", t.Session, "--launch", id, "--color", string(color), "--output", string(output)}
	if activity {
		argv = append(argv, "--activity")
	}
	if err := t.start(a, argv); err != nil {
		return "", err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var ready launchReady
		if err := readJSON(filepath.Join(a.dir, "launch-ready.json"), &ready); err == nil && ready.ID == id {
			if ready.Status != "started" {
				if ready.Blocker != nil {
					return "", &executionFailure{Blocker: *ready.Blocker}
				}
				return "", errors.New(ready.Error)
			}
			return "started", nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !launching(a) {
		return "", errors.New("detached controller exited before acknowledging startup; inspect the checkpoint")
	}
	// Do not kill a slow controller: it may already own active work.
	return "starting", nil
}
