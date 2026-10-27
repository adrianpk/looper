package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The batch owns its locks across all commands. Its identity is also mirrored
// in the legacy validation.json file while this version keeps the full record separately.
type validationRun struct {
	processIdentity
	Version       int             `json:"version"`
	ID            string          `json:"validation_id"`
	Config        string          `json:"config"`
	ConfigHash    string          `json:"config_hash"`
	Repository    string          `json:"repository"`
	Worktree      string          `json:"worktree"`
	Head          string          `json:"head"`
	Commands      []string        `json:"commands"`
	Status        string          `json:"status"`
	Started       time.Time       `json:"started_at"`
	Finished      *time.Time      `json:"finished_at,omitempty"`
	Check         int             `json:"check"`
	Completed     int             `json:"completed_checks"`
	CheckStart    time.Time       `json:"check_started_at"`
	Child         processIdentity `json:"check_process"`
	Log           string          `json:"log"`
	Failure       *runBlocker     `json:"failure,omitempty"`
	ExitCode      int             `json:"exit_code,omitempty"`
	FailureFamily string          `json:"failure_family,omitempty"`
}

type validationWait struct {
	ID       string          `json:"validation_id"`
	Owner    processIdentity `json:"owner"`
	Worktree string          `json:"worktree"`
	Head     string          `json:"head"`
	Commands []string        `json:"commands"`
}

func (a *app) validationLock() (string, error) {
	root, err := projectIdentity(a.config.Repo)
	return filepath.Join(root, "looper-validation.lock"), err
}

func validationFailure(code, class, message string) *executionFailure {
	f := contextFailure("validation", class, message, 1)
	f.Blocker.Code = "validation_" + code
	if class == "integrity" {
		f.Blocker.NextAction = inspectAction("inspect_validation_outcome", "validation-run.json")
	}
	if class == "ownership" {
		f.Blocker.NextAction = inspectAction("inspect_validation_owner", "validate.lock")
	}
	return f
}

func (a *app) validate(dir string, commands []string) (result error) {
	root, err := projectIdentity(dir)
	if err != nil {
		return err
	}
	canonical, err := projectIdentity(a.config.Repo)
	if err != nil {
		return err
	}
	if root != canonical {
		return errors.New("validation checkout belongs to another repository")
	}
	dir, err = filepath.Abs(dir)
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		return err
	}
	if len(commands) == 0 || len(commands) > 100 {
		return errors.New("expected 1 to 100 validation commands")
	}
	for _, cmd := range commands {
		if strings.TrimSpace(cmd) == "" || len(cmd) > 8192 || strings.ContainsAny(cmd, "\r\n") {
			return errors.New("invalid validation command")
		}
	}
	lease := &runLease{}
	defer lease.close()
	for _, path := range []string{filepath.Join(root, "looper-validation.lock"), filepath.Join(a.dir, "validate.lock")} {
		if err := lease.acquire(path); err != nil {
			return validationFailure("ownership", "ownership", "Another validation owns this run or repository; inspect its owner")
		}
	}
	head, err := cleanHead(dir)
	if err != nil {
		return err
	}
	owner, err := identifyProcess(os.Getpid())
	if err != nil {
		return err
	}
	id := rand.Text()
	logPath := filepath.Join(a.dir, "checks", head+"."+id+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	v := validationRun{processIdentity: owner, Version: 1, ID: id, Config: a.path, ConfigHash: a.state.ConfigHash, Repository: canonical, Worktree: dir, Head: head, Commands: commands, Status: "running", Started: time.Now().UTC(), Log: logPath}
	metadata := filepath.Join(a.dir, "validation-run.json")
	if err := writeJSON(filepath.Join(a.dir, "validation.json"), owner); err != nil {
		return err
	}
	if err := writeJSON(metadata, v); err != nil {
		return err
	}
	var log capture
	log.limit = 1 << 20
	// Persist an explicit outcome before releasing either lock. A vanished process
	// without this terminal record is never evidence of success.
	defer func() {
		if log.truncated {
			_, _ = log.b.WriteString("\n[diagnostics truncated at 1 MiB]\n")
		}
		result = errors.Join(result, writeAtomic(logPath, log.b.Bytes()))
		// Retain the legacy diagnostic location for existing gate-repair adoption.
		result = errors.Join(result, writeAtomic(filepath.Join(a.dir, "checks", head+".log"), log.b.Bytes()))
		if result == nil {
			result = writeJSON(a.evidencePath(head), evidence{Head: head, Commands: commands, Log: logPath})
		}
		finished := time.Now().UTC()
		v.Finished, v.Child = &finished, processIdentity{}
		v.Status = "passed"
		if result != nil {
			v.Status = "failed"
			var known *executionFailure
			if !errors.As(result, &known) {
				known = validationFailure("incomplete", "integrity", "Validation did not complete: "+activityText(result.Error())+"; diagnostics: "+logPath)
			}
			v.Failure = &known.Blocker
		}
		result = errors.Join(result, writeJSON(metadata, v))
		// The validator may be this long-lived controller. A completed batch must
		// not leave its still-live controller PID looking like an active old validator.
		result = errors.Join(result, writeJSON(filepath.Join(a.dir, "validation.json"), processIdentity{}))
	}()
	if a.activity != nil {
		a.activity.begin("Controller validation", "validation.started")
		defer a.activity.end()
	}
	for i, cmd := range commands {
		v.Check, v.CheckStart, v.Child = i+1, time.Now().UTC(), processIdentity{}
		if err := writeJSON(metadata, v); err != nil {
			return err
		}
		a.printf(activityRunning, "Check %d/%d: %s\n", i+1, len(commands), cmd)
		if a.activity != nil {
			a.activity.check(i+1, len(commands), cmd)
		}
		_, _ = fmt.Fprintf(&log, "$ %s\n", cmd)
		outputStart := log.b.Len()
		child, err := executionCommand(context.Background(), dir, "sh", "-ec", cmd)
		if err != nil {
			return err
		}
		child.Stdout, child.Stderr = &log, &log
		if err := child.Start(); err != nil {
			return err
		}
		v.Child, err = identifyProcess(child.Process.Pid)
		if errors.Is(err, os.ErrNotExist) {
			v.Child, err = processIdentity{}, nil
		}
		if err == nil {
			err = writeJSON(metadata, v)
		}
		waitErr := child.Wait()
		if err != nil {
			return errors.Join(err, waitErr)
		}
		if waitErr != nil {
			v.ExitCode = child.ProcessState.ExitCode()
			v.FailureFamily = validationFailureFamily(log.b.String()[outputStart:])
			return contextFailure("validation", "validation", fmt.Sprintf("Check %d failed; diagnostics: %s", i+1, logPath), 1)
		}
		after, err := cleanHead(dir)
		if err != nil {
			if a.state.Recovery != nil {
				return validationFailure("candidate_dirty", "integrity", "Validation candidate became dirty; retain the failed batch and diagnostics: "+logPath)
			}
			return err
		}
		if after != head {
			return errors.New("HEAD changed during validation; evidence discarded")
		}
		v.Completed, v.Child = i+1, processIdentity{}
		if err := writeJSON(metadata, v); err != nil {
			return err
		}
	}
	return nil
}
