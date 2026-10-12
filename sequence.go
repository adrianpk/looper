package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type configRef struct {
	Path string `json:"config"`
	Hash string `json:"config_hash"`
}

type closureReceipt struct {
	ConfigRef configRef `json:"set"`
	Head      string    `json:"closure_commit"`
}

type deliverySequence struct {
	Version  int              `json:"version"`
	Sets     []configRef      `json:"sets"`
	Current  int              `json:"current_index"`
	Phase    string           `json:"phase"`
	Receipts []closureReceipt `json:"completed"`
	Blocker  *runBlocker      `json:"blocker,omitempty"`
}

type sequenceOwner struct {
	Config string `json:"config"`
}

type sequenceInfo struct {
	Root            string      `json:"root_config"`
	Current         string      `json:"current_config"`
	Phase           string      `json:"phase"`
	Completed       int         `json:"completed_sets"`
	Total           int         `json:"total_sets"`
	Blocker         *runBlocker `json:"blocker,omitempty"`
	CompletedSlices int         `json:"current_completed_slices"`
	TotalSlices     int         `json:"current_total_slices"`
	WorkerTurns     int         `json:"current_worker_turns"`
	WorkerSession   string      `json:"current_worker_session,omitempty"`
}

type configPaths []string

func (p *configPaths) String() string { return fmt.Sprint([]string(*p)) }
func (p *configPaths) Set(value string) error {
	if len(*p) >= 31 || value == "" {
		return errors.New("expected 1 to 31 explicitly listed successor configs")
	}
	*p = append(*p, value)
	return nil
}

func loadSequence(a *app) (*deliverySequence, error) {
	var s deliverySequence
	if err := readJSON(filepath.Join(a.dir, "sequence.json"), &s); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if s.Version != 1 || len(s.Sets) < 2 || len(s.Sets) > 32 || s.Current < 0 || s.Current >= len(s.Sets) || len(s.Receipts) < s.Current || len(s.Receipts) > s.Current+1 || s.Sets[0].Path != a.path {
		return nil, errors.New("invalid sequence checkpoint")
	}
	switch s.Phase {
	case "running", "handoff", "blocked", "complete":
	default:
		return nil, errors.New("invalid sequence phase")
	}
	if s.Phase == "complete" && len(s.Receipts) != len(s.Sets) || s.Phase == "handoff" && len(s.Receipts) != s.Current+1 {
		return nil, errors.New("sequence phase does not match completion receipts")
	}
	seen := map[string]bool{}
	for i, ref := range s.Sets {
		if !filepath.IsAbs(ref.Path) || len(ref.Hash) != 64 || seen[ref.Path] {
			return nil, errors.New("invalid or duplicate sequence config")
		}
		seen[ref.Path] = true
		if i < len(s.Receipts) && (s.Receipts[i].ConfigRef != ref || !fullSHA.MatchString(s.Receipts[i].Head)) {
			return nil, errors.New("invalid sequence closure receipt")
		}
	}
	return &s, nil
}

func (s *deliverySequence) save(a *app) error {
	return writeJSON(filepath.Join(a.dir, "sequence.json"), s)
}

func sequenceApp(root *app, ref configRef) (*app, error) {
	a, err := loadApp(ref.Path)
	if err != nil {
		failure := contextFailure("sequence", "configuration", "A pinned configuration or checkpoint cannot be loaded", 1)
		failure.Blocker.NextAction = inspectAction("restore_approved_configuration", ref.Path)
		return nil, failure
	}
	first, err := projectIdentity(root.config.Repo)
	if err != nil {
		return nil, err
	}
	other, err := projectIdentity(a.config.Repo)
	if err != nil {
		return nil, err
	}
	if a.state.ConfigHash != ref.Hash || first != other || a.config.Origin != root.config.Origin || a.config.Repository != root.config.Repository || a.config.Forge != root.config.Forge {
		return nil, contextFailure("sequence", "configuration", "A pinned configuration changed or belongs to another repository", 1)
	}
	return a, nil
}

