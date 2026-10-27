package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *app) fetch() error {
	_, err := accessCommand("git", a.config.Repo, "git", "fetch", "origin", "refs/heads/dev:refs/remotes/origin/dev")
	return err
}

func (a *app) syncDev() error {
	branch, err := git(a.config.Repo, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch != "dev" {
		return errors.New("primary checkout must remain on dev; slice work belongs in its dedicated worktree")
	}
	if _, err := cleanHead(a.config.Repo); err != nil {
		return err
	}
	_, err = git(a.config.Repo, "merge", "--ff-only", "origin/dev")
	if err != nil {
		return err
	}
	head, err := git(a.config.Repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	remote, err := git(a.config.Repo, "rev-parse", "origin/dev")
	if err != nil {
		return err
	}
	if head != remote {
		return errors.New("primary dev has unpushed commits")
	}
	return nil
}

func (a *app) run() error {
	if err := a.waitValidation(); err != nil {
		return err
	}
	if err := a.fetch(); err != nil {
		return err
	}
	if err := a.verifyIdentity(); err != nil {
		return err
	}
	if a.state.Status == "complete" {
		if err := a.finished(); err != nil {
			return err
		}
		a.printf(activitySuccess, "Slice set already completed and verified.\n")
		return nil
	}
	a.state.Status = "running"
	if err := a.save(); err != nil {
		return err
	}
	for {
		if err := a.waitValidation(); err != nil {
			return err
		}
		if err := a.fetch(); err != nil {
			return err
		}
		if err := a.verifyIdentity(); err != nil {
			return err
		}
		if a.state.Pending != nil {
			if err := a.reconcile(); err != nil {
				return err
			}
			continue
		}
		if err := a.recordMerges(); err != nil {
			return err
		}
		if a.state.Next == len(a.config.Slices) {
			return a.closeSet()
		}
		s := a.config.Slices[a.state.Next]
		if err := a.deliverPR(s, "implement", "repair"); err != nil {
			return err
		}
	}
}

// Both slice delivery and authorized gate repairs use the same exact-HEAD merge path.
func (a *app) deliverPR(s slice, implementPhase, repairPhase string) error {
	for {
		if err := a.fetch(); err != nil {
			return err
		}
		if err := a.verifyIdentity(); err != nil {
			return err
		}
		p, err := a.discover(s)
		if err != nil {
			return err
		}
		if p == nil {
			if err := a.work(implementPhase, fmt.Sprintf("Prepare canonical PR %q on branch %s, using the existing Slice %d report %s. %s", s.Title, s.Branch, s.Number, s.Report, a.state.Feedback)); err != nil {
				return err
			}
			continue
		}
		pinned, err := a.pull(p.Number)
		if err != nil {
			return err
		}
		p = &pinned
		if p.Merged {
			if _, err := a.checkDeliveryPR(s, *p); err != nil {
				return fmt.Errorf("already-merged PR fails the contract: %w", err)
			}
			return a.acceptMerge(*p)
		}
		if p.State != "open" {
			return fmt.Errorf("PR #%d closed without merge", p.Number)
		}
		parsed, err := a.checkDeliveryPR(s, *p)
		if err != nil {
			if err := a.work(repairPhase, fmt.Sprintf("Repair PR #%d without changing approved scope: %s", p.Number, err)); err != nil {
				return err
			}
			continue
		}
		if !p.Mergeable {
			if err := a.work(repairPhase, fmt.Sprintf("PR #%d is not currently mergeable. Resolve the concrete integration condition within the approved scope; do not force-push without existing authorization.", p.Number)); err != nil {
				return err
			}
			continue
		}
		worktree, err := a.findWorktree(p.Head.SHA, s.Branch)
		if err != nil {
			if err := a.work(repairPhase, err.Error()); err != nil {
				return err
			}
			continue
		}
		if a.activeGateRepair() != nil {
			a.printf(activityRunning, "Final-gate repair: checking PR #%d and exact-HEAD validation evidence.\n", p.Number)
		} else {
			a.printf(activityRunning, "Slice %d/%d: checking PR #%d and exact-HEAD validation evidence.\n", s.Number, len(a.config.Slices), p.Number)
		}
		if err := a.ensureValidation(worktree, parsed.Commands); err != nil {
			var failure *executionFailure
			if errors.As(err, &failure) && failure.Blocker.Class != "validation" {
				return err
			}
			a.printf(activityFailure, "Required validation failed: %s\n", err)
			if err := a.work(repairPhase, fmt.Sprintf("Required validation failed: %s. Correct only the active approved unit and regenerate its PR/evidence.", err)); err != nil {
				return err
			}
			continue
		}
		integrated, repair, err := a.verifyAndMerge(s, *p, worktree, parsed.Commands)
		if err != nil {
			return err
		}
		if integrated {
			return nil
		}
		if repair != "" {
			if err := a.work(repairPhase, repair); err != nil {
				return err
			}
		}
	}
}

// Keep admission closed to new validators from the last candidate checks through
// merge reconciliation; release it before any repair worker can launch validation.
func (a *app) verifyAndMerge(s slice, p pullRequest, worktree string, commands []string) (bool, string, error) {
	guard, err := a.guardValidation()
	if err != nil {
		return false, "", err
	}
	defer guard.close()
	head, err := cleanHead(worktree)
	var e evidence
	if err != nil || head != p.Head.SHA || readJSON(a.evidencePath(head), &e) != nil || e.Head != head || !equalStrings(e.Commands, commands) {
		a.state.Feedback = "Candidate or evidence changed after validation; repeat canonical verification"
		return false, "", nil
	}
	v, _, err := a.validationState()
	if err != nil {
		return false, "", err
	}
	if v != nil && v.Status == "failed" && v.Head == head && equalStrings(v.Commands, commands) {
		return false, "", nil
	}
	current, err := a.pull(p.Number)
	if err != nil {
		return false, "", err
	}
	if current.Title != p.Title || current.Body != p.Body || current.Head.SHA != p.Head.SHA || current.Base.SHA != p.Base.SHA || current.State != "open" || current.Merged {
		a.state.Feedback = "PR changed during verification; restart canonical checks"
		return false, "", nil
	}
	if _, err := a.checkDeliveryPR(s, current); err != nil {
		return false, "", err
	}
	if !current.Mergeable || current.Draft {
		return false, "PR mergeability or draft state changed during verification; restore the approved handoff", nil
	}
	if err := a.verifyDocuments(current.Head.SHA); err != nil {
		return false, "", fmt.Errorf("candidate documents violate the frozen delivery identity: %w", err)
	}
	if err := a.fetch(); err != nil {
		return false, "", err
	}
	if err := a.syncDev(); err != nil {
		return false, err.Error(), nil
	}
	dev, err := git(a.config.Repo, "rev-parse", "origin/dev")
	if err != nil {
		return false, "", err
	}
	if dev != current.Base.SHA {
		return false, "dev advanced during validation; refresh the PR and its exact-HEAD evidence before integration", nil
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", dev, current.Head.SHA); err != nil {
		return false, "The candidate does not include current dev. Integrate dev under the existing authorization and regenerate exact-HEAD checks. The controller will not merge an untested rebased tree.", nil
	}
	return true, "", a.merge(current)
}

func (a *app) merge(p pullRequest) error {
	a.printf(activitySuccess, "PR ready: %s\n", p.HTMLURL)
	a.printf(activityRunning, "Merging Rebase + Fast-forward into dev.\n")
	a.state.Pending = &pendingMerge{PR: p.Number, Head: p.Head.SHA, GateRepair: a.activeGateRepair() != nil}
	if err := a.save(); err != nil {
		return err
	}
	body := map[string]any{"Do": "rebase", "head_commit_id": p.Head.SHA, "force_merge": false, "delete_branch_after_merge": false, "merge_when_checks_succeed": false}
	err := a.api("POST", a.endpoint(fmt.Sprintf("/pulls/%d/merge", p.Number)), body, nil)
	// Never repeat a mutation whose outcome is uncertain. Reconcile first, including after restart.
	if confirmErr := a.reconcile(); confirmErr != nil {
		if err != nil {
			return errors.Join(err, confirmErr)
		}
		return confirmErr
	}
	return nil
}

func (a *app) reconcile() error {
	p, err := a.pull(a.state.Pending.PR)
	if err != nil {
		return fmt.Errorf("pending merge outcome is unknown; no resubmission: %w", err)
	}
	if !p.Merged {
		return errors.New("pending merge is not confirmed; inspect the remote PR before 'recover --retry-unmerged'; no automatic resubmission")
	}
	if p.Head.SHA != a.state.Pending.Head {
		return errors.New("merged PR head differs from the submitted exact head")
	}
	identity, err := a.pendingIdentity()
	if err != nil {
		return err
	}
	if _, err := a.checkDeliveryPR(identity, p); err != nil {
		return err
	}
	return a.acceptMerge(p)
}

func (a *app) acceptMerge(p pullRequest) error {
	if !fullSHA.MatchString(p.MergedCommit) {
		return errors.New("merged PR has no full merge commit")
	}
	if err := a.fetch(); err != nil {
		return err
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", p.MergedCommit, "origin/dev"); err != nil {
		return errors.New("canonical merged commit is not integrated in origin/dev")
	}
	record := mergeRecord{PR: p.Number, Head: p.Head.SHA, MergedCommit: p.MergedCommit}
	isRepair := a.activeGateRepair() != nil
	if repair := a.activeGateRepair(); repair != nil {
		repair.Merge = &record
		a.state.GateFailure = nil
	} else {
		a.state.Merged = append(a.state.Merged, record)
		a.state.Next++
	}
	a.state.Pending = nil
	a.state.Feedback = fmt.Sprintf("PR #%d merged into dev. Close its report/tracker entries and activate the next recorded slice.", p.Number)
	if isRepair {
		a.state.Feedback = fmt.Sprintf("Final-gate repair PR #%d integrated. Prepare the corrected composition for the unchanged full gate; do not start another delivery set.", p.Number)
	}
	a.state.Stalled = 0
	if err := a.save(); err != nil {
		return err
	}
	if err := a.recordMerges(); err != nil {
		return err
	}
	if err := a.syncDev(); err != nil {
		return err
	}
	if isRepair {
		a.printf(activitySuccess, "Final-gate repair PR #%d integrated in dev.\n", p.Number)
	} else {
		a.printf(activitySuccess, "Slice %d/%d integrated in dev.\n", a.state.Next, len(a.config.Slices))
	}
	return nil
}

func (a *app) recordMerges() error {
	for i := range a.state.GateRepairs {
		r := a.state.GateRepairs[i].Merge
		if r == nil || r.Recorded {
			continue
		}
		if _, err := command(a.config.Repo, "acta", "record", "pull-request", "--forge", a.config.Forge, "--repository", a.config.Repository, "--number", fmt.Sprint(r.PR), "--action", "merged", "--source", "integration", fmt.Sprintf("Authorized final-gate repair %d merged into dev at %s.", i+1, r.MergedCommit)); err != nil {
			return fmt.Errorf("repair merge confirmed; activity recording failed: %w", err)
		}
		r.Recorded = true
		if err := a.save(); err != nil {
			return err
		}
	}
	for i := range a.state.Merged {
		r := &a.state.Merged[i]
		if r.Recorded {
			continue
		}
		_, err := command(a.config.Repo, "acta", "record", "pull-request", "--forge", a.config.Forge, "--repository", a.config.Repository, "--number", fmt.Sprint(r.PR), "--action", "merged", "--source", "integration", fmt.Sprintf("Slice %d merged through Rebase + Fast-forward into dev at %s.", i+1, r.MergedCommit))
		if err != nil {
			return fmt.Errorf("merge confirmed; activity recording failed: %w", err)
		}
		r.Recorded = true
		if err := a.save(); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) findWorktree(head, branch string) (string, error) {
	out, err := git(a.config.Repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, block := range strings.Split(out, "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, " ")
			fields[key] = value
		}
		if fields["HEAD"] == head && fields["branch"] == "refs/heads/"+branch {
			if _, err := cleanHead(fields["worktree"]); err != nil {
				return "", err
			}
			return fields["worktree"], nil
		}
	}
	return "", errors.New("no clean local slice worktree matches the canonical PR head and branch")
}

func (a *app) evidencePath(head string) string { return filepath.Join(a.dir, "checks", head+".json") }

func (a *app) ensureValidation(dir string, commands []string) error {
	if err := a.waitValidation(); err != nil {
		return err
	}
	head, err := cleanHead(dir)
	if err != nil {
		return err
	}
	v, _, err := a.validationState()
	if err != nil {
		return err
	}
	if v != nil && v.Status == "failed" && v.Head == head && equalStrings(v.Commands, commands) && v.ID != a.state.ValidationHandled {
		return &executionFailure{Blocker: *v.Failure}
	}
	if v != nil && v.Status == "failed" && v.Head == head && equalStrings(v.Commands, commands) {
		return a.validate(dir, commands)
	}
	var e evidence
	if err := readJSON(a.evidencePath(head), &e); err != nil && !errors.Is(err, os.ErrNotExist) {
		return validationFailure("outcome_inconsistent", "integrity", "Validation evidence is unreadable; inspect "+a.evidencePath(head))
	}
	if v != nil && v.Status == "passed" && v.Head == head && equalStrings(v.Commands, commands) && (e.Head != head || !equalStrings(e.Commands, commands) || e.Log != v.Log) {
		return validationFailure("outcome_inconsistent", "integrity", "Validation passed without matching candidate, commands and batch evidence; inspect "+a.evidencePath(head))
	}
	if e.Head == head && equalStrings(e.Commands, commands) {
		a.printf(activitySuccess, "Reusing successful checks for the unchanged exact HEAD.\n")
		return nil
	}
	return a.validate(dir, commands)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type closureMetadataError struct {
	Artifact, Condition, Expected string
}

func (e *closureMetadataError) Error() string {
	return fmt.Sprintf("%s: %s; expected %s", e.Artifact, e.Condition, e.Expected)
}

func (a *app) closureIntegrity(reason string) error {
	failure := contextFailure("closure", "integrity", reason, 1)
	failure.Blocker.NextAction = inspectAction("inspect_closure_integrity", a.config.Repo)
	return failure
}

// Keep typed access failures and their retry counts; local verification failures require inspection.
func (a *app) closureFailure(reason string, err error) error {
	var known *executionFailure
	if errors.As(err, &known) {
		return err
	}
	failure := classifyAccess("closure", err, 1)
	if failure.Blocker.Class != "access" {
		return failure
	}
	return a.closureIntegrity(reason)
}

func (a *app) closeSet() error {
	if err := a.syncDev(); err != nil {
		return a.closureFailure("Primary dev must be clean, pushed and synchronized before closure", err)
	}
	if err := a.reconcileGateRetry(); err != nil {
		return err
	}
	// Runs without delegated repair authority retain their explicit manual retry workflow.
	if a.state.GateFailure != nil && a.state.GateRepairPolicy == nil {
		if len(a.state.GateRetries) != 0 && a.pendingGateRetry() == nil {
			return errors.New("authorized gate retry failed; inspect diagnostics and authorize another retry before resuming")
		}
		a.state.GateFailure = nil
		if err := a.save(); err != nil {
			return err
		}
	}
	for a.state.Gate == nil {
		if a.state.GateFailure != nil {
			if err := a.prepareGateRepair(); err != nil {
				return err
			}
			if err := a.deliverPR(a.activeGateRepair().Identity, "repair_gate", "repair_gate"); err != nil {
				return err
			}
			continue
		}
		for {
			// A resumed capacity retry may already have finished this preparation turn.
			if a.replyPhase != "prepare_gate" || a.reply.Status != "ready_for_gate" {
				if err := a.work("prepare_gate", "All recorded slices are integrated. Close delivered slice entries, prepare the approved full-gate prerequisites, leave primary dev clean and pushed, and return ready_for_gate. Do not run the full gate: the controller owns it."); err != nil {
					return err
				}
			}
			if a.reply.Status == "ready_for_gate" {
				break
			}
		}
		if err := a.fetch(); err != nil {
			return err
		}
		if err := a.syncDev(); err != nil {
			return err
		}
		head, err := cleanHead(a.config.Repo)
		if err != nil {
			return err
		}
		if retry := a.pendingGateRetry(); retry != nil && head != retry.Failure.Head {
			return errors.New("candidate changed after gate retry authorization")
		}
		a.printf(activityRunning, "All slices integrated. Running the configured full gate on clean dev %s\n", head)
		if err := a.validate(a.config.Repo, []string{a.config.FullGate}); err != nil {
			var failure *executionFailure
			if !errors.As(err, &failure) || failure.Blocker.Class != "validation" {
				return err
			}
			if err := a.finishGateRetry("failed"); err != nil {
				return err
			}
			a.state.GateFailure = &gateFailure{Head: head, Message: err.Error()}
			if saveErr := a.save(); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			if a.state.GateRepairPolicy == nil {
				return fmt.Errorf("full gate failed; do not close the set: %w", err)
			}
			continue
		}
		var e evidence
		if err := readJSON(a.evidencePath(head), &e); err != nil {
			return err
		}
		if err := a.finishGateRetry("passed"); err != nil {
			return err
		}
		a.state.Gate = &e
		if err := a.save(); err != nil {
			return err
		}
	}
	for {
		err := a.finished()
		if err == nil {
			a.state.Status = "complete"
			a.state.Feedback = ""
			if err := a.save(); err != nil {
				return err
			}
			a.printf(activitySuccess, "Slice set completed: %d/%d integrated in dev; full gate passed at %s.\n", len(a.config.Slices), len(a.config.Slices), a.state.Gate.Head)
			return nil
		}
		var metadata *closureMetadataError
		if !errors.As(err, &metadata) {
			return err
		}
		allowed := false
		for _, path := range a.config.ClosurePaths {
			allowed = allowed || path == metadata.Artifact
		}
		if !allowed {
			return a.closureIntegrity(fmt.Sprintf("%s; the artifact is outside approved closure_paths", metadata))
		}
		if err := a.work("close", fmt.Sprintf("Closure verification failed: %s. Full gate passed at immutable candidate %s. Evidence: %s. Stop only owned test fixtures after validation. Close reports, plan and tracker, update the local tree/handoff when applicable, record activity, commit and push documentation closure to dev. Only the configured closure paths may differ from the tested candidate. Return closed when done.", metadata, a.state.Gate.Head, a.evidencePath(a.state.Gate.Head))); err != nil {
			return err
		}
		if err := a.fetch(); err != nil {
			return err
		}
		if err := a.syncDev(); err != nil {
			return a.closureFailure("Primary dev must be clean, pushed and synchronized after closure work", err)
		}
		if err := a.verifyClosureDiff(); err != nil {
			return err
		}
	}
}

func (a *app) verifyClosureDiff() error {
	if a.state.Gate == nil {
		return a.closureIntegrity("no full-gate evidence")
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", a.state.Gate.Head, "origin/dev"); err != nil {
		return a.closureFailure("validated candidate is not an ancestor of current dev", err)
	}
	paths, err := git(a.config.Repo, "diff", "--name-only", a.state.Gate.Head, "origin/dev")
	if err != nil {
		return a.closureFailure("Cannot verify changed paths against the full-gate candidate", err)
	}
	allowed := map[string]bool{}
	for _, p := range a.config.ClosurePaths {
		allowed[p] = true
	}
	for _, p := range strings.Split(paths, "\n") {
		if p != "" && !allowed[p] {
			return a.closureIntegrity(fmt.Sprintf("runtime or unapproved artifact changed after full gate: %s", p))
		}
	}
	return nil
}

func (a *app) finished() error {
	if a.state.Next != len(a.config.Slices) || a.state.Gate == nil {
		return a.closureIntegrity("slices or full gate are incomplete")
	}
	if err := a.syncDev(); err != nil {
		return a.closureFailure("Primary dev must be clean, pushed and synchronized for closure verification", err)
	}
	if err := a.verifyClosureDiff(); err != nil {
		return err
	}
	for _, repair := range a.state.GateRepairs {
		if repair.Merge == nil {
			return a.closureIntegrity("gate repair is not integrated")
		}
		r := repair.Merge
		p, err := a.pull(r.PR)
		if err != nil {
			return a.closureFailure(fmt.Sprintf("Cannot verify canonical gate-repair PR #%d", r.PR), err)
		}
		if !p.Merged || p.Head.SHA != r.Head || p.MergedCommit != r.MergedCommit {
			return a.closureIntegrity(fmt.Sprintf("gate repair merge for PR #%d no longer matches canonical state", r.PR))
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", r.MergedCommit, "origin/dev"); err != nil {
			return a.closureFailure(fmt.Sprintf("Gate-repair PR #%d is not integrated in origin/dev", r.PR), err)
		}
	}
	for i, r := range a.state.Merged {
		p, err := a.pull(r.PR)
		if err != nil {
			return a.closureFailure(fmt.Sprintf("Cannot verify canonical PR #%d", r.PR), err)
		}
		if !p.Merged || p.MergedCommit != r.MergedCommit {
			return a.closureIntegrity(fmt.Sprintf("recorded merge for PR #%d no longer matches canonical state", r.PR))
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", r.MergedCommit, "origin/dev"); err != nil {
			return a.closureFailure(fmt.Sprintf("Recorded PR #%d is not integrated in origin/dev", r.PR), err)
		}
		report, err := git(a.config.Repo, "show", "origin/dev:"+a.config.Slices[i].Report)
		if err != nil {
			return a.closureFailure("Cannot read closure artifact "+a.config.Slices[i].Report+" from origin/dev", err)
		}
		if !hasStatus(report, "delivered") {
			return &closureMetadataError{a.config.Slices[i].Report, "Status is not Delivered", "Status: Delivered"}
		}
	}
	for _, path := range []string{a.config.Plan, a.config.Tracker} {
		doc, err := git(a.config.Repo, "show", "origin/dev:"+path)
		if err != nil {
			return a.closureFailure("Cannot read closure artifact "+path+" from origin/dev", err)
		}
		if !hasStatus(doc, "completed") {
			return &closureMetadataError{path, "Status is not Completed", "Status: Completed"}
		}
		if strings.Contains(doc, "- [ ]") {
			return &closureMetadataError{path, "unchecked tasks or gates remain", "no unchecked tasks or gates after verifying their completion"}
		}
	}
	return nil
}

func hasStatus(doc, status string) bool {
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "Status:") {
			return strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(line, "Status:")), status)
		}
	}
	return false
}

