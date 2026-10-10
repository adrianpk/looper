package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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
		p, err := a.discover(s)
		if err != nil {
			return err
		}
		if p == nil {
			if err := a.work("implement", fmt.Sprintf("Implement Slice %d of %d and prepare its canonical PR. %s", s.Number, len(a.config.Slices), a.state.Feedback)); err != nil {
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
			if _, err := a.checkPR(s, *p); err != nil {
				return fmt.Errorf("already-merged PR fails the contract: %w", err)
			}
			if err := a.acceptMerge(*p); err != nil {
				return err
			}
			continue
		}
		if p.State != "open" {
			return fmt.Errorf("PR #%d closed without merge", p.Number)
		}
		parsed, err := a.checkPR(s, *p)
		if err != nil {
			if err := a.work("repair", fmt.Sprintf("Repair PR #%d without changing slice scope: %s", p.Number, err)); err != nil {
				return err
			}
			continue
		}
		if !p.Mergeable {
			if err := a.work("repair", fmt.Sprintf("PR #%d is not currently mergeable. Resolve the concrete integration condition within the approved slice; do not force-push without existing authorization.", p.Number)); err != nil {
				return err
			}
			continue
		}
		worktree, err := a.findWorktree(p.Head.SHA, s.Branch)
		if err != nil {
			if err := a.work("repair", err.Error()); err != nil {
				return err
			}
			continue
		}
		a.printf(activityRunning, "Slice %d/%d: checking PR #%d and exact-HEAD validation evidence.\n", s.Number, len(a.config.Slices), p.Number)
		if err := a.ensureValidation(worktree, parsed.Commands); err != nil {
			if err := a.work("repair", fmt.Sprintf("Required validation failed: %s. Correct only the active slice and regenerate its PR/evidence.", err)); err != nil {
				return err
			}
			continue
		}
		current, err := a.pull(p.Number)
		if err != nil {
			return err
		}
		if current.Title != p.Title || current.Body != p.Body || current.Head.SHA != p.Head.SHA || current.Base.SHA != p.Base.SHA || current.State != "open" || current.Merged {
			a.state.Feedback = "PR changed during verification; restart canonical checks"
			continue
		}
		if _, err := a.checkPR(s, current); err != nil {
			return err
		}
		if !current.Mergeable || current.Draft {
			if err := a.work("repair", "PR mergeability or draft state changed during verification; restore the approved handoff"); err != nil {
				return err
			}
			continue
		}
		if err := a.syncDev(); err != nil {
			if err := a.work("repair", err.Error()); err != nil {
				return err
			}
			continue
		}
		dev, err := git(a.config.Repo, "rev-parse", "origin/dev")
		if err != nil {
			return err
		}
		if dev != current.Base.SHA {
			if err := a.work("repair", "dev advanced during validation; refresh the PR and its exact-HEAD evidence before integration"); err != nil {
				return err
			}
			continue
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", dev, current.Head.SHA); err != nil {
			if err := a.work("repair", "The candidate does not include current dev. Integrate dev under the existing authorization and regenerate exact-HEAD checks. The controller will not merge an untested rebased tree."); err != nil {
				return err
			}
			continue
		}
		if err := a.merge(current); err != nil {
			return err
		}
	}
}