// Enqueueing is explicit authority for this fixed list, not discovery of another approved plan.
func prepareSequence(a *app, next []string) error {
	if len(next) < 1 || len(next) > 31 {
		return errors.New("expected 1 to 31 explicitly listed successor configs")
	}
	l := &runLease{}
	defer l.close()
	if err := l.acquire(filepath.Join(a.dir, "launch.lock")); err != nil {
		return contextFailure("sequence", "ownership", "Another launcher owns sequence preparation", 1)
	}
	lease, err := acquireRun(a)
	if err != nil {
		return contextFailure("sequence", "ownership", "Repository or controller ownership cannot be acquired", 1)
	}
	defer lease.close()
	if err := workerIdle(a); err != nil {
		return err
	}
	existing, err := loadSequence(a)
	if err != nil {
		return err
	}
	s := &deliverySequence{Version: 1, Phase: "running", Sets: []configRef{{Path: a.path, Hash: a.state.ConfigHash}}}
	seen := map[string]bool{a.path: true}
	for _, path := range next {
		child, err := loadApp(path)
		if err != nil {
			return err
		}
		ref := configRef{Path: child.path, Hash: child.state.ConfigHash}
		if _, err := sequenceApp(a, ref); err != nil {
			return err
		}
		if seen[ref.Path] {
			return errors.New("a set can appear only once in a sequence")
		}
		seen[ref.Path] = true
		if err := workerIdle(child); err != nil {
			return err
		}
		if busy, err := lockBusy(filepath.Join(child.dir, "run.lock")); err != nil || busy {
			return contextFailure("sequence", "ownership", "A successor has an active or uncertain controller", 1)
		}
		var owner sequenceOwner
		if err := readJSON(filepath.Join(child.dir, "sequence-owner.json"), &owner); err == nil && owner.Config != a.path {
			return errors.New("successor already belongs to another sequence")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if nested, err := loadSequence(child); err != nil || nested != nil {
			return errors.New("nested delivery sequences are not supported")
		}
		s.Sets = append(s.Sets, ref)
	}
	if existing != nil {
		if len(existing.Sets) != len(s.Sets) {
			return errors.New("the approved sequence is already pinned; resume it without replacing its list")
		}
		for i := range s.Sets {
			if s.Sets[i] != existing.Sets[i] {
				return errors.New("the approved sequence is already pinned; resume it without replacing its list")
			}
		}
		return nil
	}
	if err := a.fetch(); err != nil {
		return err
	}
	for _, ref := range s.Sets {
		child, err := sequenceApp(a, ref)
		if err != nil {
			return err
		}
		if err := child.verifyIdentity(); err != nil {
			return err
		}
	}
	if err := s.save(a); err != nil {
		return err
	}
	return claimSequence(a, s)
}

func claimSequence(root *app, s *deliverySequence) error {
	for _, ref := range s.Sets[1:] {
		a, err := sequenceApp(root, ref)
		if err != nil {
			return err
		}
		var owner sequenceOwner
		if err := readJSON(filepath.Join(a.dir, "sequence-owner.json"), &owner); err == nil && owner.Config != root.path {
			return errors.New("successor ownership changed")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := writeJSON(filepath.Join(a.dir, "sequence-owner.json"), sequenceOwner{Config: root.path}); err != nil {
			return err
		}
	}
	return nil
}

func sequenceStatus(a *app, r *runInfo) {
	if r.Status == "invalid" {
		return
	}
	root := a
	var owner sequenceOwner
	if err := readJSON(filepath.Join(a.dir, "sequence-owner.json"), &owner); err == nil {
		var err error
		root, err = loadApp(owner.Config)
		if err != nil {
			r.Status, r.Blocker = "blocked", &contextFailure("sequence", "configuration", "Sequence owner cannot be loaded", 1).Blocker
			return
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		r.Status, r.Blocker = "blocked", &contextFailure("sequence", "configuration", "Sequence ownership record is invalid", 1).Blocker
		return
	}
	s, err := loadSequence(root)
	if err != nil || root != a && s == nil {
		r.Status, r.Blocker = "blocked", &contextFailure("sequence", "configuration", "Sequence checkpoint cannot be loaded", 1).Blocker
		return
	}
	if s == nil {
		return
	}
	r.Sequence = &sequenceInfo{Root: root.path, Current: s.Sets[s.Current].Path, Phase: s.Phase, Completed: len(s.Receipts), Total: len(s.Sets), Blocker: s.Blocker}
	if current, err := sequenceApp(root, s.Sets[s.Current]); err == nil {
		r.Sequence.CompletedSlices, r.Sequence.TotalSlices = current.state.Next, len(current.config.Slices)
		r.Sequence.WorkerTurns, r.Sequence.WorkerSession = current.state.Turns, current.state.Session
		if root == a || a.path == current.path {
			r.ProviderRetry = current.retryInfo()
		}
	} else {
		r.Status, r.Blocker = "blocked", &contextFailure("sequence", "configuration", "Current sequence config or checkpoint changed", 1).Blocker
		return
	}
	if s.Phase == "complete" {
		return
	}
	busy, err := lockBusy(filepath.Join(root.dir, "run.lock"))
	if err != nil {
		r.Status, r.Blocker = "blocked", &contextFailure("sequence", "ownership", "Sequence controller ownership is uncertain", 1).Blocker
	} else if busy {
		if r.Status != "starting" {
			r.Status = "running"
		}
		r.Blocker = nil
		if root != a {
			r.Status = "queued"
			if a.path == s.Sets[s.Current].Path {
				r.Status = "running_in_sequence"
			}
		}
		if (root == a || a.path == s.Sets[s.Current].Path) && r.ProviderRetry != nil && r.ProviderRetry.NextAt != nil {
			r.Status = "waiting"
			if current, err := sequenceApp(root, s.Sets[s.Current]); err == nil {
				r.Blocker = current.state.Blocker
			}
		}
	} else {
		r.Status = "blocked"
		r.Blocker = s.Blocker
		if r.Blocker == nil {
			r.Blocker = &contextFailure("handoff", "interrupted", "Sequence controller exited before completing the approved continuation; progress is retained", 1).Blocker
		}
		if r.Blocker.Class == "ownership" {
			idle := true
			for i := s.Current; i <= min(s.Current+1, len(s.Sets)-1); i++ {
				candidate, err := sequenceApp(root, s.Sets[i])
				if err != nil || workerIdle(candidate) != nil {
					idle = false
					break
				}
			}
			if idle {
				copy := *r.Blocker
				copy.Retryable = true
				r.Blocker = &copy
			}
		}
		r.Blocker.NextAction = commandAction("looper", "resume", "--config", root.path, "--detach", "--json")
	}
	if root != a {
		r.Blocker = &runBlocker{Code: "sequence_owned", Component: "sequence", Class: "ownership", Message: "Resume the sequence entry to preserve serial handoff", NextAction: commandAction("looper", "resume", "--config", root.path, "--detach", "--json")}
	}
}

func (a *app) block(err error) error {
	a.state.Status, a.state.Feedback = "blocked", activityText(err.Error())
	var failure *executionFailure
	a.state.Blocker = nil
	if errors.As(err, &failure) {
		a.state.Blocker = &failure.Blocker
	}
	return errors.Join(err, a.save())
}

// Record launcher failure only while no controller owns the repository.
func blockSequenceLaunch(a *app, blocker *runBlocker) error {
	lease, err := acquireRun(a)
	if err != nil {
		return nil
	}
	defer lease.close()
	s, err := loadSequence(a)
	if err != nil || s == nil {
		return err
	}
	copy := *blocker
	copy.Retryable = true // Explicit resume rechecks the repaired launcher; this does not enable automatic retries.
	s.Phase, s.Blocker = "blocked", &copy
	return s.save(a)
}

func runOwnedSequence(root *app, s *deliverySequence, options controllerOptions, identity processIdentity, ready func(error) error) error {
	if s == nil {
		return runOwnedSet(root, root, options, identity, ready)
	}
	// Validate every pin before starting work. No changed successor is silently substituted.
	for _, ref := range s.Sets {
		if _, err := sequenceApp(root, ref); err != nil {
			return err
		}
	}
	if s.Phase == "complete" {
		a, err := sequenceApp(root, s.Sets[s.Current])
		if err != nil {
			return err
		}
		return runOwnedSet(root, a, options, identity, ready)
	}
	for {
		a, err := sequenceApp(root, s.Sets[s.Current])
		if err != nil {
			return err
		}
		if len(s.Receipts) == s.Current {
			if err := a.fetch(); err != nil {
				return err
			}
			for _, receipt := range s.Receipts {
				if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", receipt.Head, "origin/dev"); err != nil {
					return contextFailure("handoff", "validation", "A retained closure commit is no longer integrated in dev", 1)
				}
			}
			s.Phase, s.Blocker = "running", nil
			if err := s.save(root); err != nil {
				return err
			}
			if err := runOwnedSet(root, a, options, identity, ready); err != nil {
				return err
			}
			head, err := git(a.config.Repo, "rev-parse", "origin/dev")
			if err != nil {
				return err
			}
			s.Receipts = append(s.Receipts, closureReceipt{ConfigRef: s.Sets[s.Current], Head: head})
			s.Phase = "handoff"
			if err := s.save(root); err != nil {
				return err
			}
		}
		if s.Current == len(s.Sets)-1 {
			s.Phase, s.Blocker = "complete", nil
			return s.save(root)
		}
		// Preserve handoff intent until closure and successor access are reconfirmed.
		s.Phase, s.Blocker = "handoff", nil
		if err := s.save(root); err != nil {
			return err
		}
		if err := a.fetch(); err != nil {
			return err
		}
		for _, receipt := range s.Receipts {
			if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", receipt.Head, "origin/dev"); err != nil {
				return contextFailure("handoff", "validation", "A retained closure commit is no longer integrated in dev", 1)
			}
		}
		if err := a.finished(); err != nil {
			return err
		}
		next, err := sequenceApp(root, s.Sets[s.Current+1])
		if err != nil {
			return err
		}
		if err := requireIdleWorker(next); err != nil {
			return err
		}
		if err := next.preflight(); err != nil {
			return err
		}
		if err := next.verifyIdentity(); err != nil {
			return err
		}
		s.Current++
		s.Phase = "running"
		if err := s.save(root); err != nil {
			return err
		}
	}
}
