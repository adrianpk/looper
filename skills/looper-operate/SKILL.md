---
name: looper-operate
description: Inspect, launch, watch, and recover explicitly approved Looper deliveries. Use for loop progress, detached startup, blockers, or sequential-set continuation while retaining controller ownership, checkpoints, budgets, and worker conversations.
---

# Operate Looper

Manage the requested delivery through the installed CLI. Start with
`looper version` and `looper --help`; use command help before relying on a
feature that may not exist in that version.

## Select and inspect

Run from the target repository or provide an explicit selector. Start with
`looper status --json` and, if needed, `looper list --json`. Defaults are
project-scoped. Use `list --all --json` only for requested global discovery.
Resolve ambiguity with a returned `--set`, exact `--run`, or `--config`.

Read `run.status`, its blocker, progress, and ownership even when the top-level
JSON status is `ok`. A saved checkpoint is not proof of a live controller;
local status is not a complete canonical merge or closure audit. In a sequence,
inspect the sequence phase and current set as well as predecessor completion.

A request to inspect or explain does not request a new worker. Keep active
controllers, workers, builds, validations, terminal sessions, and their
executables intact. Prepare new work separately from a repository already
owned by an execution.

## Start or continue authorized work

Before launch, verify actual approval and delegated execution and merge
authority for exactly the selected set. Existing tickets and documents are
valid regardless of their producer. Execution uses approved plan and tracker
identities and the supporting artifacts they link.

For a new configuration, `init` requires committed approved plan and tracker
paths, an exact full-gate command, and an output path outside the target
repository. Canonical execution checks approval again in `origin/dev`.
Inspect an existing config rather than replacing it during recovery.

When supported, use `looper preflight --config FILE` to inspect tool, Git,
Forgejo, and provider access. Host execution contexts contain location and
credential references; credentials remain host-owned. Do not copy secrets or
the full environment into configs, checkpoints, commands, or reports. A
caller-side check does not replace checks in the actual detached environment.

Use `looper resume --detach --activity --json` with the selected set when
authorized. `started` acknowledges owned startup after checks; `starting`
requires follow-up inspection. Neither means delivery is complete. An existing
controller can return `already_running`; inspect it instead of launching another.

For explicitly approved sequential sets, use supported `resume --config FIRST
--then SECOND --detach --json`, repeating `--then` in the approved order.
Each set has its own approval. Recovery resumes the original entry without
replacing the pinned list. One repository owner spans predecessor integration,
full validation, closure, and successor startup. Do not start queued successors
independently or substitute an auxiliary handoff process.

## Observe and recover

Use `looper --watch` or the selected run's view command for the existing
detached view. Inside Herdr, the text view preserves slice, phase, and message.
Closing the viewer leaves execution running. Do not diagnose a summarized
worker failure without inspecting the corresponding diagnostics.

For failures, read [Recovery](references/recovery.md). Resolve the recorded
prerequisite, then resume the same configuration under the existing authority.
Let Looper reconcile pending merges, ownership, budgets, conversations, and
retained progress; do not edit state to bypass a blocker.

## Report evidence

Report the selected project and set, current slice or sequence phase, retained
progress, failing component and reason, and the concrete recovery action.
Distinguish local checkpoint state, verified canonical integration, full-gate
evidence, and closure. Say which checks actually ran. Keep private identifiers
and credential details out of versioned artifacts.
