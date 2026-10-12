package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type workerResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

const workerSchema = `{"type":"object","additionalProperties":false,"required":["status","message"],"properties":{"status":{"type":"string","enum":["continue","pr_ready","ready_for_gate","closed","blocked"]},"message":{"type":"string"}}}`

func (a *app) work(phase, task string) error {
	if a.state.Turns >= a.config.MaxTurns {
		return errors.New("worker turn limit reached; inspect the checkpoint before continuing")
	}
	if err := requireIdleWorker(a); err != nil {
		return err
	}
	before, err := a.fingerprint()
	if err != nil {
		return err
	}
	if a.state.Stalled >= a.config.MaxStalled && before == a.state.Fingerprint {
		return errors.New("worker progress limit reached; resolve the workspace blocker before continuing")
	}
	if before != a.state.Fingerprint {
		a.state.Stalled = 0
	}
	if retry := a.state.ProviderRetry; retry != nil && retry.Phase != "" {
		if retry.Phase != phase || retry.Instruction != task {
			return contextFailure("worker", "integrity", "Pending provider retry must resume its retained phase and instruction", 1)
		}
		before = retry.Before
	}
	if err := a.workerWithRecovery(phase, task, before); err != nil {
		return err
	}
	if err := a.fetch(); err != nil {
		return err
	}
	after, err := a.fingerprint()
	if err != nil {
		return err
	}
	if before == after {
		a.state.Stalled++
	} else {
		a.state.Stalled = 0
	}
	a.state.Fingerprint = after
	if err := a.save(); err != nil {
		return err
	}
	if a.reply.Status == "blocked" {
		return fmt.Errorf("Codex reported a concrete blocker: %s", a.reply.Message)
	}
	if a.state.Stalled >= a.config.MaxStalled {
		return errors.New("worker repeatedly returned without observable progress; checkpoint retained")
	}
	return nil
}

