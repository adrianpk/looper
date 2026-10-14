package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type validationInfo struct {
	ID           string    `json:"validation_id"`
	Head         string    `json:"head"`
	Status       string    `json:"status"`
	Check        int       `json:"check"`
	Total        int       `json:"total_checks"`
	Completed    int       `json:"completed_checks"`
	Command      string    `json:"command,omitempty"`
	Started      time.Time `json:"started_at"`
	Elapsed      int64     `json:"elapsed_seconds"`
	CheckElapsed int64     `json:"check_elapsed_seconds,omitempty"`
	Log          string    `json:"log"`
}

func (v *validationRun) info() *validationInfo {
	end := time.Now()
	if v.Finished != nil {
		end = *v.Finished
	}
	i := &validationInfo{ID: v.ID, Head: v.Head, Status: v.Status, Check: v.Check, Total: len(v.Commands), Completed: v.Completed, Started: v.Started, Elapsed: max(0, int64(end.Sub(v.Started).Seconds())), Log: v.Log}
	if v.Check > 0 && v.Check <= len(v.Commands) {
		i.Command = activityCommand(v.Commands[v.Check-1])
		i.CheckElapsed = max(0, int64(end.Sub(v.CheckStart).Seconds()))
	}
	if v.Status == "running" && v.Child.PID == 0 {
		i.Status = "between_checks"
	}
	return i
}

func validationDescription(i *validationInfo) string {
	if i.Status == "between_checks" {
		return fmt.Sprintf("Validation between checks: %d/%d completed; elapsed %s", i.Completed, i.Total, (time.Duration(i.Elapsed) * time.Second).String())
	}
	return fmt.Sprintf("Validation %s: check %d/%d (%s, elapsed %s), %d completed; total elapsed %s", i.Status, i.Check, i.Total, i.Command, (time.Duration(i.CheckElapsed) * time.Second).String(), i.Completed, (time.Duration(i.Elapsed) * time.Second).String())
}

func validationRecovery(a *app) *runBlocker {
	return &runBlocker{Component: "validation", Class: "interrupted", Code: "validation_controller_interrupted", Message: "Controller interrupted while validation was retained; resume this execution to observe and reconcile its actual outcome", Retryable: true, NextAction: commandAction("looper", "resume", "--config", a.path, "--detach", "--json")}
}

// An open descriptor alone does not prove ownership: fdinfo must identify its
// flock mode, and the process start time must still match after inspection.
func processOwnsLock(owner processIdentity, path, mode string) (bool, error) {
	target, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	base := filepath.Join("/proc", strconv.Itoa(owner.PID))
	files, err := os.ReadDir(filepath.Join(base, "fd"))
	if err != nil {
		return false, err
	}
	if len(files) > 65536 {
		return false, errors.New("validation descriptor inventory exceeds the safety limit")
	}
	for _, f := range files {
		held, err := os.Stat(filepath.Join(base, "fd", f.Name()))
		if err != nil || !os.SameFile(target, held) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, "fdinfo", f.Name()))
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 6 && fields[0] == "lock:" && fields[2] == "FLOCK" && fields[4] == mode && fields[5] == strconv.Itoa(owner.PID) {
				return processState(owner)
			}
		}
	}
	return false, nil
}

// Shared guards make worker admission and final PR verification atomic with
// respect to validator startup. Validators require exclusive ownership of both files.
func (a *app) guardValidation() (*runLease, error) {
	for {
		if err := a.waitValidation(); err != nil {
			return nil, err
		}
		repo, err := a.validationLock()
		if err != nil {
			return nil, err
		}
		l := &runLease{}
		err = l.acquireMode(repo, syscall.LOCK_SH)
		if err == nil {
			err = l.acquireMode(filepath.Join(a.dir, "validate.lock"), syscall.LOCK_SH)
		}
		if err == nil {
			return l, nil
		}
		l.close()
		_, active, inspectErr := a.validationState()
		if inspectErr != nil {
			return nil, inspectErr
		}
		if !active {
			return nil, validationFailure("ownership", "ownership", "Validation admission guard is unavailable or has an uncertain owner")
		}
	}
}

func (a *app) controllerValidationGuard(repo, run string) bool {
	local, _ := identifyProcess(os.Getpid())
	var runtime runRuntime
	_ = readJSON(filepath.Join(a.dir, "runtime.json"), &runtime)
	for _, owner := range []processIdentity{local, runtime.Controller} {
		first, _ := processOwnsLock(owner, repo, "READ")
		second, _ := processOwnsLock(owner, run, "READ")
		if first && second {
			return true
		}
	}
	return false
}