func (a *app) fingerprint() (string, error) {
	identity := a.config.Slices[min(a.state.Next, len(a.config.Slices)-1)]
	if repair := a.activeGateRepair(); repair != nil {
		identity = repair.Identity
	}
	p, err := a.discover(identity)
	if err != nil {
		return "", err
	}
	dev, err := git(a.config.Repo, "rev-parse", "origin/dev")
	if err != nil {
		return "", err
	}
	worktrees, err := git(a.config.Repo, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	progress := worktrees
	for _, line := range strings.Split(worktrees, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		path := strings.TrimPrefix(line, "worktree ")
		status, err := git(path, "status", "--porcelain")
		if err != nil {
			return "", err
		}
		diff, err := git(path, "diff", "HEAD")
		if err != nil {
			return "", err
		}
		progress += status + digest([]byte(diff))
	}
	// A new scoped approval resolves a real worker blocker without resetting counters.
	if len(a.state.GateRepairPaths) != 0 {
		grants, err := json.Marshal(a.state.GateRepairPaths)
		if err != nil {
			return "", err
		}
		progress += string(grants)
	}
	b, err := json.Marshal(struct {
		PR        *pullRequest
		Dev       string
		Next      int
		Worktrees string
	}{p, dev, a.state.Next, digest([]byte(progress))})
	if err != nil {
		return "", err
	}
	return digest(b), nil
}
