package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// Producers write an exclusively allocated descriptor. An inventory or a file
// name alone never gives the controller authority over an existing file.
type outputPolicy struct {
	Path    string `json:"path"`
	Command string `json:"command"`
}

type outputIdentity struct {
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
	Size       int64  `json:"size"`
	Mode       uint32 `json:"mode"`
	Digest     string `json:"digest"`
	ModifiedNS int64  `json:"modified_ns"`
}

type outputReceipt struct {
	ID           string          `json:"id"`
	ConfigHash   string          `json:"config_hash"`
	PolicyHash   string          `json:"policy_hash"`
	ValidationID string          `json:"validation_id"`
	Worktree     string          `json:"worktree"`
	Head         string          `json:"head"`
	Path         string          `json:"path"`
	CommandHash  string          `json:"command_hash"`
	Producer     processIdentity `json:"producer"`
	Identity     outputIdentity  `json:"identity"`
	Status       string          `json:"status"`
	Destination  string          `json:"destination,omitempty"`
}

func safeOutputLocation(root, path string) (string, error) {
	if !safeRecoveryPath(path) {
		return "", errors.New("generated output requires an exact path inside the validation checkout")
	}
	parent := filepath.Dir(path)
	for parent != "." {
		info, err := os.Lstat(filepath.Join(root, parent))
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("generated output parent is absent, linked or not a directory")
		}
		parent = filepath.Dir(parent)
	}
	return filepath.Join(root, path), nil
}

func describeOutput(path string) (outputIdentity, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return outputIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return outputIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 || info.Size() > 512<<20 {
		return outputIdentity{}, errors.New("output is not a bounded exclusive regular file")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (512<<20)+1))
	if err != nil || n != info.Size() {
		return outputIdentity{}, errors.New("output changed while recording its identity")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != info.Size() || after.ModTime() != info.ModTime() {
		return outputIdentity{}, errors.New("output changed while recording its digest")
	}
	return outputIdentity{Device: uint64(stat.Dev), Inode: stat.Ino, Size: n, Mode: uint32(info.Mode()), Digest: hex.EncodeToString(hash.Sum(nil)), ModifiedNS: info.ModTime().UnixNano()}, nil
}

func descendantOf(child int, ancestor processIdentity) bool {
	if alive, err := processState(ancestor); err != nil || !alive {
		return false
	}
	for n := 0; n < 128 && child > 1; n++ {
		if child == ancestor.PID {
			return true
		}
		b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(child), "stat"))
		if err != nil {
			return false
		}
		end := strings.LastIndexByte(string(b), ')')
		if end < 0 {
			return false
		}
		fields := strings.Fields(string(b[end+1:]))
		if len(fields) < 2 {
			return false
		}
		child, err = strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
	}
	return false
}

