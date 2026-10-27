package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const capacityReason = "provider_capacity"

// Count automatic launches across the entire run, including interrupted launches.
// Instruction and Before retain only the existing controller task and progress baseline.
type providerRetry struct {
	Reason      string     `json:"reason"`
	Attempts    int        `json:"attempts"`
	NextAt      *time.Time `json:"next_retry_at,omitempty"`
	Phase       string     `json:"phase,omitempty"`
	Instruction string     `json:"instruction,omitempty"`
	Before      string     `json:"before,omitempty"`
}

type providerRetryInfo struct {
	Reason    string     `json:"reason"`
	Attempts  int        `json:"attempts"`
	Limit     int        `json:"limit"`
	NextRetry int        `json:"next_retry,omitempty"`
	NextAt    *time.Time `json:"next_retry_at,omitempty"`
	Phase     string     `json:"phase,omitempty"`
}

var capacityDelays = [...]time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute}

// The clock boundary allows recovery tests to cross persisted deadlines without real minute-long waits.
type workerRetryClock interface {
	Now() time.Time
	WaitUntil(time.Time) error
}

func (a *app) retryNow() time.Time {
	if a.retryClock != nil {
		return a.retryClock.Now()
	}
	return time.Now()
}

func (a *app) waitRetry(at time.Time) error {
	if a.retryClock != nil {
		return a.retryClock.WaitUntil(at)
	}
	time.Sleep(max(0, time.Until(at)))
	return nil
}

func (a *app) retryInfo() *providerRetryInfo {
	r := a.state.ProviderRetry
	if r == nil {
		return nil
	}
	i := &providerRetryInfo{Reason: r.Reason, Attempts: r.Attempts, Limit: len(capacityDelays), NextAt: r.NextAt, Phase: r.Phase}
	if r.NextAt != nil {
		i.NextRetry = r.Attempts + 1
	}
	return i
}

func retryDescription(i *providerRetryInfo) string {
	if i.NextAt != nil {
		return fmt.Sprintf("%s: automatic retry %d/%d at %s (phase %s)", i.Reason, i.NextRetry, i.Limit, i.NextAt.UTC().Format(time.RFC3339), i.Phase)
	}
	return fmt.Sprintf("%s: %d/%d automatic retries used", i.Reason, i.Attempts, i.Limit)
}

func (a *app) verifyProviderRetry() error {
	r := a.state.ProviderRetry
	if r == nil {
		return nil
	}
	validPhase := r.Phase == "implement" || r.Phase == "repair" || r.Phase == "prepare_gate" || r.Phase == "repair_gate" || r.Phase == "close" || r.Phase == "diagnose"
	if r.Reason != capacityReason || r.Attempts < 0 || r.Attempts > len(capacityDelays) || r.Phase != "" && (!validPhase || r.Instruction == "" || a.state.Session == "") || r.NextAt != nil && (r.Phase == "" || r.NextAt.IsZero() || r.Attempts == len(capacityDelays)) {
		return errors.New("invalid provider retry checkpoint")
	}
	return nil
}

func (a *app) capacityFailure(exhausted bool) *executionFailure {
	f := contextFailure("provider", "capacity", "Selected model is temporarily at capacity; the conversation and partial work are retained", 1)
	if exhausted {
		f.Blocker.Code = "provider_capacity_exhausted"
		f.Blocker.Message = "Automatic capacity retry allowance exhausted; restore provider capacity, then explicitly resume the retained run"
	}
	if a.state.ProviderRetry != nil {
		f.Blocker.Attempts = a.state.ProviderRetry.Attempts
	}
	path := a.path
	var owner sequenceOwner
	if readJSON(filepath.Join(a.dir, "sequence-owner.json"), &owner) == nil {
		path = owner.Config
	}
	f.Blocker.NextAction = commandAction("looper", "resume", "--config", path, "--detach", "--json")
	return f
}

func actionCommand(action *runAction) string {
	words := make([]string, len(action.Argv))
	for i, word := range action.Argv {
		words[i] = shellQuote(word)
	}
	return strings.Join(words, " ")
}

func (a *app) clearRetryPhase() {
	if r := a.state.ProviderRetry; r != nil {
		r.NextAt, r.Phase, r.Instruction, r.Before = nil, "", "", ""
	}
}

func (a *app) resumeProviderRetry() error {
	r := a.state.ProviderRetry
	if r == nil || r.Phase == "" {
		return nil
	}
	if r.NextAt == nil {
		// A reserved retry may have run before the controller disappeared. Do not guess its outcome.
		a.clearRetryPhase()
		return errors.Join(contextFailure("worker", "execution", "Controller interrupted an automatic worker retry; inspect partial work before explicitly resuming", 1), a.save())
	}
	return a.work(r.Phase, r.Instruction)
}

