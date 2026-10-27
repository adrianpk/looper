package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Recovery authority is pinned independently of the delivery configuration.
// The profile never increases an existing worker or repair allowance.
type recoveryPolicy struct {
	Version                int               `json:"version"`
	Profile                string            `json:"profile"`
	GateRepair             *gateRepairPolicy `json:"gate_repair,omitempty"`
	ExcludedPaths          []string          `json:"excluded_paths,omitempty"`
	Outputs                []outputPolicy    `json:"outputs,omitempty"`
	DiagnosticsPerFamily   int               `json:"diagnostics_per_family"`
	DiagnosticsPerRun      int               `json:"diagnostics_per_run"`
	FailedRepairsPerFamily int               `json:"failed_repairs_per_family"`
	EnvironmentalRetries   int               `json:"environmental_retries"`
}

type recoveryFamily struct {
	Key           string `json:"key"`
	Operation     string `json:"operation,omitempty"`
	Diagnostics   int    `json:"diagnostics"`
	FailedRepairs int    `json:"failed_repairs"`
	Quarantines   int    `json:"quarantines"`
}

type recoveryRequest struct {
	Version      int      `json:"version"`
	Kind         string   `json:"kind"`
	Paths        []string `json:"paths,omitempty"`
	Candidate    string   `json:"candidate_head,omitempty"`
	ValidationID string   `json:"validation_id,omitempty"`
	IncidentID   int      `json:"incident_id,omitempty"`
	ActionID     int      `json:"action_id,omitempty"`
}

type recoveryIncident struct {
	ID           int              `json:"id"`
	Code         string           `json:"code"`
	Family       string           `json:"family"`
	Phase        string           `json:"phase"`
	Instruction  string           `json:"instruction,omitempty"`
	Candidate    string           `json:"candidate_head"`
	ValidationID string           `json:"validation_id,omitempty"`
	Message      string           `json:"message"`
	Status       string           `json:"status"`
	Action       string           `json:"action,omitempty"`
	ActionID     int              `json:"action_id,omitempty"`
	Uncertain    bool             `json:"uncertain,omitempty"`
	Request      *recoveryRequest `json:"request,omitempty"`
	Result       *workerResult    `json:"result,omitempty"`
}

type recoveryState struct {
	Policy        recoveryPolicy     `json:"policy"`
	PolicyHash    string             `json:"policy_hash"`
	Families      []recoveryFamily   `json:"families,omitempty"`
	Incidents     []recoveryIncident `json:"incidents,omitempty"`
	Diagnostics   int                `json:"diagnostics"`
	GateRetries   int                `json:"environmental_retries"`
	Observed      []string           `json:"observed_validations,omitempty"`
	OutputBatches []string           `json:"output_batches,omitempty"`
	Phase         string             `json:"phase,omitempty"`
	Instruction   string             `json:"instruction,omitempty"`
}

type recoveryInfo struct {
	Profile                 string `json:"profile"`
	Diagnostics             int    `json:"diagnostics"`
	DiagnosticLimit         int    `json:"diagnostic_limit"`
	EnvironmentalRetries    int    `json:"environmental_retries"`
	EnvironmentalRetryLimit int    `json:"environmental_retry_limit"`
	Incident                int    `json:"incident,omitempty"`
	Code                    string `json:"code,omitempty"`
	Phase                   string `json:"phase,omitempty"`
	Status                  string `json:"status,omitempty"`
	Action                  string `json:"action,omitempty"`
}

func (a *app) recoveryInfo() *recoveryInfo {
	r := a.state.Recovery
	if r == nil {
		return nil
	}
	info := &recoveryInfo{Profile: r.Policy.Profile, Diagnostics: r.Diagnostics, DiagnosticLimit: r.Policy.DiagnosticsPerRun, EnvironmentalRetries: r.GateRetries, EnvironmentalRetryLimit: r.Policy.EnvironmentalRetries}
	if i := a.activeIncident(); i != nil {
		info.Incident, info.Code, info.Phase, info.Status, info.Action = i.ID, i.Code, i.Phase, i.Status, i.Action
	}
	return info
}