var errValidationTransition = errors.New("validation finished during ownership inspection")

func (a *app) validationState() (*validationRun, bool, error) {
	for range 2 {
		v, active, err := a.validationSnapshot()
		if !errors.Is(err, errValidationTransition) {
			return v, active, err
		}
	}
	return nil, false, validationFailure("ownership", "ownership", "Validation ownership changed repeatedly during inspection")
}

func (a *app) validationSnapshot() (*validationRun, bool, error) {
	lock := filepath.Join(a.dir, "validate.lock")
	busy, err := lockBusy(lock)
	if err != nil {
		return nil, false, err
	}
	repoLock, err := a.validationLock()
	if err != nil {
		return nil, false, err
	}
	repoBusy, err := lockBusy(repoLock)
	if err != nil {
		return nil, false, err
	}
	if (busy || repoBusy) && a.controllerValidationGuard(repoLock, lock) {
		busy, repoBusy = false, false
	}
	var v validationRun
	err = readJSON(filepath.Join(a.dir, "validation-run.json"), &v)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, validationFailure("ownership", "ownership", "Validation batch metadata is unreadable; inspect validation-run.json")
	}
	if err == nil && v.Version == 0 {
		return nil, false, validationFailure("ownership", "ownership", "Validation batch metadata has an unsupported schema")
	}
	if v.Version == 0 {
		if err := readJSON(filepath.Join(a.dir, "validation.json"), &v.processIdentity); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, false, err
		}
		alive, ownerErr := processState(v.processIdentity)
		if busy || repoBusy || alive || ownerErr != nil {
			return nil, false, validationFailure("ownership", "ownership", "Active or uncertain validation has no verifiable batch identity; wait for its owner and inspect retained evidence")
		}
		if a.state.ValidationWait != nil {
			return nil, false, validationFailure("outcome_missing", "integrity", "Retained validation has no batch outcome; inspect its diagnostics before resuming")
		}
		return nil, false, nil
	}
	if v.Version != 1 || !activityIdentifier.MatchString(v.ID) || v.Config != a.path || v.ConfigHash != a.state.ConfigHash || v.Repository != filepath.Dir(repoLock) || !fullSHA.MatchString(v.Head) || len(v.Commands) == 0 || len(v.Commands) > 100 || !filepath.IsAbs(v.Worktree) || v.Started.IsZero() || v.Check < 0 || v.Check > len(v.Commands) || v.Completed < 0 || v.Completed > len(v.Commands) || v.Status != "running" && v.Status != "passed" && v.Status != "failed" || v.Log != filepath.Join(a.dir, "checks", v.Head+"."+v.ID+".log") {
		return nil, false, validationFailure("ownership", "ownership", "Validation batch identity does not match this execution")
	}
	if w := a.state.ValidationWait; w != nil && (w.ID != v.ID || w.Owner != v.processIdentity || w.Head != v.Head || w.Worktree != v.Worktree || !equalStrings(w.Commands, v.Commands)) {
		return nil, false, validationFailure("ownership", "ownership", "The retained validation batch was replaced; inspect both owners and outcomes")
	}
	if !busy && !repoBusy {
		if v.Status == "running" {
			return nil, false, validationFailure("outcome_missing", "integrity", "Validation released ownership without a terminal outcome; inspect its retained diagnostics")
		}
		if v.Finished == nil || v.Child.PID != 0 || v.Status == "failed" && v.Failure == nil || v.Status == "passed" && (v.Completed != len(v.Commands) || v.Failure != nil) {
			return nil, false, validationFailure("outcome_inconsistent", "integrity", "Validation terminal outcome is incomplete or contradictory")
		}
		return &v, false, nil
	}
	root, err := projectIdentity(v.Worktree)
	if err != nil {
		return nil, false, err
	}
	canonical, err := projectIdentity(a.config.Repo)
	if err != nil {
		return nil, false, err
	}
	if root != canonical || v.Repository != canonical {
		return nil, false, validationFailure("ownership", "ownership", "Active validation belongs to another repository")
	}
	branch, err := git(v.Worktree, "branch", "--show-current")
	if err != nil {
		return nil, false, err
	}
	approved := "dev"
	if a.state.Next < len(a.config.Slices) {
		approved = a.config.Slices[a.state.Next].Branch
	}
	if repair := a.activeGateRepair(); repair != nil {
		approved = repair.Identity.Branch
	}
	if branch != approved {
		return nil, false, validationFailure("ownership", "ownership", "Validation candidate is outside the current approved delivery unit")
	}
	if !busy || !repoBusy {
		var latest validationRun
		if readJSON(filepath.Join(a.dir, "validation-run.json"), &latest) == nil && latest.ID == v.ID && latest.Status != "running" {
			return nil, false, errValidationTransition
		}
		return nil, false, validationFailure("ownership", "ownership", "Validation does not hold both execution and repository validation locks")
	}
	for _, path := range []string{lock, repoLock} {
		owned, err := processOwnsLock(v.processIdentity, path, "WRITE")
		if err != nil || !owned {
			// The batch may have finished between metadata and lock inspection.
			if stillBusy, checkErr := lockBusy(lock); checkErr == nil && !stillBusy {
				return nil, false, errValidationTransition
			}
			return nil, false, validationFailure("ownership", "ownership", "Validation lock ownership cannot be matched to its recorded process and start time")
		}
	}
	return &v, true, nil
}

