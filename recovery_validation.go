package main

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

var failureFamilyName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/-]{0,127}$`)

// An approved check can identify its failing case without making candidate
// hashes or worker-supplied diagnoses replenish incident budgets.
func validationFailureFamily(output string) string {
	family := ""
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, "LOOPER_FAILURE_FAMILY="); ok {
			if !failureFamilyName.MatchString(value) || family != "" && family != value {
				return ""
			}
			family = value
		}
	}
	return family
}

func (a *app) observeRecoveryValidation() error {
	if a.state.Recovery == nil {
		return nil
	}
	v, active, err := a.validationState()
	if err != nil {
		return err
	}
	if active || v == nil {
		return nil
	}
	r := a.state.Recovery
	if slices.Contains(r.Observed, v.ID) {
		return nil
	}
	if len(r.Observed) >= 1024 {
		return recoveryStop("validation_limit", "budget", "Recovery validation history limit reached")
	}
	r.Observed = append(r.Observed, v.ID)
	if v.Status == "passed" {
		var e evidence
		head, err := cleanHead(v.Worktree)
		if err != nil || head != v.Head {
			return a.save()
		} // A later candidate cannot credit an earlier passing batch.
		if readJSON(a.evidencePath(head), &e) != nil || e.Head != head || e.Log != v.Log || !equalStrings(e.Commands, v.Commands) {
			return recoveryStop("validation_evidence", "integrity", "Verified passing checks are required before recording recovery progress")
		}
		for i := range r.Families {
			for _, command := range v.Commands {
				if r.Families[i].Operation == digest([]byte(command)) {
					r.Families[i].FailedRepairs = 0
					r.Families[i].Quarantines = 0
				}
			}
		}
	}
	if v.Status == "failed" && v.Failure != nil && v.Failure.Class == "validation" && v.ExitCode != 75 {
		check := max(0, min(v.Check-1, len(v.Commands)-1))
		key := digest([]byte("validation\x00" + v.Commands[check] + "\x00" + v.FailureFamily))
		family, err := a.recoveryFamily(key)
		if err != nil {
			return err
		}
		family.Operation = digest([]byte(v.Commands[check]))
		// The initial red gate does not consume a repair attempt. Completed repair
		// candidates that remain red do, including across different commits.
		isRepair := a.activeGateRepair() != nil || a.replyPhase == "repair" || len(a.state.GateRepairs) > 0 && v.Worktree == a.config.Repo
		if isRepair {
			family.FailedRepairs++
			if family.FailedRepairs >= r.Policy.FailedRepairsPerFamily {
				return errors.Join(recoveryStop("repairs_stalled", "technical", "The same failure family remained unresolved after the delegated failed-repair allowance"), a.save())
			}
		}
	}
	return a.save()
}

// Exit 75 is the approved command's explicit temporary-failure contract. Error
// text, arbitrary worker claims and ordinary failing assertions cannot mint retries.
func (a *app) tryRecoveryGateRetry() (bool, error) {
	r := a.state.Recovery
	if r == nil || a.state.GateFailure == nil || a.activeGateRepair() != nil {
		return false, nil
	}
	v, active, err := a.validationState()
	if err != nil {
		return false, err
	}
	if active || v == nil || v.Status != "failed" || v.ExitCode != 75 || v.Failure == nil || v.Failure.Class != "validation" || v.Worktree != a.config.Repo || v.Head != a.state.GateFailure.Head || !equalStrings(v.Commands, []string{a.config.FullGate}) {
		return false, nil
	}
	if err := a.recoveryAdmission(); err != nil {
		return false, err
	}
	return a.reserveAutomaticGateRetry(v, "Approved full gate reported temporary failure with exit status 75")
}

func (a *app) reserveAutomaticGateRetry(v *validationRun, reason string) (bool, error) {
	r := a.state.Recovery
	if pending := a.pendingGateRetry(); pending != nil && pending.ValidationID == v.ID {
		return true, a.reconcileGateRetry()
	}
	for _, retry := range a.state.GateRetries {
		if retry.Automatic && retry.Failure.Head == v.Head {
			return false, recoveryStop("gate_candidate_exhausted", "budget", "The delegated environmental retry for this candidate is exhausted")
		}
	}
	if r.GateRetries >= r.Policy.EnvironmentalRetries {
		return false, recoveryStop("gate_retries_exhausted", "budget", "The run-wide environmental gate retry allowance is exhausted")
	}
	r.GateRetries++
	prior := len(a.state.GateRetries)
	if err := a.appendGateRetry(v.Head, reason, true); err != nil {
		if len(a.state.GateRetries) == prior {
			r.GateRetries--
		}
		return false, err
	}
	return true, a.reconcileGateRetry()
}

// Cleanup can establish a narrowly identified environmental cause, but cannot
// turn the dirty batch into a pass or rerun the full gate outside its allowance.
func (a *app) retryAfterOutputQuarantine(v *validationRun) error {
	if v == nil || v.Status != "failed" || v.Failure == nil || v.Failure.Code != "validation_candidate_dirty" || v.Worktree != a.config.Repo || !equalStrings(v.Commands, []string{a.config.FullGate}) {
		return nil
	}
	head, err := cleanHead(a.config.Repo)
	if err != nil || head != v.Head {
		return recoveryStop("output_candidate", "integrity", "Quarantine did not restore the exact clean validation candidate")
	}
	if a.state.Pending != nil || a.activeGateRepair() != nil || a.state.Gate != nil {
		return recoveryStop("output_phase", "integrity", "Output recovery cannot replace an active repair, merge or successful gate")
	}
	if pending := a.pendingGateRetry(); pending != nil && pending.ValidationID != v.ID {
		if err := a.reconcileGateRetry(); err != nil {
			return err
		}
	}
	a.state.GateFailure = &gateFailure{Head: v.Head, Message: v.Failure.Message}
	if a.state.ValidationWait != nil && a.state.ValidationWait.ID == v.ID {
		a.state.ValidationWait = nil
	}
	_, err = a.reserveAutomaticGateRetry(v, "Quarantined the exclusively attributed output that dirtied the completed gate candidate")
	return err
}