// A condition is execution input, independent of human-readable worker text.
type recoveryCondition struct {
	Code, Phase, Message string
	Request              *recoveryRequest
}

func (e *recoveryCondition) Error() string { return e.Message }

func (p *recoveryPolicy) defaults() {
	if p.DiagnosticsPerFamily == 0 {
		p.DiagnosticsPerFamily = 2
	}
	if p.DiagnosticsPerRun == 0 {
		p.DiagnosticsPerRun = 8
	}
	if p.FailedRepairsPerFamily == 0 {
		p.FailedRepairsPerFamily = 3
	}
	if p.EnvironmentalRetries == 0 {
		p.EnvironmentalRetries = 3
	}
	if p.GateRepair != nil && p.GateRepair.MaxRepairs == 0 {
		p.GateRepair.MaxRepairs = 8
	}
}

func (a *app) validateRecoveryPolicy(p recoveryPolicy) error {
	if p.Version != 1 || p.Profile != "bounded-v1" || p.DiagnosticsPerFamily < 1 || p.DiagnosticsPerFamily > 8 || p.DiagnosticsPerRun < p.DiagnosticsPerFamily || p.DiagnosticsPerRun > 100 || p.FailedRepairsPerFamily < 1 || p.FailedRepairsPerFamily > 100 || p.EnvironmentalRetries < 1 || p.EnvironmentalRetries > 100 || len(p.Outputs) > 100 || len(p.ExcludedPaths) > 100 {
		return errors.New("recovery requires a bounded-v1 policy and finite diagnostic, repair and retry limits")
	}
	if p.GateRepair != nil {
		if err := a.validateGateRepairPolicy(*p.GateRepair); err != nil {
			return err
		}
	}
	for _, path := range p.ExcludedPaths {
		if !safeRecoveryPath(path) {
			return errors.New("recovery exclusions must be safe exact paths or directories")
		}
	}
	seen := map[string]bool{}
	for _, output := range p.Outputs {
		if !safeRecoveryPath(output.Path) || seen[output.Path] || strings.TrimSpace(output.Command) != output.Command || output.Command == "" || len(output.Command) > 8192 || strings.ContainsAny(output.Command, "\r\n\x00") {
			return errors.New("recovery outputs require unique exact paths and single-line producer commands")
		}
		seen[output.Path] = true
	}
	return nil
}

func safeRecoveryPath(path string) bool {
	return safePath(path) && path != "." && path != ".git" && !strings.HasPrefix(path, ".git/") && !strings.ContainsRune(path, '\x00')
}

func authorizeRecovery(path, policyPath string) error {
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
	if a.state.Status == "complete" || a.state.Pending != nil {
		return errors.New("recovery delegation requires an unfinished idle execution without an uncertain merge")
	}
	var policy recoveryPolicy
	if err := readJSON(policyPath, &policy); err != nil {
		return err
	}
	policy.defaults()
	if err := a.validateRecoveryPolicy(policy); err != nil {
		return err
	}
	if a.state.Recovery != nil {
		if reflect.DeepEqual(a.state.Recovery.Policy, policy) {
			return nil
		}
		return errors.New("recovery delegation is already pinned and cannot be replaced or widened")
	}
	if policy.GateRepair != nil {
		if a.state.GateRepairPolicy != nil && !reflect.DeepEqual(*a.state.GateRepairPolicy, *policy.GateRepair) {
			return errors.New("recovery delegation cannot replace existing gate repair authority")
		}
		a.state.GateRepairPolicy = policy.GateRepair
	}
	b, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	a.state.Recovery = &recoveryState{Policy: policy, PolicyHash: digest(b)}
	return a.save()
}