func (a *app) requireAvailableWorker() error {
	if err := requireIdleWorkerProcess(a); err != nil {
		return err
	}
	_, _, err := a.validationState()
	return err
}

func (a *app) waitValidation() error {
	if err := requireIdleWorkerProcess(a); err != nil {
		return err
	}
	v, active, err := a.validationState()
	if err != nil {
		return err
	}
	if !active && a.state.ValidationWait == nil {
		return nil
	}
	if err := a.requireRepositoryOwnership(); err != nil {
		return err
	}
	if a.state.Pending != nil {
		return validationFailure("ownership", "ownership", "An uncertain merge must be reconciled before adopting validation")
	}
	if a.state.ValidationWait == nil {
		a.state.ValidationWait = &validationWait{ID: v.ID, Owner: v.processIdentity, Worktree: v.Worktree, Head: v.Head, Commands: append([]string(nil), v.Commands...)}
	}
	a.state.Status, a.state.Blocker = "waiting", nil
	if err := a.save(); err != nil {
		return err
	}
	var lastCheck int
	var lastStatus string
	lastPrint := time.Time{}
	for active {
		info := v.info()
		if info.Check != lastCheck || info.Status != lastStatus || time.Since(lastPrint) >= 30*time.Second {
			if a.output != nil {
				a.output.emit(time.Now(), "validation.waiting", "", validationDescription(info), activityRunning)
			} else {
				a.printf(activityRunning, "%s\n", validationDescription(info))
			}
			lastCheck, lastStatus, lastPrint = info.Check, info.Status, time.Now()
		}
		// Observation has no validation deadline and spends no execution budgets.
		time.Sleep(2 * time.Second)
		if err := a.requireRepositoryOwnership(); err != nil {
			return err
		}
		if err := requireIdleWorkerProcess(a); err != nil {
			return err
		}
		v, active, err = a.validationState()
		if err != nil {
			return err
		}
	}
	root, err := projectIdentity(v.Worktree)
	if err != nil || root != v.Repository {
		return validationFailure("ownership", "ownership", "Retained validation repository identity changed")
	}
	head, err := cleanHead(v.Worktree)
	if err != nil || head != v.Head {
		return validationFailure("candidate_changed", "integrity", "Validation candidate changed or became dirty; retained evidence must not be reused")
	}
	if v.Status == "passed" {
		var e evidence
		if err := readJSON(a.evidencePath(v.Head), &e); err != nil || e.Head != v.Head || e.Log != v.Log || !equalStrings(e.Commands, v.Commands) {
			return validationFailure("outcome_inconsistent", "integrity", "Successful validation has no matching exact-HEAD, ordered-command evidence")
		}
		if a.output != nil {
			a.output.emit(time.Now(), "validation.completed", "", validationDescription(v.info()), activitySuccess)
		}
	} else {
		a.state.Feedback = v.Failure.Message
		if a.output != nil {
			a.output.emit(time.Now(), "validation.failed", "", v.Failure.Message, activityFailure)
		}
		if v.Failure.Class != "validation" {
			return &executionFailure{Blocker: *v.Failure}
		}
	}
	if err := a.fetch(); err != nil {
		return err
	}
	if err := a.verifyIdentity(); err != nil {
		return err
	}
	a.state.ValidationWait = nil
	a.state.Status = "running"
	return a.save()
}
