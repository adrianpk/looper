package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

type repairPathSource struct {
	PR           int    `json:"pr"`
	Head         string `json:"head"`
	MergedCommit string `json:"merged_commit"`
	Entry        string `json:"tree_entry"`
}

func (a *app) deliveryRecords() []mergeRecord {
	records := slices.Clone(a.state.Merged)
	for _, r := range a.state.GateRepairs {
		if r.Merge != nil {
			records = append(records, *r.Merge)
		}
	}
	return records
}

func (a *app) repairPathSource(base, path string) (*repairPathSource, error) {
	if !a.trackedRepairFile(base, path) {
		return nil, errors.New("derived authority requires a regular tracked file")
	}
	records := a.deliveryRecords()
	if len(records) > 1024 {
		return nil, errors.New("delivery provenance exceeds the bounded record limit")
	}
	for n := len(records) - 1; n >= 0; n-- {
		r := records[n]
		p, err := a.pull(r.PR)
		if err != nil {
			return nil, err
		}
		if !p.Merged || p.Head.SHA != r.Head || p.MergedCommit != r.MergedCommit || p.Base.Ref != "dev" || p.Base.Repo.FullName != a.config.Repository || p.Head.Repo.FullName != a.config.Repository {
			return nil, recoveryStop("provenance", "integrity", "Recorded delivery integration no longer matches the canonical PR")
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", r.MergedCommit, base); err != nil {
			return nil, err
		}
		found := false
		for page := 1; page <= 100; page++ {
			var files []struct {
				Filename string `json:"filename"`
				Status   string `json:"status"`
				Previous string `json:"previous_filename"`
			}
			if err := a.api("GET", a.endpoint(fmt.Sprintf("/pulls/%d/files?limit=100&page=%d", r.PR, page)), nil, &files); err != nil {
				return nil, err
			}
			for _, f := range files {
				if f.Filename == path {
					if f.Status == "renamed" || f.Status == "removed" || f.Previous != "" && f.Previous != path {
						return nil, errors.New("renamed or removed paths require explicit authority")
					}
					found = true
				}
			}
			if len(files) < 100 {
				break
			}
			if page == 100 {
				return nil, errors.New("canonical PR file inventory exceeds the provenance limit")
			}
		}
		if !found {
			continue
		}
		current, err := a.pull(r.PR)
		if err != nil {
			return nil, err
		}
		if !current.Merged || current.Head.SHA != r.Head || current.MergedCommit != r.MergedCommit {
			return nil, errors.New("canonical PR changed during provenance verification")
		}
		entry, err := git(a.config.Repo, "--literal-pathspecs", "ls-tree", r.MergedCommit, "--", path)
		if err != nil {
			return nil, err
		}
		grant := gateRepairPathGrant{FailedHead: base, Paths: []string{path}, Source: &repairPathSource{PR: r.PR, Head: r.Head, MergedCommit: r.MergedCommit, Entry: entry}}
		if err := a.verifyRepairPathSource(grant); err != nil {
			return nil, err
		}
		return grant.Source, nil
	}
	return nil, recoveryStop("path_authority", "decision", "Requested file is outside verified delivery provenance and existing explicit authority")
}

// Recovery can verify pinned provenance offline; only grant creation consults the forge.
func (a *app) verifyRepairPathSource(g gateRepairPathGrant) error {
	s := g.Source
	if s == nil || len(g.Paths) != 1 || !fullSHA.MatchString(s.Head) || !fullSHA.MatchString(s.MergedCommit) || s.Entry == "" {
		return errors.New("invalid derived exact-file provenance")
	}
	found := false
	for _, r := range a.deliveryRecords() {
		found = found || r.PR == s.PR && r.Head == s.Head && r.MergedCommit == s.MergedCommit
	}
	if !found {
		return errors.New("derived grant does not belong to a recorded delivery integration")
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", s.MergedCommit, g.FailedHead); err != nil {
		return err
	}
	for _, revision := range []string{s.MergedCommit, g.FailedHead} {
		entry, err := git(a.config.Repo, "--literal-pathspecs", "ls-tree", revision, "--", g.Paths[0])
		if err != nil || entry != s.Entry {
			return errors.New("derived file contents or mode differ from their delivery provenance")
		}
	}
	changes, err := git(a.config.Repo, "--literal-pathspecs", "log", "--format=%H", s.MergedCommit+".."+g.FailedHead, "--", g.Paths[0])
	if err != nil || changes != "" {
		return errors.New("intervening file changes require explicit authority")
	}
	return nil
}

func (a *app) deriveRepairPaths(paths []string) error {
	if err := a.recoveryAdmission(); err != nil {
		return err
	}
	repair := a.activeGateRepair()
	if repair == nil {
		return recoveryStop("path_phase", "decision", "Additional path delegation only applies to the active final-gate repair")
	}
	for _, path := range paths {
		if !safeRecoveryPath(path) {
			return recoveryStop("unsafe_path", "decision", "Requested file is not a safe exact repository path")
		}
		// Explicit authority remains valid regardless of a file's historical contributors.
		if a.repairPathAuthorized(path) {
			continue
		}
		if path == "AGENTS.md" || path == ".agents" || strings.HasPrefix(path, ".agents/") || path == a.config.Plan || path == a.config.Tracker {
			return recoveryStop("contract_path", "decision", "Additional authority over agent contracts or approved delivery identities requires explicit approval")
		}
		for _, root := range a.state.Recovery.Policy.ExcludedPaths {
			if path == root || strings.HasPrefix(path, root+"/") {
				return recoveryStop("excluded_path", "decision", "Requested derived authority is excluded by the delegation")
			}
		}
		source, err := a.repairPathSource(repair.Base, path)
		if err != nil {
			return err
		}
		if err := a.appendGateRepairPaths(repair.Base, []string{path}, "Verified delivery-produced file omitted from repair preparation", source); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) repairPathAuthorized(path string) bool {
	if a.activeGateRepair() == nil || !safeRecoveryPath(path) {
		return false
	}
	if a.gateRepairPathGranted(len(a.state.GateRepairs), path) {
		return true
	}
	policy := a.gateRepairPolicyFor(len(a.state.GateRepairs))
	for _, root := range policy.AllowedPaths {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}
