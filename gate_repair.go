package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Gate repair authority is opt-in and independent of the frozen slice configuration.
// Paths constrain repository changes; the approved plan remains the semantic scope.
type gateRepairPolicy struct {
	BranchPrefix     string   `json:"branch_prefix"`
	Title            string   `json:"title"`
	ReportSlice      int      `json:"report_slice"`
	MaxRepairs       int      `json:"max_repairs"`
	AllowedPaths     []string `json:"allowed_paths"`
	RequiredCommands []string `json:"required_commands"`
}

// Each supplemental grant starts only after all repairs under prior grants merged.
// Its failed candidate anchors the first repair without rewriting earlier authority.
type gateRepairAuthorization struct {
	AfterRepairs int              `json:"after_repairs"`
	FailedHead   string           `json:"failed_head"`
	Policy       gateRepairPolicy `json:"policy"`
}

type gateFailure struct {
	Head    string `json:"head"`
	Message string `json:"message"`
}

type gateRepair struct {
	Identity slice        `json:"identity"`
	Base     string       `json:"base"`
	Failure  gateFailure  `json:"failure"`
	Merge    *mergeRecord `json:"merge,omitempty"`
}

func (a *app) validateGateRepairPolicy(p gateRepairPolicy) error {
	return a.validateGateRepairPolicyAt(p, 0)
}

func (a *app) validateGateRepairPolicyAt(p gateRepairPolicy, after int) error {
	if p.MaxRepairs < 1 || p.MaxRepairs > 100 || p.ReportSlice < 1 || p.ReportSlice > len(a.config.Slices) || len(p.AllowedPaths) == 0 || len(p.AllowedPaths) > 100 || len(p.RequiredCommands) == 0 || len(p.RequiredCommands) > 100 {
		return errors.New("gate repairs require a bounded policy, existing report slice, allowed paths and focused validation commands")
	}
	if !safePath(p.BranchPrefix) || p.BranchPrefix == "." || strings.ContainsAny(p.BranchPrefix, " ~^:?*[\\") || p.Title == "" || strings.TrimSpace(p.Title) != p.Title || strings.ContainsAny(p.Title, "\r\n") {
		return errors.New("invalid gate repair branch prefix or title")
	}
	for _, path := range p.AllowedPaths {
		if !safePath(path) || path == "." || path == ".git" || strings.HasPrefix(path, ".git/") {
			return fmt.Errorf("unsafe gate repair path %q", path)
		}
	}
	for _, cmd := range p.RequiredCommands {
		if strings.TrimSpace(cmd) != cmd || cmd == "" || cmd == a.config.FullGate || len(cmd) > 8192 || strings.ContainsAny(cmd, "\r\n`") {
			return errors.New("gate repair commands must be focused, nonempty single lines distinct from the full gate")
		}
	}
	for i := after + 1; i <= after+p.MaxRepairs; i++ {
		branch := fmt.Sprintf("%s-%d", p.BranchPrefix, i)
		if _, err := git(a.config.Repo, "check-ref-format", "--branch", branch); err != nil {
			return err
		}
		for _, s := range a.config.Slices {
			if s.Branch == branch {
				return errors.New("gate repair branch collides with a configured slice")
			}
		}
	}
	return nil
}

func (a *app) activeGateRepair() *gateRepair {
	if n := len(a.state.GateRepairs); n > 0 && a.state.GateRepairs[n-1].Merge == nil {
		return &a.state.GateRepairs[n-1]
	}
	return nil
}

func (a *app) pendingIdentity() (slice, error) {
	if a.state.Pending.GateRepair {
		if repair := a.activeGateRepair(); repair != nil && a.state.Next == len(a.config.Slices) {
			return repair.Identity, nil
		}
		return slice{}, errors.New("pending merge has no authorized gate repair")
	}
	if a.state.Next >= len(a.config.Slices) {
		return slice{}, errors.New("pending merge has no current slice")
	}
	return a.config.Slices[a.state.Next], nil
}

// Authorization preserves config identity, worker conversation, budgets and merge history.
// It is only accepted while this repository and run have no active owner.
func authorizeGateRepairs(path, policyPath, failedHead string) error {
	return authorizeGateRepairPolicy(path, policyPath, failedHead, false)
}