func (a *app) verifyRecovery() error {
	r := a.state.Recovery
	if r == nil {
		return nil
	}
	if err := a.validateRecoveryPolicy(r.Policy); err != nil {
		return err
	}
	b, err := json.Marshal(r.Policy)
	if err != nil {
		return err
	}
	if r.PolicyHash != digest(b) || r.Diagnostics < 0 || r.Diagnostics > r.Policy.DiagnosticsPerRun || r.GateRetries < 0 || r.GateRetries > r.Policy.EnvironmentalRetries || len(r.Families) > 512 || len(r.Incidents) > 512 || len(r.Observed) > 1024 || len(r.OutputBatches) > 512 {
		return errors.New("invalid retained recovery authority or accounting")
	}
	if r.Policy.GateRepair != nil && (a.state.GateRepairPolicy == nil || !reflect.DeepEqual(*r.Policy.GateRepair, *a.state.GateRepairPolicy)) {
		return errors.New("recovery gate repair authority changed")
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range r.Families {
		if f.Key == "" || seen[f.Key] || f.Diagnostics < 0 || f.Diagnostics > r.Policy.DiagnosticsPerFamily || f.FailedRepairs < 0 || f.FailedRepairs > r.Policy.FailedRepairsPerFamily || f.Quarantines < 0 || f.Quarantines > r.Policy.FailedRepairsPerFamily {
			return errors.New("invalid recovery failure-family accounting")
		}
		total += f.Diagnostics
		seen[f.Key] = true
	}
	if total != r.Diagnostics {
		return errors.New("recovery diagnostic totals do not match their families")
	}
	automatic := 0
	for _, retry := range a.state.GateRetries {
		if retry.Automatic {
			automatic++
		}
	}
	if automatic != r.GateRetries {
		return errors.New("automatic gate retry totals do not match their reservations")
	}
	for i, incident := range r.Incidents {
		if incident.ID != i+1 || !seen[incident.Family] || !fullSHA.MatchString(incident.Candidate) || incident.Code == "" || !validRecoveryPhase(incident.Phase) || incident.ActionID < 0 || incident.ActionID > 100 {
			return errors.New("invalid retained recovery incident")
		}
		switch incident.Status {
		case "open", "reserved", "result", "resolved", "blocked", "needs_decision":
		default:
			return errors.New("invalid recovery incident status")
		}
		if incident.Status != "resolved" && i != len(r.Incidents)-1 {
			return errors.New("recovery history contains an unfinished earlier incident")
		}
		if incident.Request != nil && !validRecoveryRequest(incident.Request) {
			return errors.New("invalid retained recovery request")
		}
	}
	return nil
}

func validRecoveryPhase(phase string) bool {
	switch phase {
	case "implement", "repair", "prepare_gate", "repair_gate", "close", "diagnose", "controller":
		return true
	}
	return false
}

func validRecoveryRequest(r *recoveryRequest) bool {
	if r == nil || r.Version != 1 || len(r.Paths) > 100 || r.Candidate != "" && !fullSHA.MatchString(r.Candidate) || r.ValidationID != "" && !activityIdentifier.MatchString(r.ValidationID) || r.IncidentID < 0 || r.IncidentID > 512 || r.ActionID < 0 || r.ActionID > 100 {
		return false
	}
	for _, path := range r.Paths {
		if len(path) > 4096 || strings.ContainsAny(path, "\r\n\x00") {
			return false
		}
	}
	switch r.Kind {
	case "repair", "paths", "quarantine", "wait", "decision":
		return true
	}
	return false
}

func (a *app) activeIncident() *recoveryIncident {
	if r := a.state.Recovery; r != nil && len(r.Incidents) != 0 {
		i := &r.Incidents[len(r.Incidents)-1]
		if i.Status != "resolved" {
			return i
		}
	}
	return nil
}

func (a *app) recoveryFamily(key string) (*recoveryFamily, error) {
	r := a.state.Recovery
	for i := range r.Families {
		if r.Families[i].Key == key {
			return &r.Families[i], nil
		}
	}
	if len(r.Families) >= 512 {
		return nil, errors.New("recovery failure-family limit reached")
	}
	r.Families = append(r.Families, recoveryFamily{Key: key})
	return &r.Families[len(r.Families)-1], nil
}

func recoveryStop(code, class, message string) *executionFailure {
	f := contextFailure("recovery", class, message, 1)
	f.Blocker.Code, f.Blocker.Retryable = "recovery_"+code, false
	f.Blocker.NextAction = inspectAction("inspect_recovery_incident", "recovery")
	if class == "budget" || class == "decision" {
		f.Blocker.NextAction = inspectAction("review_recovery_authority", "recovery")
	}
	return f
}

func (a *app) recoveryEvent(kind string, i *recoveryIncident) {
	if a.output != nil {
		a.output.emit(time.Now(), "recovery."+kind, "", fmt.Sprintf("Recovery incident %d: %s (%s, %s)", i.ID, i.Code, i.Phase, i.Status), activityWarning)
	}
}

func (a *app) reserveDiagnostic() error {
	i := a.activeIncident()
	if i == nil {
		return recoveryStop("identity", "integrity", "A diagnosis requires a retained incident")
	}
	f, err := a.recoveryFamily(i.Family)
	if err != nil {
		return err
	}
	r := a.state.Recovery
	if f.Diagnostics >= r.Policy.DiagnosticsPerFamily || r.Diagnostics >= r.Policy.DiagnosticsPerRun {
		return recoveryStop("diagnostics_exhausted", "budget", "Delegated diagnostic allowance is exhausted; counters and work are retained")
	}
	f.Diagnostics++
	r.Diagnostics++
	return nil // workerWithRecovery saves this reservation with the worker turn before launch.
}

func (a *app) newIncident(err error) error {
	r := a.state.Recovery
	if len(r.Incidents) >= 512 {
		return recoveryStop("incident_limit", "budget", "Recovery incident limit reached")
	}
	i := recoveryIncident{ID: len(r.Incidents) + 1, Code: "technical", Phase: "controller", Status: "open", Message: activityText(err.Error())}
	var condition *recoveryCondition
	var failure *executionFailure
	if errors.As(err, &condition) {
		i.Code, i.Phase, i.Request = condition.Code, condition.Phase, condition.Request
		if i.Request != nil && !validRecoveryRequest(i.Request) {
			i.Request = nil
			i.Code = "worker_protocol"
		}
	} else if errors.As(err, &failure) {
		if failure.Blocker.Component != "worker" || failure.Blocker.Class != "execution" {
			return err
		}
		i.Code = failure.Blocker.Code
		if validRecoveryPhase(r.Phase) {
			i.Phase = r.Phase
		}
	} else {
		return err // Unclassified errors are not permission to retry or alter a repository.
	}
	if a.state.Session == "" {
		return recoveryStop("session", "integrity", "Recovery has no retained worker conversation")
	}
	if !validRecoveryPhase(i.Phase) {
		return recoveryStop("phase", "integrity", "Recovery has an unknown continuation phase")
	}
	head, err := git(a.config.Repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	i.Candidate = head
	i.Instruction = r.Instruction
	if v, _, err := a.validationState(); err != nil {
		return err
	} else if v != nil {
		i.ValidationID = v.ID
	}
	i.Family = digest([]byte(i.Code + "\x00" + i.Phase))
	if _, err := a.recoveryFamily(i.Family); err != nil {
		return err
	}
	r.Incidents = append(r.Incidents, i)
	a.state.Status = "recovering"
	if err := a.save(); err != nil {
		return err
	}
	a.recoveryEvent("started", &r.Incidents[len(r.Incidents)-1])
	return nil
}

func (a *app) recoveryAdmission() error {
	if err := a.requireRepositoryOwnership(); err != nil {
		return err
	}
	if a.state.Pending != nil {
		return recoveryStop("pending_merge", "integrity", "Reconcile the canonical merge before any recovery action; never resubmit automatically")
	}
	if err := a.waitValidation(); err != nil {
		return err
	}
	if err := requireIdleWorker(a); err != nil {
		return err
	}
	if err := a.fetch(); err != nil {
		return err
	}
	return a.verifyIdentity()
}

// Only the existing controller calls this loop; it never launches resume or a second owner.
func (a *app) supervise(advance func() error) error {
	if a.state.Recovery == nil {
		return advance()
	}
	for {
		if _, err := a.quarantineOutputs(); err != nil {
			return err
		}
		if err := a.observeRecoveryValidation(); err != nil {
			return err
		}
		for _, family := range a.state.Recovery.Families {
			if family.FailedRepairs >= a.state.Recovery.Policy.FailedRepairsPerFamily {
				return recoveryStop("repairs_stalled", "technical", "Unresolved failure-family repair allowance remains exhausted")
			}
		}
		if i := a.activeIncident(); i != nil {
			if err := a.recoverIncident(); err != nil {
				return err
			}
		}
		err := advance()
		if err == nil {
			return nil
		}
		if a.state.Turns >= a.config.MaxTurns {
			return recoveryStop("turns_exhausted", "budget", "The original hard worker-turn allowance is exhausted")
		}
		// Owned output can prevent entry to the ordinary clean-checkout repair path.
		if changed, cleanupErr := a.quarantineOutputs(); cleanupErr != nil {
			return cleanupErr
		} else if changed {
			continue
		}
		if err := a.recoveryAdmission(); err != nil {
			return err
		}
		if err := a.newIncident(err); err != nil {
			return err
		}
	}
}

func (a *app) recoverIncident() (result error) {
	i := a.activeIncident()
	defer func() {
		if result != nil {
			i.Status = "blocked"
			i.Message = activityText(result.Error())
			var f *executionFailure
			if errors.As(result, &f) && (f.Blocker.Class == "decision" || f.Blocker.Class == "budget") {
				i.Status = "needs_decision"
			}
			result = errors.Join(result, a.save())
			a.recoveryEvent("blocked", i)
		}
	}()
	if err := a.recoveryAdmission(); err != nil {
		return err
	}
	if i.Status == "needs_decision" {
		approved := i.Request != nil && i.Request.Kind == "paths" && len(i.Request.Paths) != 0
		if approved {
			for _, path := range i.Request.Paths {
				approved = approved && a.repairPathAuthorized(path)
			}
		}
		if !approved {
			return recoveryStop("decision_required", "decision", i.Message)
		}
	}
	if i.Uncertain {
		return recoveryStop("interrupted", "integrity", "The retained diagnostic has an unresolved outcome; do not launch another action")
	}
	if i.Status == "reserved" && i.Action == "diagnose" {
		r := a.state.ProviderRetry
		if r == nil || r.Phase != "diagnose" || r.NextAt == nil {
			i.Uncertain = true
			return recoveryStop("interrupted", "integrity", "A diagnostic launch was interrupted; inspect its retained effects before further work")
		}
		if err := a.workerWithRecovery("diagnose", r.Instruction, r.Before); err != nil {
			return err
		}
		result := a.reply
		i.Result, i.Request, i.Status = &result, result.Recovery, "result"
		if !validRecoveryRequest(i.Request) || i.Request.IncidentID != i.ID || i.Request.ActionID != i.ActionID {
			i.Request = nil
		}
		if err := a.save(); err != nil {
			return err
		}
	}
	for {
		if i.Request != nil {
			resolved, err := a.applyRecoveryRequest(i)
			if err != nil {
				return err
			}
			if resolved {
				i.Status = "resolved"
				a.state.Status = "running"
				a.state.Feedback = i.Instruction
				if err := a.save(); err != nil {
					return err
				}
				a.recoveryEvent("resolved", i)
				return nil
			}
		}
		i.Action, i.Status = "diagnose", "reserved"
		i.ActionID++
		if err := a.save(); err != nil {
			return err
		}
		a.recoveryEvent("action", i)
		before, err := a.fingerprint()
		if err != nil {
			return err
		}
		task := fmt.Sprintf("Diagnose retained incident %d, action %d, code %s in phase %s at candidate %s. %s. Interrupted instruction: %s. Inspect actual effects before any change. Preserve approved assertions, resource limits, foreign work and every budget. Return recovery_requested with a structured request tied to incident_id %d and action_id %d; request decision only for genuinely missing authority. Do not merge, run the full gate, modify host privileges or clear counters.", i.ID, i.ActionID, i.Code, i.Phase, i.Candidate, i.Message, i.Instruction, i.ID, i.ActionID)
		if err := a.workerWithRecovery("diagnose", task, before); err != nil {
			return err
		}
		workerReply := a.reply
		i.Result = &workerReply
		i.Request, i.Status = a.reply.Recovery, "result"
		if !validRecoveryRequest(i.Request) || i.Request.IncidentID != i.ID || i.Request.ActionID != i.ActionID {
			i.Request = nil // A malformed response spends its reservation and cannot resolve an incident.
		}
		if err := a.save(); err != nil {
			return err
		}
	}
}

func (a *app) applyRecoveryRequest(i *recoveryIncident) (bool, error) {
	r := i.Request
	if r.Version != 1 || len(r.Paths) > 100 || r.Candidate != "" && r.Candidate != i.Candidate {
		return false, recoveryStop("request", "integrity", "Recovery request does not match its retained candidate")
	}
	switch r.Kind {
	case "decision":
		message := i.Message
		if i.Result != nil {
			message = activityText(i.Result.Message)
		}
		return false, recoveryStop("decision_required", "decision", message)
	case "paths":
		if len(r.Paths) == 0 {
			return false, recoveryStop("paths", "decision", "Exact additional file authority was not supplied")
		}
		if err := a.deriveRepairPaths(r.Paths); err != nil {
			return false, err
		}
		return true, nil
	case "quarantine":
		return a.quarantineOutputs()
	case "repair":
		if i.Phase == "close" && a.state.Gate != nil && a.finished() == nil {
			return true, nil
		}
		// The worker must supply fresh exact-candidate validation, not merely claim progress.
		v, active, err := a.validationState()
		if err != nil {
			return false, err
		}
		if active || v == nil || v.Status != "passed" || v.ID != r.ValidationID || v.ID == i.ValidationID {
			return false, nil
		}
		head, err := cleanHead(v.Worktree)
		if err != nil || head != v.Head {
			return false, recoveryStop("candidate", "integrity", "Recovery verification candidate changed")
		}
		var e evidence
		if err := readJSON(a.evidencePath(head), &e); err != nil || e.Head != head || e.Log != v.Log || !equalStrings(e.Commands, v.Commands) {
			return false, recoveryStop("evidence", "integrity", "Recovery requires exact-candidate ordered validation evidence")
		}
		if i.Phase == "prepare_gate" && v.Worktree == a.config.Repo && a.state.GateRepairPolicy != nil {
			required := a.state.GateRepairPolicy.RequiredCommands
			matches := len(v.Commands) >= len(required)
			for n, command := range required {
				if n >= len(v.Commands) || v.Commands[n] != command {
					matches = false
				}
			}
			remote, err := git(a.config.Repo, "rev-parse", "origin/dev")
			if err != nil {
				return false, err
			}
			if matches && remote == head {
				a.state.Stalled = 0
				return true, nil
			}
		}
		identity := a.config.Slices[min(a.state.Next, len(a.config.Slices)-1)]
		if repair := a.activeGateRepair(); repair != nil {
			identity = repair.Identity
		}
		p, err := a.discover(identity)
		if err != nil {
			return false, err
		}
		if p == nil || p.Merged || p.Head.SHA != head {
			return false, nil
		}
		parsed, err := a.checkDeliveryPR(identity, *p)
		if err != nil {
			return false, err
		}
		if !equalStrings(parsed.Commands, v.Commands) {
			return false, nil
		}
		// Ordinary work may continue after verified prerequisite repair, without refunding totals.
		a.state.Stalled = 0
		return true, nil
	case "wait":
		return false, recoveryStop("wait_unavailable", "technical", "No additional wait is delegated; native capacity and owned-validation waits retain their existing policies")
	default:
		return false, nil
	}
}

func recoveryMetadataPath(a *app) string { return filepath.Join(a.dir, "recovery") }
