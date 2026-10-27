package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// A grant augments one existing repair without replacing its frozen policy.
type gateRepairPathGrant struct {
	Repair         int      `json:"repair"`
	FailedHead     string   `json:"failed_head"`
	AuthorizedHead string   `json:"authorized_head"`
	Paths          []string `json:"paths"`
	Reason         string   `json:"reason"`
}

func validRepairPathReason(reason string) bool {
	return reason != "" && strings.TrimSpace(reason) == reason && len(reason) <= 512 && !strings.ContainsAny(reason, "\r\n\x00")
}

func (a *app) trackedRepairFile(base, path string) bool {
	if !safePath(path) || path == "." || path == ".git" || strings.HasPrefix(path, ".git/") || strings.ContainsAny(path, "\r\n\x00") {
		return false
	}
	entry, err := git(a.config.Repo, "--literal-pathspecs", "ls-tree", base, "--", path)
	metadata, _, _ := strings.Cut(entry, "\t")
	fields := strings.Fields(metadata)
	return err == nil && len(fields) == 3 && fields[1] == "blob" && (fields[0] == "100644" || fields[0] == "100755")
}

func (a *app) gateRepairPathGranted(repair int, path string) bool {
	for _, grant := range a.state.GateRepairPaths {
		if grant.Repair == repair && slices.Contains(grant.Paths, path) {
			return true
		}
	}
	return false
}

func authorizeGateRepairPaths(configPath, failedHead string, paths []string, reason string) error {
	a, err := loadApp(configPath)
	if err != nil {
		return err
	}
	lease, err := acquireRun(a)
	if err != nil {
		return err
	}
	defer lease.close()
	a, err = loadApp(configPath)
	if err != nil {
		return err
	}
	if err := requireIdleWorker(a); err != nil {
		return err
	}
	repair := a.activeGateRepair()
	if a.state.Status != "blocked" || a.state.Pending != nil || a.state.Gate != nil || repair == nil || a.state.GateFailure == nil || a.state.Next != len(a.config.Slices) {
		return errors.New("path authorization requires an idle blocked active repair and no successful gate or uncertain merge")
	}
	if !fullSHA.MatchString(failedHead) || failedHead != repair.Base || failedHead != repair.Failure.Head || failedHead != a.state.GateFailure.Head {
		return errors.New("path authorization requires the exact retained failed base")
	}
	head, err := cleanHead(a.config.Repo)
	if err != nil {
		return err
	}
	if head != failedHead {
		return errors.New("path authorization requires the exact clean failed candidate")
	}
	if len(paths) == 0 || len(paths) > 100 || !validRepairPathReason(reason) {
		return errors.New("path authorization requires 1-100 exact tracked files and a single-line reason of at most 512 bytes")
	}
	paths = slices.Clone(paths)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	n := len(a.state.GateRepairs)
	for _, grant := range a.state.GateRepairPaths {
		if grant.Repair == n && grant.FailedHead == failedHead && reflect.DeepEqual(grant.Paths, paths) && grant.Reason == reason {
			return nil
		}
	}
	policy := a.gateRepairPolicyFor(n)
	for _, path := range paths {
		if !a.trackedRepairFile(failedHead, path) {
			return fmt.Errorf("path authorization requires an exact regular tracked file: %s", path)
		}
		for _, allowed := range policy.AllowedPaths {
			if path == allowed || strings.HasPrefix(path, allowed+"/") {
				return fmt.Errorf("repair path is already authorized: %s", path)
			}
		}
		if a.gateRepairPathGranted(n, path) {
			return fmt.Errorf("repair path is already authorized: %s", path)
		}
	}
	branchHead, err := git(a.config.Repo, "rev-parse", "--verify", "refs/heads/"+repair.Identity.Branch)
	if err != nil {
		return errors.New("path authorization requires the existing repair branch")
	}
	if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", failedHead, branchHead); err != nil {
		return errors.New("repair branch does not include its failed base")
	}
	a.state.GateRepairPaths = append(a.state.GateRepairPaths, gateRepairPathGrant{Repair: n, FailedHead: failedHead, AuthorizedHead: branchHead, Paths: paths, Reason: reason})
	if err := a.verifyGateRepairPathGrants(); err != nil {
		return err
	}
	return a.save()
}

func (a *app) verifyGateRepairPathGrants() error {
	total := 0
	for _, grant := range a.state.GateRepairPaths {
		total += len(grant.Paths)
		if grant.Repair < 1 || grant.Repair > len(a.state.GateRepairs) || !fullSHA.MatchString(grant.FailedHead) || !fullSHA.MatchString(grant.AuthorizedHead) || len(grant.Paths) == 0 || total > 100 || !validRepairPathReason(grant.Reason) {
			return errors.New("invalid exact gate-repair path authorization")
		}
		repair := a.state.GateRepairs[grant.Repair-1]
		if grant.FailedHead != repair.Base || grant.FailedHead != repair.Failure.Head {
			return errors.New("gate-repair path authorization differs from its failed base")
		}
		if _, err := git(a.config.Repo, "merge-base", "--is-ancestor", repair.Base, grant.AuthorizedHead); err != nil {
			return errors.New("gate-repair path authorization head does not include its base")
		}
		for _, path := range grant.Paths {
			if !a.trackedRepairFile(grant.FailedHead, path) {
				return errors.New("gate-repair path authorization contains a non-file path")
			}
		}
	}
	return nil
}
