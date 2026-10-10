package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// The shared Git directory identifies a project across subdirectories, symlinks and worktrees.
func projectIdentity(dir string) (string, error) {
	command, err := executionCommand(context.Background(), dir, "git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	// A launcher's Git overrides must not redirect project detection away from the requested directory.
	env := command.Env
	command.Env = nil
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "GIT_DIR" && key != "GIT_WORK_TREE" && key != "GIT_COMMON_DIR" {
			command.Env = append(command.Env, entry)
		}
	}
	common, err := commandOutput(command)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(common)
}

func currentProject() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return projectIdentity(dir)
}

func belongsToProject(e runEntry, identity string) bool {
	var c config
	if readJSON(e.Config, &c) != nil || c.Repo == "" {
		return false
	}
	actual, err := projectIdentity(c.Repo)
	return err == nil && actual == identity
}

func activeRuns(runs []runInfo) []runInfo {
	var active []runInfo
	for _, r := range runs {
		if r.Status == "running" || r.Status == "starting" {
			active = append(active, r)
		}
	}
	return active
}

// Prefer the live owner over local history, and unfinished work over completed sets.
func currentRuns(runs []runInfo) []runInfo {
	if active := activeRuns(runs); len(active) != 0 {
		return active
	}
	var unfinished []runInfo
	for _, r := range runs {
		if r.Status != "complete" {
			unfinished = append(unfinished, r)
		}
	}
	if len(unfinished) != 0 {
		return unfinished
	}
	return runs
}