func authorizeAdditionalGateRepairs(path, policyPath, failedHead string) error {
	return authorizeGateRepairPolicy(path, policyPath, failedHead, true)
}

func authorizeGateRepairPolicy(path, policyPath, failedHead string, additional bool) error {
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
	if a.state.Next != len(a.config.Slices) || a.state.Gate != nil || a.state.Pending != nil {
		return errors.New("gate repair authorization requires integrated slices, no successful gate and no uncertain merge")
	}
	var policy gateRepairPolicy
	if err := readJSON(policyPath, &policy); err != nil {
		return err
	}
	if additional {
		return a.addGateRepairAuthorization(policy, failedHead)
	}
	if err := a.validateGateRepairPolicy(policy); err != nil {
		return err
	}
	if a.state.GateRepairPolicy != nil && !reflect.DeepEqual(*a.state.GateRepairPolicy, policy) {
		return errors.New("gate repair authority is already pinned; it cannot be replaced or widened")
	}
	if failedHead != "" {
		// Adopt a pre-upgrade failure only from its retained exact-candidate diagnostics.
		head, err := cleanHead(a.config.Repo)
		if err != nil {
			return err
		}
		logPath := filepath.Join(a.dir, "checks", failedHead+".log")
		if !fullSHA.MatchString(failedHead) || head != failedHead || a.state.Status != "blocked" || a.state.Blocker == nil || a.state.Blocker.Class != "validation" || !strings.Contains(a.state.Blocker.Message, logPath) {
			return errors.New("legacy gate failure does not match the blocked exact candidate")
		}
		b, err := os.ReadFile(logPath)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(b), "$ "+a.config.FullGate+"\n") {
			return errors.New("legacy diagnostics do not belong to the configured full gate")
		}
		if a.state.GateFailure != nil && a.state.GateFailure.Head != failedHead {
			return errors.New("cannot replace the retained failed candidate")
		}
		a.state.GateFailure = &gateFailure{Head: failedHead, Message: a.state.Blocker.Message}
	}
	a.state.GateRepairPolicy = &policy
	return a.save()
}

// Called only while holding the repository/run leases and after checking worker inactivity.
func (a *app) addGateRepairAuthorization(policy gateRepairPolicy, failedHead string) error {
	if a.state.GateRepairPolicy == nil || a.state.GateFailure == nil || !fullSHA.MatchString(failedHead) || a.state.GateFailure.Head != failedHead {
		return errors.New("additional repair authorization requires pinned authority and the retained --failed-head")
	}
	head, err := cleanHead(a.config.Repo)
	if err != nil {
		return err
	}
	if head != failedHead {
		return errors.New("additional repair authorization requires the exact clean failed candidate")
	}
	// A repeated approval cannot mint another allowance for the same candidate.
	for _, grant := range a.state.GateRepairAuthorizations {
		if grant.FailedHead == failedHead {
			if reflect.DeepEqual(grant.Policy, policy) {
				return nil
			}
			return errors.New("additional repair authority for this candidate is already pinned")
		}
	}
	limit := a.gateRepairLimit()
	if a.activeGateRepair() != nil || len(a.state.GateRepairs) != limit {
		return errors.New("additional repair authorization requires exhausted authority and all prior repairs integrated")
	}
	if err := a.validateGateRepairPolicyAt(policy, limit); err != nil {
		return err
	}
	if limit+policy.MaxRepairs > 100 {
		return errors.New("total final-gate repair authority cannot exceed 100 repairs")
	}
	for _, repair := range a.state.GateRepairs {
		if repair.Failure.Head == failedHead {
			return errors.New("the same failed gate candidate cannot authorize another repair")
		}
	}
	a.state.GateRepairAuthorizations = append(a.state.GateRepairAuthorizations, gateRepairAuthorization{AfterRepairs: limit, FailedHead: failedHead, Policy: policy})
	return a.save()
}

func (a *app) gateRepairLimit() int {
	if a.state.GateRepairPolicy == nil {
		return 0
	}
	limit := a.state.GateRepairPolicy.MaxRepairs
	for _, grant := range a.state.GateRepairAuthorizations {
		limit += grant.Policy.MaxRepairs
	}
	return limit
}