func generateOutput(configPath, worktree, path, command string) error {
	a, err := loadApp(configPath)
	if err != nil {
		return err
	}
	if a.state.Recovery == nil {
		return errors.New("generated output has no pinned recovery delegation")
	}
	allowed := false
	for _, output := range a.state.Recovery.Policy.Outputs {
		allowed = allowed || output.Path == path && output.Command == command
	}
	if !allowed {
		return errors.New("output path and producer command are not explicitly delegated")
	}
	v, active, err := a.validationState()
	if err != nil {
		return err
	}
	if !active || v == nil || !descendantOf(os.Getpid(), v.Child) {
		return errors.New("output producer must descend from this execution's verified active validation command")
	}
	if worktree == "" {
		worktree = v.Worktree
	}
	worktree, err = filepath.EvalSymlinks(worktree)
	if err != nil || worktree != v.Worktree {
		return errors.New("output producer worktree differs from its validation owner")
	}
	location, err := safeOutputLocation(worktree, path)
	if err != nil {
		return err
	}
	tracked, err := git(worktree, "--literal-pathspecs", "ls-files", "--", path)
	if err != nil || tracked != "" {
		return errors.New("generated output must not replace a tracked file")
	}
	file, err := os.OpenFile(location, os.O_CREATE|os.O_EXCL|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("allocate owned output without replacing existing content: %w", err)
	}
	defer file.Close()
	initial, err := file.Stat()
	if err != nil {
		return err
	}
	id := rand.Text()
	receipt := outputReceipt{ID: id, ConfigHash: a.state.ConfigHash, PolicyHash: a.state.Recovery.PolicyHash, ValidationID: v.ID, Worktree: worktree, Head: v.Head, Path: path, CommandHash: digest([]byte(command)), Status: "reserved"}
	metadata := filepath.Join(recoveryMetadataPath(a), "outputs", id+".json")
	if err := writeJSON(metadata, receipt); err != nil {
		return err
	}
	child, err := executionCommand(context.Background(), worktree, "sh", "-ec", command)
	if err != nil {
		return err
	}
	child.Env = append(child.Env, "LOOPER_OUTPUT_PATH=/proc/self/fd/3")
	child.ExtraFiles = []*os.File{file}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	receipt.Producer, err = identifyProcess(child.Process.Pid)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = writeJSON(metadata, receipt)
	}
	waitErr := child.Wait() // A producer is never terminated by a controller timeout.
	if err != nil {
		return errors.Join(err, waitErr)
	}
	final, err := os.Lstat(location)
	if err != nil || !os.SameFile(initial, final) {
		return errors.New("producer replaced its exclusively allocated output; ownership is ambiguous")
	}
	receipt.Identity, err = describeOutput(location)
	if err != nil {
		return err
	}
	receipt.Status = "complete"
	return errors.Join(waitErr, writeJSON(metadata, receipt))
}

