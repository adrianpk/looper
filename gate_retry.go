package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"unicode/utf8"
)

// An environmental retry preserves the failed validation and authorizes one
// further execution of the frozen gate on that same clean candidate.
type gateRetry struct {
	Failure      gateFailure      `json:"failure"`
	ValidationID string           `json:"validation_id"`
	Reason       string           `json:"reason"`
	Result       *gateRetryResult `json:"result,omitempty"`
}

type gateRetryResult struct {
	ValidationID string `json:"validation_id"`
	Status       string `json:"status"`
}

func validRetryReason(reason string) bool {
	return reason != "" && strings.TrimSpace(reason) == reason && len(reason) <= 512 && utf8.ValidString(reason) && !strings.ContainsAny(reason, "\r\n")
}

func (a *app) pendingGateRetry() *gateRetry {
	if n := len(a.state.GateRetries); n != 0 && a.state.GateRetries[n-1].Result == nil {
		return &a.state.GateRetries[n-1]
	}
	return nil
}

func (a *app) verifyGateRetries() error {
	if len(a.state.GateRetries) > 100 {
		return errors.New("gate retry history exceeds 100 authorizations")
	}
	seen := map[string]bool{}
	for i, retry := range a.state.GateRetries {
		if !fullSHA.MatchString(retry.Failure.Head) || retry.Failure.Message == "" || !activityIdentifier.MatchString(retry.ValidationID) || seen[retry.ValidationID] || !validRetryReason(retry.Reason) {
			return errors.New("invalid gate retry authorization")
		}
		seen[retry.ValidationID] = true
		if result := retry.Result; result != nil {
			if !activityIdentifier.MatchString(result.ValidationID) || result.ValidationID == retry.ValidationID || result.Status != "passed" && result.Status != "failed" {
				return errors.New("invalid gate retry outcome")
			}
		} else if i != len(a.state.GateRetries)-1 || a.state.Next != len(a.config.Slices) || a.state.Gate != nil || a.state.Pending != nil || a.activeGateRepair() != nil || a.state.GateFailure != nil && *a.state.GateFailure != retry.Failure {
			return errors.New("inconsistent pending gate retry")
		}
	}
	return nil
}

// Use the validator's existing ownership and terminal-outcome checks. A focused
// check or another worktree cannot authorize or satisfy a full-gate retry.
func (a *app) gateRetryValidation(head string) (*validationRun, error) {
	v, active, err := a.validationState()
	if err != nil {
		return nil, err
	}
	if active || v == nil || v.Head != head || v.Worktree != a.config.Repo || !equalStrings(v.Commands, []string{a.config.FullGate}) {
		return nil, errors.New("gate retry requires an inactive exact-candidate full-gate outcome")
	}
	return v, nil
}

func authorizeGateRetry(path, head, reason string) error {
	if !fullSHA.MatchString(head) || !validRetryReason(reason) {
		return errors.New("retry-gate requires --failed-head and a single-line --reason of at most 512 bytes")
	}
	a, err := loadApp(path)
	if err != nil {
		return err
	}
	lease, err := acquireRun(a)
	if err != nil {
		return err
	}
	defer lease.close()
	a, err = loadApp(path)
	if err != nil {
		return err
	}
	if err := requireIdleWorker(a); err != nil {
		return err
	}
	if a.state.Next != len(a.config.Slices) || a.state.Gate != nil || a.state.Pending != nil || a.activeGateRepair() != nil || a.state.GateFailure == nil || a.state.GateFailure.Head != head {
		return errors.New("gate retry requires integrated slices, their retained failure, and no pending repair or merge")
	}
	current, err := cleanHead(a.config.Repo)
	if err != nil {
		return err
	}
	if current != head {
		return errors.New("gate retry requires the exact clean failed candidate")
	}
	v, err := a.gateRetryValidation(head)
	if err != nil {
		return err
	}
	if v.Status != "failed" || !strings.Contains(a.state.GateFailure.Message, v.Log) {
		return errors.New("gate retry does not match the retained failed validation")
	}
	log, err := os.ReadFile(v.Log)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(log), "$ "+a.config.FullGate+"\n") {
		return errors.New("retained diagnostics do not belong to the frozen full gate")
	}
	for _, retry := range a.state.GateRetries {
		if retry.ValidationID == v.ID {
			if retry.Result == nil && retry.Failure == *a.state.GateFailure && retry.Reason == reason {
				return nil
			}
			return errors.New("this validation already has a gate retry authorization")
		}
	}
	if a.pendingGateRetry() != nil {
		return errors.New("another gate retry authorization is still pending")
	}
	if len(a.state.GateRetries) >= 100 {
		return errors.New("gate retry authorization limit reached")
	}
	a.state.GateRetries = append(a.state.GateRetries, gateRetry{Failure: *a.state.GateFailure, ValidationID: v.ID, Reason: reason})
	return a.save()
}

// Recover a completed attempt before starting anything else. An unknown or
// replaced batch blocks recovery rather than spending the authorization again.
func (a *app) reconcileGateRetry() error {
	retry := a.pendingGateRetry()
	if retry == nil {
		return nil
	}
	head, err := cleanHead(a.config.Repo)
	if err != nil {
		return err
	}
	if head != retry.Failure.Head {
		return errors.New("candidate changed after gate retry authorization")
	}
	v, err := a.gateRetryValidation(head)
	if err != nil {
		return err
	}
	if v.ID == retry.ValidationID {
		if v.Status != "failed" {
			return errors.New("authorized source validation is no longer failed")
		}
		a.state.GateFailure = nil
		return a.save()
	}
	if v.Status == "failed" {
		retry.Result = &gateRetryResult{ValidationID: v.ID, Status: v.Status}
		a.state.GateFailure = &gateFailure{Head: head, Message: fmt.Sprintf("Authorized gate retry failed; diagnostics: %s", v.Log)}
		return a.save()
	}
	var receipt evidence
	if err := readJSON(a.evidencePath(head), &receipt); err != nil {
		return err
	}
	if receipt.Head != head || !equalStrings(receipt.Commands, v.Commands) || receipt.Log != v.Log {
		return errors.New("completed gate retry has no matching validation receipt")
	}
	retry.Result = &gateRetryResult{ValidationID: v.ID, Status: v.Status}
	a.state.GateFailure = nil
	a.state.Gate = &receipt
	return a.save()
}

func (a *app) finishGateRetry(status string) error {
	retry := a.pendingGateRetry()
	if retry == nil {
		return nil
	}
	v, err := a.gateRetryValidation(retry.Failure.Head)
	if err != nil {
		return err
	}
	if v.ID == retry.ValidationID || v.Status != status {
		return errors.New("gate retry has no new matching terminal outcome")
	}
	retry.Result = &gateRetryResult{ValidationID: v.ID, Status: status}
	return nil
}