func (a *app) merge(p pullRequest) error {
	a.printf(activitySuccess, "PR ready: %s\n", p.HTMLURL)
	a.printf(activityRunning, "Merging Rebase + Fast-forward into dev.\n")
	a.state.Pending = &pendingMerge{PR: p.Number, Head: p.Head.SHA}
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
	if a.state.Next >= len(a.config.Slices) {
		return errors.New("pending merge has no current slice")
	}
	if _, err := a.checkPR(a.config.Slices[a.state.Next], p); err != nil {
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
	a.state.Merged = append(a.state.Merged, mergeRecord{PR: p.Number, Head: p.Head.SHA, MergedCommit: p.MergedCommit})
	a.state.Next++
	a.state.Pending = nil
	a.state.Feedback = fmt.Sprintf("PR #%d merged into dev. Close its report/tracker entries and activate the next recorded slice.", p.Number)
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
	a.printf(activitySuccess, "Slice %d/%d integrated in dev.\n", a.state.Next, len(a.config.Slices))
	return nil
}

func (a *app) recordMerges() error {
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
	head, err := cleanHead(dir)
	if err != nil {
		return err
	}
	var e evidence
	if err := readJSON(a.evidencePath(head), &e); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
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

func (a *app) validate(dir string, commands []string) error {
	root, err := git(dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(dir, root)
	}
	canonical, err := git(a.config.Repo, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(canonical) {
		canonical = filepath.Join(a.config.Repo, canonical)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	canonical, err = filepath.EvalSymlinks(canonical)
	if err != nil {
		return err
	}
	if root != canonical {
		return errors.New("validation checkout belongs to another repository")
	}
	head, err := cleanHead(dir)
	if err != nil {
		return err
	}
	if len(commands) == 0 || len(commands) > 100 {
		return errors.New("expected 1 to 100 validation commands")
	}
	if err := os.MkdirAll(a.dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(a.dir, "validate.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another validation process owns this run")
	}
	logPath := filepath.Join(a.dir, "checks", head+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0700); err != nil {
		return err
	}
	var log capture
	log.limit = 1 << 20
	if a.activity != nil {
		a.activity.begin("Controller validation", "validation.started")
		defer a.activity.end()
	}
	for i, cmd := range commands {
		if strings.TrimSpace(cmd) == "" || len(cmd) > 8192 || strings.ContainsAny(cmd, "\r\n") {
			return errors.New("invalid validation command")
		}
		a.printf(activityRunning, "Check %d/%d: %s\n", i+1, len(commands), cmd)
		if a.activity != nil {
			a.activity.check(i+1, len(commands), cmd)
		}
		_, _ = fmt.Fprintf(&log, "$ %s\n", cmd)
		child, err := executionCommand(context.Background(), dir, "sh", "-ec", cmd)
		if err != nil {
			return err
		}
		child.Stdout = &log
		child.Stderr = &log
		if err := child.Start(); err != nil {
			return err
		}
		validation, metadataErr := identifyProcess(child.Process.Pid)
		if metadataErr == nil {
			metadataErr = writeJSON(filepath.Join(a.dir, "validation.json"), validation)
		}
		waitErr := child.Wait()
		if metadataErr != nil && !errors.Is(metadataErr, os.ErrNotExist) {
			return errors.Join(metadataErr, waitErr)
		}
		if err := waitErr; err != nil {
			if saveErr := writeAtomic(logPath, log.b.Bytes()); saveErr != nil {
				return errors.Join(err, saveErr)
			}
			return contextFailure("validation", "validation", fmt.Sprintf("Check %d failed; diagnostics: %s", i+1, logPath), 1)
		}
		after, err := cleanHead(dir)
		if err != nil {
			return err
		}
		if after != head {
			return errors.New("HEAD changed during validation; evidence discarded")
		}
	}
	if log.truncated {
		_, _ = log.b.WriteString("\n[diagnostics truncated at 1 MiB]\n")
	}
	if err := writeAtomic(logPath, log.b.Bytes()); err != nil {
		return err
	}
	return writeJSON(a.evidencePath(head), evidence{Head: head, Commands: commands, Log: logPath})
}

func (a *app) closeSet() error {
	if err := a.syncDev(); err != nil {
		return err
	}
	if a.state.Gate == nil {
		for {
			if err := a.work("prepare_gate", "All recorded slices are integrated. Close delivered slice entries, prepare the approved full-gate prerequisites, leave primary dev clean and pushed, and return ready_for_gate. Do not run the full gate: the controller owns it."); err != nil {
				return err
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
		a.printf(activityRunning, "All slices integrated. Running the configured full gate on clean dev %s\n", head)
		if err := a.validate(a.config.Repo, []string{a.config.FullGate}); err != nil {
			return fmt.Errorf("full gate failed; do not close the set: %w", err)
		}
		var e evidence
		if err := readJSON(a.evidencePath(head), &e); err != nil {
			return err
		}
		a.state.Gate = &e
		if err := a.save(); err != nil {
			return err
		}
	}
	for {
		if err := a.finished(); err == nil {
			a.state.Status = "complete"
			a.state.Feedback = ""
			if err := a.save(); err != nil {
				return err
			}
			a.printf(activitySuccess, "Slice set completed: %d/%d integrated in dev; full gate passed at %s.\n", len(a.config.Slices), len(a.config.Slices), a.state.Gate.Head)
			return nil
		}
		if err := a.work("close", fmt.Sprintf("Full gate passed at immutable candidate %s. Evidence: %s. Stop only owned test fixtures after validation. Close reports, plan and tracker, update the local tree/handoff when applicable, record activity, commit and push documentation closure to dev. Only the configured closure paths may differ from the tested candidate. Return closed when done.", a.state.Gate.Head, a.evidencePath(a.state.Gate.Head))); err != nil {
			return err
		}
		if err := a.fetch(); err != nil {
			return err
		}
		if err := a.syncDev(); err != nil {
			return err
		}
		if err := a.verifyClosureDiff(); err != nil {
			return err
		}
	}
}

func (a *app) verifyClosureDiff() error {
	if a.state.Gate == nil {
		return errors.New("no full-gate evidence")
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", a.state.Gate.Head, "origin/dev"); err != nil {
		return errors.New("validated candidate is not an ancestor of current dev")
	}
	paths, err := git(a.config.Repo, "diff", "--name-only", a.state.Gate.Head, "origin/dev")
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	for _, p := range a.config.ClosurePaths {
		allowed[p] = true
	}
	for _, p := range strings.Split(paths, "\n") {
		if p != "" && !allowed[p] {
			return fmt.Errorf("runtime or unapproved artifact changed after full gate: %s", p)
		}
	}
	return nil
}

func (a *app) finished() error {
	if a.state.Next != len(a.config.Slices) || a.state.Gate == nil {
		return errors.New("slices or full gate are incomplete")
	}
	if err := a.syncDev(); err != nil {
		return err
	}
	if err := a.verifyClosureDiff(); err != nil {
		return err
	}
	for i, r := range a.state.Merged {
		p, err := a.pull(r.PR)
		if err != nil {
			return err
		}
		if !p.Merged || p.MergedCommit != r.MergedCommit {
			return errors.New("recorded merge no longer matches canonical state")
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", r.MergedCommit, "origin/dev"); err != nil {
			return err
		}
		report, err := git(a.config.Repo, "show", "origin/dev:"+a.config.Slices[i].Report)
		if err != nil {
			return err
		}
		if !hasStatus(report, "delivered") {
			return errors.New("slice report is not delivered")
		}
	}
	for _, path := range []string{a.config.Plan, a.config.Tracker} {
		doc, err := git(a.config.Repo, "show", "origin/dev:"+path)
		if err != nil {
			return err
		}
		if !hasStatus(doc, "completed") || strings.Contains(doc, "- [ ]") {
			return errors.New("plan/tracker completion metadata or gates are incomplete")
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
	p, err := a.discover(a.config.Slices[min(a.state.Next, len(a.config.Slices)-1)])
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