func (a *app) workerWithRecovery(phase, task, before string) error {
	for {
		if err := a.waitValidation(); err != nil {
			return err
		}
		if a.state.Turns >= a.config.MaxTurns {
			return errors.New("worker turn limit reached; inspect the checkpoint before continuing")
		}
		r := a.state.ProviderRetry
		retrying := r != nil && r.Phase != ""
		if retrying {
			if r.NextAt == nil {
				return contextFailure("worker", "execution", "Automatic retry has no waiting deadline; inspect the retained checkpoint", 1)
			}
			a.state.Status, a.state.Blocker = "waiting", &a.capacityFailure(false).Blocker
			if err := a.save(); err != nil {
				return err
			}
			a.printf(activityWarning, "%s.\n", retryDescription(a.retryInfo()))
			if err := a.waitRetry(*r.NextAt); err != nil {
				return err
			}
			if err := a.requireRepositoryOwnership(); err != nil {
				return err
			}
			if a.state.Pending != nil {
				return contextFailure("worker", "integrity", "An uncertain merge must be reconciled before retrying a worker", 1)
			}
			if err := a.fetch(); err != nil {
				return err
			}
			if err := a.verifyIdentity(); err != nil {
				return err
			}
		}
		if err := a.waitValidation(); err != nil {
			return err
		}
		if err := requireIdleWorker(a); err != nil {
			return err
		}
		if err := a.preflight(); err != nil {
			return err
		}
		guard, err := a.guardValidation()
		if err != nil {
			return err
		}
		var repairedValidation string
		if v, _, err := a.validationState(); err != nil {
			guard.close()
			return err
		} else if v != nil && v.Status == "failed" && v.Failure.Class == "validation" {
			repairedValidation = v.ID
		}
		if retrying {
			r.Attempts++
			r.NextAt = nil
		}
		// Reserve both budgets before launch: interruption cannot refund a worker turn or retry.
		if phase == "diagnose" {
			if err := a.reserveDiagnostic(); err != nil {
				guard.close()
				return err
			}
		}
		a.state.Turns++
		if a.state.Recovery != nil && phase != "diagnose" {
			a.state.Recovery.Phase, a.state.Recovery.Instruction = phase, task
		}
		a.state.Status, a.state.Feedback, a.state.Blocker = "running", task, nil
		if err := a.save(); err != nil {
			guard.close()
			return err
		}
		guard.close()
		err = a.invokeWorker(phase, task)
		if err == nil && repairedValidation != "" {
			a.state.ValidationHandled = repairedValidation
		}
		var failure *executionFailure
		if !errors.As(err, &failure) || failure.Blocker.Code != capacityReason {
			a.clearRetryPhase()
			return errors.Join(err, a.save())
		}
		if a.state.Session == "" {
			return contextFailure("worker", "execution", "Capacity failure supplied no resumable conversation; inspect the worker before continuing", 1)
		}
		if r == nil {
			r = &providerRetry{Reason: capacityReason}
			a.state.ProviderRetry = r
		}
		if r.Attempts == len(capacityDelays) {
			a.clearRetryPhase()
			failure := a.capacityFailure(true)
			a.printf(activityFailure, "provider_capacity_exhausted: restore capacity, then run: %s\n", actionCommand(failure.Blocker.NextAction))
			return errors.Join(failure, a.save())
		}
		next := a.retryNow().Add(capacityDelays[r.Attempts]).UTC()
		r.NextAt, r.Phase, r.Instruction, r.Before = &next, phase, task, before
	}
}

type workerEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Code     string `json:"code"`
	Error    *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error"`
}

func (e workerEvent) failure() *executionFailure {
	message, code := e.Message, e.Code
	if e.Error != nil {
		message, code = e.Error.Message, e.Error.Code
	}
	capacity := code == "server_overloaded"
	if code == "" {
		// Codex exec 0.161 emits only message for error/turn.failed, dropping the provider code.
		text := strings.TrimSpace(message)
		for _, prefix := range []string{"unexpected status 503 Service Unavailable: ", "unexpected status 429 Too Many Requests: "} {
			if strings.HasPrefix(text, prefix) {
				text, _, _ = strings.Cut(strings.TrimPrefix(text, prefix), ", url: ")
				break
			}
		}
		capacity = text == "Selected model is at capacity. Please try a different model."
	}
	if capacity {
		return contextFailure("provider", "capacity", "Selected model is temporarily at capacity; the conversation and partial work are retained", 1)
	}
	if code == "" || code == "unauthorized" {
		f := classifyAccess("worker", errors.New(message), 1)
		if f.Blocker.Class == "authentication" {
			return f
		}
	}
	return contextFailure("worker", "execution", "Worker failed; inspect the retained conversation and host Codex diagnostics", 1)
}