func (a *app) gateRepairPolicyFor(n int) *gateRepairPolicy {
	if p := a.state.GateRepairPolicy; p != nil && n > 0 && n <= p.MaxRepairs {
		return p
	}
	for i := range a.state.GateRepairAuthorizations {
		grant := &a.state.GateRepairAuthorizations[i]
		if n > grant.AfterRepairs && n <= grant.AfterRepairs+grant.Policy.MaxRepairs {
			return &grant.Policy
		}
	}
	return nil
}

func (a *app) prepareGateRepair() error {
	p := a.state.GateRepairPolicy
	if p == nil {
		return fmt.Errorf("full gate failed; repair authority is not delegated: %s", a.state.GateFailure.Message)
	}
	if a.activeGateRepair() != nil {
		return nil
	}
	if len(a.state.GateRepairs) >= a.gateRepairLimit() {
		if a.state.Recovery != nil {
			return recoveryStop("repair_limit", "budget", "Delegated final-gate repair limit reached; prior grants and budgets are retained")
		}
		return errors.New("authorized final-gate repair limit reached; checkpoint retained")
	}
	for _, r := range a.state.GateRepairs {
		if r.Failure.Head == a.state.GateFailure.Head {
			return errors.New("the same failed gate candidate cannot start another repair")
		}
	}
	n := len(a.state.GateRepairs) + 1
	base, err := cleanHead(a.config.Repo)
	if err != nil {
		return err
	}
	for _, grant := range a.state.GateRepairAuthorizations {
		if n == grant.AfterRepairs+1 && base != grant.FailedHead {
			return errors.New("primary candidate changed after additional repair authorization")
		}
	}
	a.state.GateRepairs = append(a.state.GateRepairs, gateRepair{Identity: a.gateRepairIdentity(n), Base: base, Failure: *a.state.GateFailure})
	a.state.Feedback = fmt.Sprintf("Final gate failed at %s: %s. Diagnose the retained failed log and correct only defects within the approved delivery set and gate repair allowed_paths. Preserve every gate requirement; run focused reproduction/regression checks, not the full gate. Use this pinned repair PR identity and the existing report. Do not add numbered slices or start a successor.", a.state.GateFailure.Head, a.state.GateFailure.Message)
	a.printf(activityRunning, "Final gate repair %d/%d: %s\n", n, a.gateRepairLimit(), a.state.GateFailure.Message)
	return a.save()
}

func (a *app) gateRepairIdentity(n int) slice {
	p := a.gateRepairPolicyFor(n)
	s := a.config.Slices[p.ReportSlice-1]
	s.Branch = fmt.Sprintf("%s-%d", p.BranchPrefix, n)
	s.Title = p.Title
	// Preserve identities already pinned by older controllers during recovery.
	if n > 0 && n <= len(a.state.GateRepairs) {
		legacy := fmt.Sprintf("%s (attempt %d)", p.Title, n)
		if a.state.GateRepairs[n-1].Identity.Title == legacy {
			s.Title = legacy
		}
	}
	s.RequiredCommands = append([]string(nil), p.RequiredCommands...)
	return s
}