func (a *app) quarantineOutputs() (bool, error) {
	if a.state.Recovery == nil || len(a.state.Recovery.Policy.Outputs) == 0 {
		return false, nil
	}
	if err := a.requireRepositoryOwnership(); err != nil {
		return false, err
	}
	if a.state.Pending != nil {
		return false, recoveryStop("pending_merge", "integrity", "An uncertain merge prevents output quarantine")
	}
	if err := requireIdleWorkerProcess(a); err != nil {
		return false, err
	}
	dir := filepath.Join(recoveryMetadataPath(a), "outputs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(entries) > 512 {
		return false, recoveryStop("output_limit", "budget", "Owned-output receipt limit reached")
	}
	v, active, err := a.validationState()
	if err != nil {
		return false, err
	}
	if active {
		return false, nil // The ordinary validation owner continues observation.
	}
	changed := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return false, errors.New("invalid owned-output receipt inventory")
		}
		var receipt outputReceipt
		path := filepath.Join(dir, entry.Name())
		if err := readJSON(path, &receipt); err != nil {
			return false, err
		}
		if receipt.Status == "quarantined" {
			continue
		}
		if receipt.ConfigHash != a.state.ConfigHash || receipt.PolicyHash != a.state.Recovery.PolicyHash || !activityIdentifier.MatchString(receipt.ID) || entry.Name() != receipt.ID+".json" || !fullSHA.MatchString(receipt.Head) || v == nil || receipt.ValidationID != v.ID || receipt.Worktree != v.Worktree || receipt.Head != v.Head {
			return false, recoveryStop("output_identity", "integrity", "Output receipt does not match its retained validation and delegation")
		}
		allowed := false
		for _, output := range a.state.Recovery.Policy.Outputs {
			allowed = allowed || output.Path == receipt.Path && digest([]byte(output.Command)) == receipt.CommandHash
		}
		if !allowed {
			return false, errors.New("output receipt is outside pinned producer authority")
		}
		if alive, err := processState(receipt.Producer); err != nil || alive {
			return false, recoveryStop("output_producer", "ownership", "Output producer ownership is unresolved")
		}
		if receipt.Status != "complete" && receipt.Status != "quarantining" {
			return false, recoveryStop("output_incomplete", "integrity", "Output producer did not record a complete attributed result")
		}
		location, err := safeOutputLocation(receipt.Worktree, receipt.Path)
		if err != nil {
			return false, err
		}
		destination := filepath.Join(recoveryMetadataPath(a), "quarantine", receipt.ID, "payload")
		if receipt.Status == "quarantining" {
			if receipt.Destination != destination {
				return false, errors.New("quarantine destination changed")
			}
			if _, err := os.Lstat(location); errors.Is(err, os.ErrNotExist) {
				identity, err := describeOutput(destination)
				if err != nil || identity != receipt.Identity {
					return false, errors.New("interrupted quarantine outcome cannot be verified")
				}
				receipt.Status = "quarantined"
				if err := writeJSON(path, receipt); err != nil {
					return false, err
				}
				changed = true
				continue
			}
		}
		identity, err := describeOutput(location)
		if err != nil || identity != receipt.Identity {
			return false, recoveryStop("output_changed", "integrity", "Generated output was modified; preserve it for inspection")
		}
		if !slices.Contains(a.state.Recovery.OutputBatches, receipt.ValidationID) {
			if len(a.state.Recovery.OutputBatches) >= 512 {
				return false, recoveryStop("output_limit", "budget", "Owned-output batch allowance is exhausted")
			}
			command := v.Commands[max(0, min(v.Check-1, len(v.Commands)-1))]
			family, err := a.recoveryFamily(digest([]byte("generated_output\x00" + command + "\x00" + v.FailureFamily)))
			if err != nil {
				return false, err
			}
			if family.Quarantines >= a.state.Recovery.Policy.FailedRepairsPerFamily {
				return false, recoveryStop("output_stalled", "technical", "Generated output recurred without verified progress; preserve the remaining content")
			}
			family.Operation, family.Quarantines = digest([]byte(command)), family.Quarantines+1
			a.state.Recovery.OutputBatches = append(a.state.Recovery.OutputBatches, receipt.ValidationID)
			if err := a.save(); err != nil {
				return false, err
			}
		}
		tracked, err := git(receipt.Worktree, "--literal-pathspecs", "ls-files", "--", receipt.Path)
		if err != nil || tracked != "" {
			return false, errors.New("owned output became tracked; quarantine refused")
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return false, err
		}
		if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("quarantine refuses to overwrite existing content")
		}
		receipt.Status, receipt.Destination = "quarantining", destination
		if err := writeJSON(path, receipt); err != nil {
			return false, err
		}
		// Recheck immediately before the atomic move. Cross-filesystem moves stop safely.
		identity, err = describeOutput(location)
		if err != nil || identity != receipt.Identity {
			return false, errors.New("output changed before quarantine")
		}
		if err := os.Rename(location, destination); err != nil {
			return false, err
		}
		identity, err = describeOutput(destination)
		if err != nil || identity != receipt.Identity {
			return false, recoveryStop("output_changed", "integrity", "Quarantined content differs from its attributed output; preserve both locations for inspection")
		}
		receipt.Status = "quarantined"
		if err := writeJSON(path, receipt); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		return true, a.retryAfterOutputQuarantine(v)
	}
	return false, nil
}

func (a *app) outputBatchRecovered(v *validationRun) bool {
	if a.state.Recovery == nil || v == nil || v.Failure == nil || v.Failure.Code != "validation_candidate_dirty" || !slices.Contains(a.state.Recovery.OutputBatches, v.ID) {
		return false
	}
	dir := filepath.Join(recoveryMetadataPath(a), "outputs")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 512 {
		return false
	}
	found := false
	for _, entry := range entries {
		var receipt outputReceipt
		if err := readJSON(filepath.Join(dir, entry.Name()), &receipt); err != nil {
			return false
		}
		if receipt.ValidationID != v.ID {
			continue
		}
		if receipt.Status != "quarantined" || receipt.ConfigHash != a.state.ConfigHash || receipt.PolicyHash != a.state.Recovery.PolicyHash || !activityIdentifier.MatchString(receipt.ID) || receipt.Destination != filepath.Join(recoveryMetadataPath(a), "quarantine", receipt.ID, "payload") {
			return false
		}
		found = true
	}
	return found
}