func (a *app) invokeWorker(phase, task string) error {
	schemaPath := filepath.Join(a.dir, "worker-schema.json")
	if err := writeAtomic(schemaPath, []byte(workerSchema)); err != nil {
		return err
	}
	resultPath := filepath.Join(a.dir, "worker-result.json")
	if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf(`You are the implementation worker for one approved slice set.

Configuration: %s
Checkpoint: %s
Primary repository: %s
Approved plan: %s
Approved tracker: %s
Phase: %s
Current controller instruction: %s

Read the configuration, checkpoint, approved artifacts and applicable AGENTS.md contracts. Recover live Git, canonical Forgejo and Acta state before changing anything. Follow the same rigorous implementation, validation, task commits, reporting and slice process as an ordinary interactive session. No separate reviewer role or subjective review pass is requested.

The maintainer has delegated Rebase + Fast-forward merges into dev for exactly this delivery set. The external controller owns all merge mutations. Do not merge PRs yourself. Do not promote main, release, tag, publish or broaden scope. Use dedicated slice worktrees; keep the primary checkout on dev. Complete PR-reference follow-up commits and pushes before reporting pr_ready. Do not wait for the maintainer to write Merged. Return continue if this turn ended before the current unit was ready; the controller will activate you again.

Use the exact configured title and this exact PR body form:
## Summary

One prose paragraph with the concrete delivered behavior.

## Changes

- Concrete delivered outcomes, one per item.
- [Slice N report](canonical full immutable report-introduction commit URL)

## Validation

- `+"`exact successful command`"+`
- `+"`git diff --check`"+`

No extra headings, prefatory/trailing text, empty list items, unsolicited scope disclaimers or nested lists. Required repository commands come first in canonical order, followed by focused commands, then git diff --check. The title is compared literally with the approved plan. Report links use the full introduction commit, not a branch or abbreviated hash.

Record successful checks through the controller instead of running the same final checks twice:
%s validate --config %s --worktree <slice-worktree> --command '<exact command>' --command '<next command>' --command 'git diff --check'
Use structured subprocess arguments or correct shell quoting for actual paths. Validation evidence belongs to the exact clean HEAD and the exact ordered command list. If the PR head changes afterward, evidence must be regenerated. The controller executes missing checks before merging.

Perform only the current instruction. In repair_gate, read gate_repair_policy and the last gate_repairs entry from the checkpoint. Its identity overrides the original slice identity for this repair: use exactly its branch, title, existing Slice N report and required command prefix. Diagnose the retained gate failure; correct only the approved set within allowed_paths. Do not invent another numbered slice, weaken checks, run the full gate, merge, or continue a successor. Prepare focused exact-HEAD validation and return pr_ready; the controller integrates and reruns the unchanged full gate. In prepare_gate, prepare fixtures and return ready_for_gate without running the full gate. In close, use the controller's exact gate evidence and close only approved documentation paths. Record required completed activity through Acta. Stop only owned fixtures after their checks finish. Core operational documents must not name an outer product or justify core capabilities by its needs.

Return the required JSON status and a concise factual message. A response is not proof of completion: the controller verifies actual remote and repository state.`, a.path, filepath.Join(a.dir, "state.json"), a.config.Repo, a.config.Plan, a.config.Tracker, phase, task, executable, a.path)
	args := []string{"exec"}
	if a.state.Session != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--output-schema", schemaPath, "-o", resultPath, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`)
	if a.state.Session != "" {
		args = append(args, a.state.Session)
	}
	args = append(args, "-")
	child, err := executionCommand(context.Background(), a.config.Repo, "codex", args...)
	if err != nil {
		return err
	}
	child.Stdin = strings.NewReader(prompt)
	var diagnostic capture
	diagnostic.limit = 4096
	child.Stderr = &diagnostic
	stdout, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	a.printf(activityRunning, "Activating Codex: %s (turn %d).\n", phase, a.state.Turns)
	if err := child.Start(); err != nil {
		return err
	}
	var runtimeErr error
	if a.runtime != nil {
		a.runtime.Worker, runtimeErr = identifyProcess(child.Process.Pid)
		if errors.Is(runtimeErr, os.ErrNotExist) {
			runtimeErr = nil
		}
		if runtimeErr == nil {
			runtimeErr = a.saveRuntime()
		}
	}
	if a.activity != nil {
		label := fmt.Sprintf("Slice %d/%d · %s · turn %d", min(a.state.Next+1, len(a.config.Slices)), len(a.config.Slices), phase, a.state.Turns)
		if a.activeGateRepair() != nil {
			label = fmt.Sprintf("Final-gate repair %d/%d · %s · turn %d", len(a.state.GateRepairs), a.state.GateRepairPolicy.MaxRepairs, phase, a.state.Turns)
		}
		a.activity.begin(label, "worker.started")
		defer a.activity.end()
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var streamErr error
	var reported *executionFailure
	failed := false
	for scanner.Scan() {
		if a.activity != nil {
			a.activity.event(scanner.Bytes())
		}
		var event workerEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type == "thread.started" && event.ThreadID != "" {
			if a.state.Session != "" && a.state.Session != event.ThreadID {
				streamErr = errors.New("Codex resumed an unexpected conversation")
			} else {
				a.state.Session = event.ThreadID
				if err := a.save(); err != nil {
					streamErr = err
				}
			}
		}
		// Protocol errors are execution input, independent of optional activity rendering.
		if event.Type == "turn.failed" || event.Type == "error" && !failed {
			reported = event.failure()
			failed = event.Type == "turn.failed"
		}
	}
	if err := scanner.Err(); err != nil {
		streamErr = err
		_, _ = io.Copy(io.Discard, stdout)
	}
	// Wait without a controller deadline: never interrupt an active build or validation.
	waitErr := child.Wait()
	if a.runtime != nil {
		a.runtime.Worker = processIdentity{}
		runtimeErr = errors.Join(runtimeErr, a.saveRuntime())
	}
	if runtimeErr != nil {
		return runtimeErr
	}
	if streamErr != nil {
		return streamErr
	}
	if waitErr != nil || failed {
		if reported != nil {
			return reported
		}
		failure := classifyAccess("worker", fmt.Errorf("%w: %s", waitErr, diagnostic.b.String()), 1)
		if failure.Blocker.Class == "authentication" || failure.Blocker.Class == "transient" {
			return failure
		}
		return contextFailure("worker", "execution", "Worker exited unsuccessfully; inspect the retained conversation and host Codex diagnostics", 1)
	}
	if a.state.Session == "" {
		return errors.New("Codex supplied no persistent conversation ID; refusing to start another unrelated worker")
	}
	a.reply = workerResult{}
	a.replyPhase = phase
	if err := readJSON(resultPath, &a.reply); err != nil {
		// A malformed final response is not a completion signal. Resume the same conversation.
		a.reply = workerResult{Status: "continue", Message: "Previous response did not satisfy the required JSON schema"}
	}
	switch a.reply.Status {
	case "continue", "pr_ready", "ready_for_gate", "closed", "blocked":
	default:
		a.reply.Status = "continue"
	}
	if len(a.reply.Message) > 1000 {
		a.reply.Message = a.reply.Message[:1000]
	}
	a.printf(activityRunning, "Codex returned %s. Checking actual state.\n", a.reply.Status)
	return nil
}