func (a *app) verifyGateRepairState() error {
	if a.state.GateFailure != nil && !fullSHA.MatchString(a.state.GateFailure.Head) {
		return errors.New("invalid failed gate candidate")
	}
	if a.state.GateRepairPolicy == nil {
		if len(a.state.GateRepairAuthorizations) != 0 || len(a.state.GateRepairPaths) != 0 || len(a.state.GateRepairs) != 0 || a.state.Pending != nil && a.state.Pending.GateRepair {
			return errors.New("gate repair checkpoint has no pinned authority")
		}
		return nil
	}
	if err := a.validateGateRepairPolicy(*a.state.GateRepairPolicy); err != nil {
		return err
	}
	limit := a.state.GateRepairPolicy.MaxRepairs
	if len(a.state.GateRepairAuthorizations) > 99 {
		return errors.New("too many additional gate repair authorizations")
	}
	for _, grant := range a.state.GateRepairAuthorizations {
		if grant.AfterRepairs != limit || !fullSHA.MatchString(grant.FailedHead) || len(a.state.GateRepairs) < limit {
			return errors.New("invalid additional gate repair authorization position")
		}
		if err := a.validateGateRepairPolicyAt(grant.Policy, limit); err != nil {
			return err
		}
		if limit+grant.Policy.MaxRepairs > 100 {
			return errors.New("total final-gate repair authority cannot exceed 100 repairs")
		}
		for _, repair := range a.state.GateRepairs[:limit] {
			if repair.Merge == nil || repair.Failure.Head == grant.FailedHead {
				return errors.New("additional authority does not follow integrated repairs and a fresh failed candidate")
			}
		}
		if len(a.state.GateRepairs) == limit {
			if a.state.GateFailure == nil || a.state.GateFailure.Head != grant.FailedHead {
				return errors.New("unused additional authority differs from the retained failed candidate")
			}
		} else if a.state.GateRepairs[limit].Failure.Head != grant.FailedHead || a.state.GateRepairs[limit].Base != grant.FailedHead {
			return errors.New("additional repair differs from its authorized failed candidate")
		}
		limit += grant.Policy.MaxRepairs
	}
	upfront := a.state.Recovery != nil && a.state.Recovery.Policy.GateRepair != nil && len(a.state.GateRepairs) == 0 && len(a.state.GateRepairAuthorizations) == 0 && len(a.state.GateRepairPaths) == 0 && a.state.GateFailure == nil && a.state.Gate == nil && (a.state.Pending == nil || !a.state.Pending.GateRepair)
	if a.state.Next != len(a.config.Slices) && !upfront || len(a.state.GateRepairs) > limit {
		return errors.New("invalid gate repair checkpoint position")
	}
	for i, r := range a.state.GateRepairs {
		if !reflect.DeepEqual(r.Identity, a.gateRepairIdentity(i+1)) || !fullSHA.MatchString(r.Base) || !fullSHA.MatchString(r.Failure.Head) {
			return errors.New("gate repair identity differs from pinned authority")
		}
		if r.Merge == nil && (i != len(a.state.GateRepairs)-1 || a.state.GateFailure == nil || r.Failure.Head != a.state.GateFailure.Head) {
			return errors.New("inconsistent active gate repair")
		}
		if r.Merge != nil && (r.Merge.PR < 1 || !fullSHA.MatchString(r.Merge.Head) || !fullSHA.MatchString(r.Merge.MergedCommit)) {
			return errors.New("invalid gate repair merge record")
		}
	}
	if a.state.Gate != nil && (a.activeGateRepair() != nil || a.state.GateFailure != nil) {
		return errors.New("successful gate coexists with incomplete repair")
	}
	if a.state.Pending != nil && a.state.Pending.GateRepair && a.activeGateRepair() == nil {
		return errors.New("pending repair merge has no active repair")
	}
	return a.verifyGateRepairPathGrants()
}

func (a *app) checkDeliveryPR(s slice, p pullRequest) (parsedBody, error) {
	parsed, err := a.checkPR(s, p)
	if err != nil || a.activeGateRepair() == nil {
		return parsed, err
	}
	policy := a.gateRepairPolicyFor(len(a.state.GateRepairs))
	if policy == nil {
		return parsed, errors.New("gate repair has no delegated authority")
	}
	// Forgejo's base SHA can advance after merge; scope is checked from the pinned repair base.
	base := a.activeGateRepair().Base
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", base, p.Head.SHA); err != nil {
		return parsed, errors.New("repair candidate does not include its pinned base")
	}
	for _, grant := range a.state.GateRepairPaths {
		if grant.Repair == len(a.state.GateRepairs) {
			if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", grant.AuthorizedHead, p.Head.SHA); err != nil {
				return parsed, errors.New("repair candidate does not include its path authorization head")
			}
		}
	}
	paths, err := git(a.config.Repo, "diff", "--name-only", "--no-renames", "-z", base, p.Head.SHA)
	if err != nil {
		return parsed, err
	}
	if paths == "" {
		return parsed, errors.New("gate repair PR has no repository changes")
	}
	for _, path := range strings.Split(strings.TrimSuffix(paths, "\x00"), "\x00") {
		allowed := false
		for _, root := range policy.AllowedPaths {
			allowed = allowed || path == root || strings.HasPrefix(path, root+"/")
		}
		allowed = allowed || a.gateRepairPathGranted(len(a.state.GateRepairs), path)
		if !allowed {
			return parsed, fmt.Errorf("gate repair changes an unauthorized path: %s", path)
		}
	}
	return parsed, nil
}
