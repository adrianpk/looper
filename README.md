# Looper

Looper keeps an approved set of development slices moving through `dev`.
Codex does the implementation work; Looper checks the published PR, runs or
reuses validation for its exact HEAD, merges with **Rebase + Fast-forward**, and
continues with the next slice. A checkpoint preserves progress and the worker's
conversation so an interrupted controller can resume the same delivery.

Use it when an agent works directly with maintainer steering and has permission
to execute the approved set and merge its PRs. When an existing automation tool
owns delivery, use that tool's execution and recovery workflow. Starting another
controller for the same work would create competing owners.

Completion means more than a successful worker response: every slice must be
merged in the canonical repository and integrated into `origin/dev`, the full
gate must pass on one clean candidate, and the approved operational documents
must be closed. `run` and `resume` carry out those delegated merges. Approval of
a plan and merge authority must already exist before starting them.

This README describes the CLI and delivery contract. Start with `looper --help`
for a capability reference that agents can read without a configuration.

## Contents

- [Where Looper fits](#where-looper-fits)
- [Agent skills](#agent-skills)
- [Quick start](#quick-start)
- [Requirements and installation](#requirements-and-installation)
- [Operational structure](#operational-structure)
- [Configuration](#configuration)
- [Project selection and concurrent work](#project-selection-and-concurrent-work)
- [Run, resume and watch](#run-resume-and-watch)
- [Streaming output and jq](#streaming-output-and-jq)
- [Activity and colors](#activity-and-colors)
- [Command reference](#command-reference)
- [PR and validation contract](#pr-and-validation-contract)
- [Recovery and blockers](#recovery-and-blockers)
- [Files and compatibility](#files-and-compatibility)
- [Development checks](#development-checks)

## Where Looper fits

Looper is a lightweight, CLI-focused execution alternative that can work with
development artifacts prepared for [KAL](https://forge.adrianpk.com/adrianpk/kal)
and [Chryso (CHR)](https://forge.adrianpk.com/adrianpk/chr), provided their plan
and tracker follow [Looper's contract](#operational-structure). It has served as
an occasional replacement for their execution workflows in specific contexts.

Looper has no GUI. Its optional agent skills help the harness clarify work,
write specifications, and prepare plans and tickets. The harness obtains
approval, then uses Looper to execute, inspect and resume the approved delivery.

For use cases that need more ergonomic operator tools or the complete workflow
from initial intent through planning and execution, see KAL and Chryso. KAL
provides guided task preparation and Telegram control. Chryso provides a TUI
and an optional local Web UI for managing project artifacts and execution.

## Agent skills

The repository includes two optional skills under [`skills/`](skills/):

| Skill | Purpose |
| --- | --- |
| [looper-prepare](skills/looper-prepare/SKILL.md) | Turn a request or existing artifacts into a spec, plan, tracker, and linked tickets. Includes optional dumps and clarification quizzes. |
| [looper-operate](skills/looper-operate/SKILL.md) | Inspect, launch, watch, and recover approved deliveries while preserving their execution state. |

They supplement the CLI. The harness conducts the conversation and writes the
documents using the bundled instructions and templates. You can start at any
stage; a clear request needs no dump or quiz. Existing tickets under
`ops/<namespace>/ticket/` are reused and linked to their owning slices,
regardless of who or what created them. Tickets do not need to come from a
skill. Execution keeps the same explicit approved plan and tracker contract.

Each skill is a self-contained [Agent Skills](https://agentskills.io/specification)
folder. Copy the complete folders, including references and templates, into
the directory your harness discovers. Common locations are:

| Harness | User skills | Project skills |
| --- | --- | --- |
| [Codex](https://learn.chatgpt.com/docs/build-skills) | `~/.agents/skills/` | `.agents/skills/` |
| [Claude Code](https://code.claude.com/docs/en/skills) | `~/.claude/skills/` | `.claude/skills/` |

For example, from a Looper clone, choose your harness's destination:

```sh
skills_root="$HOME/.agents/skills"
mkdir -p "$skills_root"
cp -R -i skills/looper-prepare skills/looper-operate "$skills_root/"
```

For another harness, use its skill import mechanism or skills directory. Review
an existing installation before replacing it, and follow the harness's reload
procedure. You can also ask the harness to read a skill's `SKILL.md` by path.
Copying skills does not install or upgrade the Looper executable.

Example requests to the harness:

- "Use looper-prepare to turn this request into a spec, plan, tracker, and tickets for review."
- "Use looper-prepare to run a clarification quiz before writing the spec."
- "Use looper-prepare to plan delivery from these existing tickets."
- "Use looper-operate to inspect this project's loop and explain its blocker."

## Quick start

Prepare and commit an approved plan and tracker on `dev`, then push them to
`origin/dev`. Keep the primary checkout clean. The [operational examples](#operational-structure)
show the Markdown format Looper expects. Replace the paths and gate below with
those of the actual project:

```sh
looper init \
  --repo /path/to/app \
  --plan ops/default/plan/account-access.md \
  --tracker ops/default/tracker/account-access.md \
  --gate 'scripts/check-all.sh' \
  --out "$HOME/.local/state/looper/account-access.json"
```

`init` creates and registers the configuration outside the product repository.
It does not start a worker or submit a merge. From the product checkout:

```sh
cd /path/to/app
looper status --json | jq .
looper resume --detach --activity --json | jq .
looper --watch
```

The detached controller keeps running when its caller or viewer disconnects.
Its default output is JSONL. To run in the foreground and pretty-print events
as they arrive:

```sh
looper run --activity | jq --unbuffered .
```

The preferred defaults are:

| Setting | Default | Override |
| --- | --- | --- |
| Project | Current Git project, including linked worktrees | `--project`, `--run` or `--config` |
| Detached backend | `auto`: **Herdr > Zellij > tmux** | `resume --detach --backend NAME` |
| Controller stream | `jsonl` | `--output human` |
| Human colors | `always`, including SSH and redirected output | `--color auto` or `--color never` |
| Worker activity | Off | `--activity`, then runtime `activity` controls |
| Execution | Foreground | `resume --detach` |

JSON, JSONL, help, raw checkpoints and printed view commands remain uncolored.
The color preference applies to human output. An active controller keeps its
existing backend and stream format.

## Requirements and installation

Looper is a Linux CLI written in Go, with no third-party Go dependencies.
Building requires **Go 1.27.1** and `make`. Executing a delivery requires:

| Dependency | Purpose |
| --- | --- |
| Git | Checkouts, dedicated slice worktrees, ancestry, clean candidates and integration |
| Codex CLI | Implementation worker, with working local authentication and configuration |
| `tea` | Authenticated access to the canonical Forgejo repository and its merge API |
| Acta | Record confirmed merge activity and the worker's required activity |
| `sh` | Execute configured validation and gate commands |
| Herdr, Zellij or tmux | At least one is needed for `resume --detach` and its native view |
| `jq` | Optional; format, filter or consume structured output |

The target repository needs a canonical `origin`, a `dev` branch and Forgejo
support for Rebase + Fast-forward. The worker must be able to commit, push and
create PRs; the controller's forge credentials must permit the approved merges.
The product can use any stack: its validation commands and full gate are
configured shell commands.

Build and check from this source directory:

```sh
make check build
```

For a fresh user installation:

```sh
make install PREFIX="$HOME/.local"
command -v looper
looper version
looper --help
```

Ensure `$HOME/.local/bin` is on `PATH`. For a fresh system installation instead:

```sh
sudo make install
/usr/local/bin/looper version
```

`make install` copies `bin/looper`; it does not build or test it. `PREFIX`
defaults to `/usr/local`, and `DESTDIR` is supported for staging.

### Updating while a loop is active

An active controller may invoke its own executable for validation. Keep that
executable available at its original path. Build the next executable at a new
path, for example in an immutable user build directory:

```sh
mkdir -p "$HOME/.local/lib/looper/builds" "$HOME/.local/bin"
looper_build_dir=$(mktemp -d "$HOME/.local/lib/looper/builds/build.XXXXXX")
GOWORK=off go build -trimpath -o "$looper_build_dir/looper" .
"$looper_build_dir/looper" version
```

Use that full executable path for a new launch. A user installation can point
to it through a symlink:

```sh
ln -sfn "$looper_build_dir/looper" "$HOME/.local/bin/looper"
command -v looper
looper version
```

Replace the PATH entry this way only when it is already a symlink to an
immutable build, or when no active controller uses it as a regular executable.
Otherwise, keep using the new full path until the old controller finishes.
Keep earlier build directories until their controllers finish. Changing `PATH`
or a symlink does not migrate an active controller. See [compatibility](#files-and-compatibility)
for executions started under the old command name.

## Operational structure

A **delivery set** is an ordered list of slices for one approved outcome. Each
slice has an exact branch, PR title and report path. Looper reads those
identities from an approved plan and tracker; it does not infer new slices from
free-form task descriptions.

A practical layout inside the product repository is:

```text
app/
├── ops/
│   └── default/
│       ├── spec/
│       │   └── account-access.md
│       ├── plan/
│       │   └── account-access.md
│       ├── tracker/
│       │   └── account-access.md
│       ├── ticket/
│       │   ├── TEMPLATE.md
│       │   └── ready/
│       │       └── 20261008120000-invalid-credentials.md
│       └── report/
│           └── slices/
│               └── account-access/
│                   ├── slice-1-account-flow.md
│                   └── slice-2-account-recovery.md
└── scripts/
    └── check-all.sh
```

Keep slice worktrees outside the primary checkout, for example under
`/path/to/worktrees/account-flow`. Keep Looper's JSON configuration outside the
product repository, for example in `$HOME/.local/state/looper/`.

The `ops/default/` layout is a convention, not a hardcoded path. Another
namespace or layout works when its paths are configured explicitly. Looper does
not create the plan, tracker, reports, worktrees or gate script during `init`.
Repository instructions still define implementation, commits and reporting.

### Which documents must exist

| Document | Example location | Who reads it and when |
| --- | --- | --- |
| Agent instructions | `AGENTS.md` and any applicable nested instructions | The worker follows them before implementation; they define repository-specific commands and workflow |
| Approved plan | `ops/default/plan/account-access.md` | Required by `init` and execution; defines scope and the canonical slice table |
| Approved tracker | `ops/default/tracker/account-access.md` | Required by `init` and execution; repeats slice identities and records progress |
| Slice report | `ops/default/report/slices/account-access/slice-1-account-flow.md` | Created during implementation, linked by immutable commit in the PR, checked at completion |
| Specification | `ops/default/spec/account-access.md` | Optional unless required by the approved work; the worker reads explicit references for behavior and contracts |
| Development ticket | `ops/default/ticket/ready/20261008120000-invalid-credentials.md` | Optional supporting work item; the worker reads it when the approved slice or repository instructions reference it |
| Local configuration | `/path/to/local/account-access.json` | Looper reads the exact configured paths, identities, gate and budgets |
| Execution checkpoint | `/path/to/local/account-access.json.state/state.json` | Created by execution; Looper and the worker use it to recover progress |

Plan and tracker must exist before `init`. Reports need not exist yet, but their
paths must already be assigned in the slice table. Any referenced specification
or ticket needed to implement a slice should exist and be committed before
execution. Gate scripts and required test tooling must be available when used.

**Paths are explicit.** Looper does not scan `ops/` for the next plan, guess a
specification filename or select work from ticket directories. Configuration
selects plan and tracker; their tables select reports. Put links to supporting
documents in the approved plan and identify which slice owns each ticket. The
worker receives configuration, checkpoint, primary repository, plan, tracker
and current phase paths, then reads the approved artifacts and applicable
repository instructions.

### Development tickets

Use tickets for bounded repository work: a reproducible bug, small task,
cleanup, follow-up, research question or decision. These are development records,
not application runtime or customer support tickets. A ticket should describe
one independently reviewable outcome. Broader work belongs in a specification,
plan and tracker with ordered slices.

**Tickets are not an execution queue in the current CLI.** Looper has no
`--ticket` selector and does not parse ticket frontmatter or advance its status.
The agent maintains linked tickets according to repository rules. A standalone
ticket needs an approved slice entry, even a one-slice set, before Looper can
deliver it. A linked ticket does not replace plan/tracker approval or the
configured branch and PR title.

A useful ticket convention is:

```text
ops/<namespace>/ticket/
├── TEMPLATE.md
├── open/
├── ready/
├── in_progress/
├── reviewing/
├── blocked/
├── solved/
└── closed/
```

Use the repository's `TEMPLATE.md` when present. Otherwise, this format makes
the expected work explicit. The example belongs at
`ops/default/ticket/ready/20261008120000-invalid-credentials.md`:

```markdown
---
id: TKT-20261008120000
title: Report invalid account credentials
status: ready
kind: bug
severity: medium
priority: normal
scope: ui
tags: account-access, validation
source: user_report
reported_at: 2026-10-08T12:00:00Z
ready_at: 2026-10-08T12:05:00Z
commits: []
---

# Report invalid account credentials

## Summary

The account form does not explain why invalid credentials were rejected.

## Current Behavior

Submitting invalid credentials returns to the same form without feedback.

## Expected Behavior

Show the approved rejection message and leave the form ready for another attempt.

## Scope

Update the account form feedback and its focused regression coverage.

## Acceptance Criteria

- [ ] Invalid credentials show the approved rejection message.
- [ ] The user can correct the input and submit again.
- [ ] The focused regression check passes.

## Validation

- Run the repository's focused account-form regression command.
- Run required repository checks and `git diff --check`.

## References

- [Plan](../../plan/account-access.md), Slice 1.
- [Tracker](../../tracker/account-access.md).
```

Replace descriptive validation instructions with the actual commands before
handoff. Use the filename `YYYYMMDDHHMMSS-<slug>.md`, with a UTC timestamp that
matches the `TKT-YYYYMMDDHHMMSS` ID. Keep the filename stable when moving the
ticket, and keep frontmatter `status` equal to its containing directory.

| Metadata | Convention |
| --- | --- |
| `id`, `title` | Stable ticket ID and a short concrete outcome |
| `status` | `open`, `ready`, `in_progress`, `reviewing`, `blocked`, `solved`, `closed` |
| `kind` | `unclassified`, `bug`, `task`, `cleanup`, `follow_up`, `research`, `decision` |
| `severity` | `unclassified`, `low`, `medium`, `high`, `critical`; impact if unresolved |
| `priority` | `unclassified`, `low`, `normal`, `high`, `urgent`; scheduling urgency |
| `scope` | `unclassified`, `domain`, `api`, `persistence`, `ui`, `ops`, `docs`, `infra` |
| `tags` | Sparse comma-separated lowercase labels; empty when unknown |
| `source` | `chat`, `manual_test`, `review`, `trace`, `user_report`, `implementation` |
| `reported_at` | UTC capture time |
| `commits` | Related commit hashes; empty before implementation |

New captures start in `open/`; uncertain classification fields can be
`unclassified`. The normal implemented path is `open → ready → in_progress →
reviewing → solved`. Active work can move to `blocked` with a concrete blocker
and return to `open`, `ready` or `in_progress` after resolution. Use `closed`
for discarded, duplicate, obsolete, moved or answered work without implementation.

Record `ready_at`, `started_at`, `reviewed_at` and final `closed_at` as the
corresponding transitions happen. Add actual `branch`, `pr` and `commits` when
implementation exists. Only final `solved` or `closed` tickets have
`resolution`: `fixed`, `wont_do`, `duplicate`, `obsolete`, `moved` or `answered`.
Update the directory, frontmatter and references together.

In the plan, link the ticket under its owning slice, for example:

```markdown
## Slice 1 inputs

- [Specification](../spec/account-access.md).
- [TKT-20261008120000](../ticket/ready/20261008120000-invalid-credentials.md): account rejection feedback.
```

These links guide the worker; ticket ordering does not override the slice table.
Finish ticket transitions before the full gate, or explicitly include every
affected old/new path in `closure_paths` before execution if they must change
during final closure. Closure paths are exact paths, not directory globs.

### Supporting specification

Looper does not enforce a specification schema. Follow the repository's own
format when present. A small specification linked from the approved plan can
use this structure at `ops/default/spec/account-access.md`:

```markdown
# Account access

Status: accepted

## Problem

Describe the user problem this delivery must resolve.

## Behavior

Describe sign-in, rejection feedback and account recovery in observable terms.

## Contracts and Boundaries

Record affected interfaces, persistence rules and the approved scope boundary.

## Acceptance Criteria

State concrete examples that determine whether each behavior is delivered.

## Validation

Identify the required checks and the full gate for the integrated outcome.
```

This document supplies implementation context; it does not replace the approved
slice identities or grant execution authority. Use a `Spec:` Markdown link when
the specification must be included in the generated closure paths.

### Plan example

The metadata below illustrates a real approval record; replace it with the
actual maintainer approval. The full gate must be the command approved for this
set, with any required environment setup.

```markdown
# Account access

Status: approved
Approved: maintainer, 2026-10-08
Full gate: `scripts/check-all.sh`
Spec: [Account access](../spec/account-access.md)

Deliver account sign-in and account recovery through two ordered slices.

| Slice | Short name | Exact branch | Expected PR title | Report |
| --- | --- | --- | --- | --- |
| Slice 1 | Account flow | `feat/account-flow` | `feat(slice-1): deliver account flow` | `ops/default/report/slices/account-access/slice-1-account-flow.md` |
| Slice 2 | Account recovery | `feat/account-recovery` | `feat(slice-2): deliver account recovery` | `ops/default/report/slices/account-access/slice-2-account-recovery.md` |

## Slice 1

Implement sign-in and invalid-credential feedback from the accepted specification.
Read ticket `TKT-20261008120000` at the linked path under Slice 1 inputs.

## Slice 1 inputs

- [TKT-20261008120000](../ticket/ready/20261008120000-invalid-credentials.md).

## Slice 2

Implement account recovery from the accepted specification after Slice 1 is
integrated. Record focused validation for the recovery behavior.

## Validation

Run the repository's required checks and focused regression checks for each
slice. Run `scripts/check-all.sh` once on the integrated clean candidate.
```

### Tracker example

Use the same canonical identities in both documents. Other prose and progress
columns can differ.

```markdown
# Account access tracker

Status: approved
Approved: maintainer, 2026-10-08

| Slice | Short name | Exact branch | Expected PR title | Report |
| --- | --- | --- | --- | --- |
| Slice 1 | Account flow | `feat/account-flow` | `feat(slice-1): deliver account flow` | `ops/default/report/slices/account-access/slice-1-account-flow.md` |
| Slice 2 | Account recovery | `feat/account-recovery` | `feat(slice-2): deliver account recovery` | `ops/default/report/slices/account-access/slice-2-account-recovery.md` |

- [ ] Deliver Slice 1 and record its merge.
- [ ] Deliver Slice 2 and record its merge.
- [ ] Validate the integrated candidate and close the delivery set.
```

The parser requires a table whose first header is `Slice`. Required identity
columns are `Exact branch` or `Branch`, `Expected PR title`, and `Report`.
Rows must be `Slice 1`, `Slice 2`, and so on, with 1–100 consecutive slices.
Backticks around values are accepted. Paths must be clean, repository-relative
paths without `..` escapes. Branches cannot be `dev` or `main`.

Both documents need a nonempty `Approved:` metadata line. `init` checks the local
committed files; execution also checks their approval and identities in
`origin/dev`. A `Status: approved` line alone does not provide the required
approval metadata. The `Full gate:` prose is useful to readers, but `init`
takes the executable command from `--gate`.

### Report and closure

A report describes the delivered behavior, exact validation and canonical PR.
For example, the final report for the first slice could contain:

```markdown
# Slice 1: Account flow

Status: delivered
Delivery set: Account access
Plan: `ops/default/plan/account-access.md`
Tracker: `ops/default/tracker/account-access.md`
Branch: `feat/account-flow`
PR: https://forge.example.org/team/app/pulls/42

## Purpose

Deliver the approved account sign-in flow.

## Delivered Behavior

Account sign-in follows the approved flow and reports invalid credentials.

## Implementation Notes

Describe the main implementation decision here.

## Contracts Added or Changed

Describe affected interfaces or record that no contract changed.

## Files of Interest

List the few files needed to understand the change.

## Validation

- `make check` — passed for the recorded candidate.
- `git diff --check` — passed for the same candidate.

Record the tested commit and any material limits of the checks here.

## Risks and Follow-ups

None recorded.
```

Use `Status: delivered` only after confirming delivery. At final closure, both
plan and tracker must have `Status: completed`, retain their approval and slice
identities, and contain no unchecked `- [ ]` tasks. Every configured slice
report must have `Status: delivered`.

`init` allows closure changes to the plan, tracker and configured reports. It
also includes safe relative Markdown links on `Concern:`, `Model:`, `Spec:` or
`Specification:` metadata lines, resolved relative to the document containing
the link. Review the generated `closure_paths` before starting. Only these
paths may change after the full gate's validated candidate.

## Configuration

Create configuration with `init` rather than assembling forge metadata by hand:

```sh
looper init \
  --repo /path/to/app \
  --plan ops/default/plan/account-access.md \
  --tracker ops/default/tracker/account-access.md \
  --gate 'scripts/check-all.sh' \
  --out /path/to/local/account-access.json
```

The primary checkout must be clean on `dev`, with plan and tracker committed.
`--repo` names its checkout root. Plan and tracker may be absolute paths inside
that checkout or paths relative to it. `--out` must be outside the repository;
`init` refuses to overwrite an existing configuration.

A generated configuration has schema version **1**, independently of the CLI's
version. This example also pins a required validation prefix for each slice:

```json
{
  "version": 1,
  "repo": "/path/to/app",
  "origin": "https://forge.example.org/team/app.git",
  "forge": "https://forge.example.org",
  "repository": "team/app",
  "plan": "ops/default/plan/account-access.md",
  "tracker": "ops/default/tracker/account-access.md",
  "full_gate": "scripts/check-all.sh",
  "max_stalled_turns": 5,
  "max_worker_turns": 200,
  "slices": [
    {
      "number": 1,
      "branch": "feat/account-flow",
      "title": "feat(slice-1): deliver account flow",
      "report": "ops/default/report/slices/account-access/slice-1-account-flow.md",
      "required_commands": ["make check"]
    },
    {
      "number": 2,
      "branch": "feat/account-recovery",
      "title": "feat(slice-2): deliver account recovery",
      "report": "ops/default/report/slices/account-access/slice-2-account-recovery.md",
      "required_commands": ["make check"]
    }
  ],
  "closure_paths": [
    "ops/default/plan/account-access.md",
    "ops/default/tracker/account-access.md",
    "ops/default/report/slices/account-access/slice-1-account-flow.md",
    "ops/default/report/slices/account-access/slice-2-account-recovery.md",
    "ops/default/spec/account-access.md"
  ]
}
```

| Field | Meaning |
| --- | --- |
| `version` | Configuration schema; currently `1` |
| `repo` | Absolute primary checkout root, kept on `dev` |
| `origin` | Exact frozen `origin` URL; execution rejects a changed URL |
| `forge` | Canonical HTTPS Forgejo base URL, resolved by `init` through `tea` |
| `repository` | Canonical `owner/name` |
| `plan`, `tracker` | Approved repository-relative Markdown paths |
| `session` | Optional inactive Codex conversation UUID; omitted to create a new conversation |
| `full_gate` | Exact shell command for the integrated candidate |
| `max_stalled_turns` | Consecutive worker turns without observable progress; `init` sets `5` |
| `max_worker_turns` | Total worker turns across execution and recovery; `init` sets `200` |
| `slices` | Ordered slice identities; `number` starts at `1` |
| `slices[].branch`, `title`, `report` | Exact branch, PR title and repository-relative report path |
| `slices[].required_commands` | Optional exact ordered prefix of PR validation commands |
| `closure_paths` | Repository-relative paths allowed to change after the full gate |

The two budgets must be positive. Set them and any `required_commands` before
the first checkpoint. `init` does not derive required commands from plan prose.
The configuration hash covers decoded configuration content; changing that
content after checkpoint creation is rejected. Changing approval or budgets
requires deliberate reconfiguration at a new configuration path and inspection
of the existing delivery state.

After editing a generated configuration before its first checkpoint, register
the final content again because its run ID changes:

```sh
looper register --config /path/to/local/account-access.json --json | jq .
```

An index for earlier content can remain visible as `identity_changed`. Use the
new returned `run_id` or `--config FILE` for an exact selection when old and new
indices make project-local selection ambiguous.

The first worker turn creates a dedicated Codex conversation unless `init
--session UUID` supplied an inactive one. Looper resumes its exact stored UUID,
never the most recent conversation. The worker uses local Codex configuration
with approval policy `never` and sandbox mode `danger-full-access`. Do not give
another process ownership of that conversation during execution.

## Project selection and concurrent work

`run`, `resume`, `status`, `list` and `watch` default to the current Git project.
Subdirectories, symlinks and linked worktrees share the identity of their Git
common directory. Independent clones with identical basenames remain separate
under implicit project selection. Inherited `GIT_DIR`, `GIT_WORK_TREE` and
`GIT_COMMON_DIR` do not redirect this detection.

| Selector | Meaning |
| --- | --- |
| No selector | Registered sets in the current Git project |
| `--set account-access` | Configuration filename without its extension, within the selected project |
| `--project app` | Exact configured repository basename; may be ambiguous |
| `--project /path/to/app` | Exact configured primary checkout path |
| `--run RUN_ID` | Exact stable identity returned by Looper, from any directory |
| `--config FILE` | Exact configuration, from any directory |
| `list --all` | Explicit discovery across every project |

`--run` and `--config` cannot be combined with another selector. `--set` may
combine with `--project` or the implicit current project. `list --all` cannot
combine with selectors. Outside a Git project, use an explicit selector or
`list --all`; Looper does not guess from the most recent execution.

For `run`, `resume` and `status`, automatic selection prefers a live owner, then
unfinished sets, then completed history. Multiple matches at the chosen level
return `selection_required` with candidates in the selected project. `list`
shows all matching local sets; `watch` selects only active executions.

```sh
looper list --json | jq '.runs[] | {set, status, run_id}'
looper status --set account-access --json | jq .
looper list --all --json | jq '.runs[] | {project, set, status}'
looper status --project /path/to/app --set account-access --json | jq .
looper status --run RUN_ID --json | jq .
```

There can be several registered sets for a project, but only **one controller
per Git repository** can execute at a time. Linked worktrees share that lock.
Different projects can execute concurrently with independent checkpoints,
conversations and terminal views. Concurrent resume requests for the same run
start at most one controller; further requests return `already_running`.

`init`, `run` and `resume` register configurations automatically. Register an
existing configuration stored elsewhere without executing it:

```sh
looper register --config /path/to/local/account-access.json --json | jq .
```

## Run, resume and watch

### Foreground execution

```sh
cd /path/to/app
looper run --activity
```

An exact configuration also works from outside the project:

```sh
looper run --config /path/to/local/account-access.json --activity
```

Both start or continue the same approved set. `run` without `--config` resolves
a registered project set; it does **not** automatically load `./looper.json`.
`resume` provides explicit recovery selectors and detached startup:

```sh
looper resume --set account-access --activity
looper resume --config /path/to/local/account-access.json --activity
```

A foreground process remains attached to its invoking terminal. Use detached
execution when it must survive an SSH disconnection.

### Persistent execution

```sh
looper resume --detach --activity --json | jq .
```

Automatic backend selection probes **Herdr, then Zellij, then tmux**. Select one
explicitly when required:

```sh
looper resume --detach --backend herdr --activity --json | jq .
looper resume --detach --backend zellij --activity --json | jq .
looper resume --detach --backend tmux --activity --json | jq .
```

These are alternative launch examples. An explicit backend never falls back.
Automatic selection skips unavailable backends during capability probes; once
selected, a launch failure does not start a different backend. Inspect status
before retrying a failed launch. `--backend` applies only to `resume --detach`;
it does not move an already running controller.

Herdr and Zellij reuse a named session derived from the run ID, with a fresh
controller pane for recovery. Existing user panes are preserved, and idle named
servers can remain after the controller exits. tmux uses a separate session per
launch on the compatible `slice-loop` socket.

The detached caller receives one startup result when `--json` is set. `started`
means the controller owns execution and passed prerequisites inside its actual
environment; `starting` means preflight or acknowledgement is pending.
`already_running` means an existing execution was preserved. None
of these means delivery is complete. Inspect `run.status` with `status --json`.

### Host execution context

Launchers, detached controllers, workers and validation resolve tools and host
references through the same context. Set `LOOPER_CONTEXT` to an absolute JSON
descriptor path, or use `$HOME/.config/looper/context.json`:

```json
{
  "version": 1,
  "environment": {
    "PATH": "/opt/tools/bin:/usr/local/bin:/usr/bin:/bin",
    "CODEX_HOME": "/home/operator/.codex",
    "SSH_AUTH_SOCK": "/run/user/1000/ssh-agent.socket"
  },
  "required_executables": ["go", "make"]
}
```

Use real host paths; values are literal and do not expand shell variables.
Supported overrides are `HOME`, `PATH`, `CODEX_HOME`, `XDG_CONFIG_HOME`,
`XDG_STATE_HOME`, `XDG_CACHE_HOME`, `XDG_RUNTIME_DIR`, `SSH_AUTH_SOCK`,
`GIT_CONFIG_GLOBAL`, `GIT_CONFIG_SYSTEM`, `HERDR_CONFIG_PATH` and `NO_COLOR`.
Other environment keys are rejected. Keep credentials in the host SSH agent,
Tea configuration and provider store. Looper never saves an environment
snapshot or sends secret environment values in terminal command arguments.
Without a descriptor, tools inherit the calling process's environment, with
outer terminal identities and Git checkout overrides removed.

The detached controller resolves required executables and checks Git remote
access, an authenticated Forgejo identity, push permission, rebase support and
provider access before acknowledging startup. Git cannot prompt for credentials.
These checks repeat before worker turns and at set boundaries. Recognized
transient read failures get at most three attempts, each with a 15-second
deadline. Authentication, configuration, exhausted quota, failed validation
and uncertain ownership produce blockers; they do not trigger blind retries.
Pending merges retain their existing canonical reconciliation rules.

The default provider probe reads Codex account state and online ChatGPT rate
limits, without creating a conversation or using a worker turn. For API keys
or other providers, set `provider_probe` to an absolute host executable that
checks the configured provider. It takes no arguments; output is discarded.
Exit 0 confirms access, 75 means transient failure, 77 means authentication
failure, and 78 means missing configuration. The probe must keep credentials
host-owned. An account check does not establish every model's entitlement;
actual inference can still fail. Codex's [account and rate-limit API](https://learn.chatgpt.com/docs/app-server)
defines the default check.

For a service, configure the descriptor once and check it from the service's
own environment with `looper preflight --config FILE`. Passing `PATH` alone
does not establish SSH-agent or provider access. Missing or expired access
blocks before delivery begins. Repair the host references or authentication,
then resume the same config. Recovery also rejects a changed worker home so
it cannot silently move a saved conversation to a different store.

### Approved successors

List each independently approved successor explicitly:

```sh
looper resume --config /path/to/local/first.json \
  --then /path/to/local/second.json \
  --then /path/to/local/third.json --detach --activity --json
```

This pins a sequence of 2 to 32 unique configs for the same repository. Each
plan and tracker must retain its own canonical approval and slice identities.
The list cannot change during recovery. One controller holds the repository
lease across all sets. Integration, the configured full gate and operational
closure finish before it checks access and activates the successor.

Each set keeps its own checkpoint, conversation, pending merge and budgets.
The sequence saves a closure receipt before handoff. `status --json` exposes
the phase, current config and progress, completed sets, failing component and
recovery action. A missing supervisor with unfinished continuation is blocked,
including when the predecessor is already complete. The original sequence
entry owns the shared view; successors do not create extra active views.

After restoring access or resolving ownership, resume the original entry:

```sh
looper resume --config /path/to/local/first.json --detach --json
```

Recovery verifies retained closure, config pins and live ownership. Completed
integration is retained, and the successor resumes its existing conversation
and remaining budget. Direct execution of a reserved successor is blocked.

### Native view and copying

```sh
looper --watch
looper watch --set account-access
```

The alias and subcommand are equivalent. A unique active project loop opens
directly; multiple active matches require a local selector. An exact copyable
`view_command` is also returned by `status --json`:

```sh
looper status --json | jq -r '.run.view_command // empty'
looper --watch --run RUN_ID
```

| Backend | Client opened by watch |
| --- | --- |
| Herdr | Text view inside Herdr; native interactive session client otherwise |
| Zellij | Native read-only `watch` client |
| tmux | Native read-only attachment on the `slice-loop` socket |

Watching opens the existing view without starting a worker or advancing a
checkpoint. Inside a Herdr pane, the text view reads the recorded session and
pane once per second. It joins soft-wrapped JSONL events and preserves their
slice, phase and message. Ctrl+C closes only this viewer.

Outside Herdr, its native interactive client accepts native input. Selection,
copying, scrolling and detaching use the backend's own controls or the enclosing
terminal. Detaching the client preserves the controller.

`watch` needs recorded detached terminal metadata. It cannot create a view for
a foreground execution in another terminal. An inactive loop returns
`not_running`; an active loop without a usable view returns `view_unavailable`.
Use `status --json` to inspect metadata; `watch --json` is rejected.

tmux's read-only attachment restricts keyboard commands, including entry into
copy mode. To use native keyboard copy mode, attach interactively to the exact
session returned in `.run.terminal.session`:

```sh
tmux -L slice-loop attach-session -t '=SESSION'
```

Replace `SESSION` while retaining the leading `=` for exact session matching.
With default tmux bindings, **Ctrl+B, then [** enters copy mode and **Ctrl+B,
then D** detaches. Actual bindings and clipboard behavior follow the installed
tmux configuration.

## Streaming output and jq

### Live events

`run` and foreground `resume` write one JSON object per line to stdout by
default, immediately as each event occurs. This is **JSONL**, not one final
report. `--activity` adds filtered worker/tool events and heartbeats to the
controller's phase records.

When the controller exits, `loop.completed` reports `Loop completed successfully.`
or `loop.failed` reports `Loop failed: REASON`. This final event
covers the whole loop, including all explicitly listed successor sets.
The existing `controller.completed` and `controller.error` events describe
individual sets. The same final messages appear with `--output human`, even
without `--activity`. In detached mode they appear in the controller pane;
the launching terminal receives only startup status. The backend's pane-exit
policy determines whether its output remains available after the controller exits.

Pretty-print continuously:

```sh
looper run --activity | jq --unbuffered .
```

Keep compact JSONL, or show readable messages with their event type:

```sh
looper run --activity | jq --unbuffered -c .
looper run --activity | jq --unbuffered -r '[.time, .level, .type, .message] | @tsv'
```

Filter warnings and errors, or worker and tool events:

```sh
looper run --activity | jq --unbuffered 'select(.level == "warning" or .level == "error")'
looper run --activity | jq --unbuffered 'select(.type | startswith("worker.") or startswith("tool."))'
```

Save the original stream while displaying it:

```sh
set -o pipefail
looper run --activity | tee /path/to/local/account-access.jsonl | jq --unbuffered .
```

Each pipeline above starts or resumes execution; they are alternatives, not
commands to run together for the same project. `--unbuffered` makes `jq` flush
its output after each object. Avoid `jq -s` for live viewing: slurping waits for
EOF. Use `jq -C` when jq itself should color its formatted output.

Keep stderr separate from JSONL. Startup failures and final diagnostics go to
stderr, while a started controller also emits `controller.error` on execution
failure when stdout is writable. `2>&1` would mix plain diagnostics into the
JSON stream. In Bash or Zsh, `set -o pipefail` preserves pipeline failure when
Looper fails and jq succeeds.

Closing the reader is not a cancellation command. Looper retains ownership and
lets its authorized work finish, then returns an output failure. A viewer such
as `head` can therefore disappear while execution continues. Use `status` to
inspect the owner.

### Event schema

```json
{"schema_version":1,"time":"2026-10-08T15:00:00Z","type":"worker.message","level":"info","run_id":"run-example","label":"Worker implement","message":"Preparing the approved account flow."}
```

| Field | Meaning |
| --- | --- |
| `schema_version` | Event schema; currently `1` |
| `time` | UTC RFC3339 timestamp, with fractional seconds when available |
| `type` | Event category listed below |
| `level` | `info`, `warning`, `error` or `debug` |
| `run_id` | Stable identity of this delivery configuration |
| `label` | Optional activity context |
| `message` | Human-readable text; embedded newlines and quotes are JSON-escaped |

Event types are `controller.started`, `controller`, `controller.completed`,
`controller.error`, `worker.started`, `worker.message`, `worker.error`,
`tool.started`, `tool.completed`, `tool.failed`, `validation.started`,
`validation.check`, `heartbeat` and `activity.control`. Concurrent events retain
complete line framing. JSONL contains no ANSI styling.

### Human log lines

```sh
looper run --activity --output human
looper run --activity --output human --color never
looper resume --detach --activity --output human --json | jq .
```

For a detached launch, `--output` selects the controller pane's stream format;
`--json` separately selects the caller's single startup response. Stream format
is fixed at controller startup. Runtime activity controls can change visibility
and human colors, but not switch between JSONL and human output.

### Inspection results

`--json` on `list`, `status`, `register` and detached `resume` produces exactly
one uncolored response object, including errors. The envelope has
`schema_version: 1`, `status`, and the applicable `run`, `runs` or `error` fields.
An empty discovery result explicitly contains `runs: []`.

```sh
looper status --json | jq '.run | {status, slice, completed_slices, total_slices, blocker}'
looper list --json | jq -r '.runs[] | [.project, .set, .status, .run_id] | @tsv'
looper status --json | jq '.run | {worker_turns, max_worker_turns, stalled_turns, max_stalled_turns}'
```

A top-level `ok` means inspection succeeded, even when `run.status` is
`blocked`. Inspect that status and `run.blocker`. The run object exposes:

| Fields | Meaning |
| --- | --- |
| `run_id`, `config`, `project`, `set`, `repo` | Exact run identity and its configured project |
| `status`, `checkpoint_status` | Observed ownership-aware state and saved checkpoint state |
| `slice` | Current one-based slice, omitted when none remains |
| `completed_slices`, `total_slices` | Recorded integrated slices and configured total |
| `worker_turns`, `max_worker_turns` | Used and allowed total worker turns |
| `stalled_turns`, `max_stalled_turns` | Consecutive non-progress turns and their limit |
| `worker_session` | Persistent Codex conversation UUID, when known |
| `terminal`, `view_command` | Recorded native backend metadata and exact watch command |
| `tmux_session` | Legacy tmux session field, when applicable |
| `blocker` | Recovery issue, when present |

Optional metadata can be absent. A blocker contains `code`, `message`,
`retryable` and optionally `next_action`. An action with `type: "command"`
provides an `argv` array suitable for a structured subprocess call. Other
action types provide an inspection or resolution `target`; they are not shell
commands. For example:

```json
{
  "code": "worker_not_idle",
  "message": "The worker conversation still has an active daemon turn",
  "retryable": false,
  "next_action": {
    "type": "inspect_worker",
    "target": "11111111-1111-4111-8111-111111111111"
  }
}
```

`retryable: true` permits another attempt within existing authority; it does not
prove that the original failure was fixed. Agents should inspect the blocker
and live state before resuming.

## Activity and colors

`--activity` enables worker visibility at startup. From another terminal, query
or change it without restarting the worker:

```sh
looper activity --config /path/to/local/account-access.json
looper activity --config /path/to/local/account-access.json --on
looper activity --config /path/to/local/account-access.json --off
looper activity --config /path/to/local/account-access.json --toggle
looper activity --config /path/to/local/account-access.json --color never
```

Only one of `--on`, `--off` and `--toggle` may be selected. `--color` can combine
with one of them. Use the running controller's configuration path. Enabling
activity shows the current summary and future events, without replaying a
transcript. Visibility lasts for that controller execution and is not saved in
the configuration or checkpoint.

Worker activity includes public messages, tool names or command categories,
completion status and exit codes. Messages are reduced to a bounded paragraph;
code is omitted and common credential patterns are masked. Raw worker tool
arguments, tool output, diffs and reasoning are not forwarded. Controller phase
messages can include exact validation commands; captured check diagnostics stay
in the local evidence log.

During active worker or validation work, a heartbeat every **15 seconds** shows
the last event's age and current summary. Five minutes without events produces
a silence notice. Silence alone does not prove a blocker, and there is no
controller deadline that kills an active build or gate.

| Color mode | Human output behavior |
| --- | --- |
| `always` | Default; ANSI colors even over SSH, with `NO_COLOR`, or when redirected |
| `auto` | Color only on a terminal; honor nonempty `NO_COLOR` and `TERM=dumb` |
| `never` | Plain text |

Worker messages are blue, running tools and controls cyan, success green,
silence notices yellow, failures red and routine heartbeats dim. Labels remain
readable without color. Printed view commands occupy separate uncolored lines.
A runtime color change affects future output; it does not recolor earlier text.

Runtime control uses a same-user Linux abstract Unix socket and creates no
control files. Activity toggling needs a controller from version 0.2.0 or later;
runtime color changes need 0.4.0 or later. Unsupported older controllers return
an error and keep running.

## Command reference

Flags follow the command. Commands do not take positional arguments. `looper`,
`looper help`, `looper --help`, `looper -h` and `looper COMMAND --help` print the
same plain capability reference and exit successfully without loading a
configuration. `looper version` prints the CLI version.

### init

```text
looper init --repo DIR --plan PATH --tracker PATH --gate COMMAND
            [--session UUID] [--out FILE]
```

| Flag | Meaning and default |
| --- | --- |
| `--repo DIR` | Required primary Git checkout root, clean on `dev` |
| `--plan PATH` | Required approved, committed plan inside the checkout |
| `--tracker PATH` | Required approved, committed tracker inside the checkout |
| `--gate COMMAND` | Required exact full-gate shell command |
| `--session UUID` | Optional inactive worker conversation |
| `--out FILE` | Output configuration; default `looper.json` in the invoking directory, which must be outside the product repository |

Creates configuration and discovery registration. It does not execute checks,
workers or merges.

### run and resume

```text
looper run [--config FILE | --set NAME] [--activity]
           [--output jsonl|human] [--color auto|always|never]
looper resume [--run ID | --project NAME | --config FILE] [--set NAME]
              [--then FILE ...]
              [--detach] [--backend auto|herdr|zellij|tmux] [--activity]
              [--output jsonl|human] [--color auto|always|never] [--json]
```

| Flag | Meaning and default |
| --- | --- |
| `--config`, `--run`, `--project`, `--set` | Select the exact approved set; see project selection |
| `--detach` | Start a persistent native backend execution; otherwise foreground |
| `--then FILE` | Pin an explicitly approved successor; repeat in execution order |
| `--backend NAME` | Detached selection only; default `auto`, preferring Herdr, Zellij, tmux |
| `--activity` | Enable filtered worker visibility; default off |
| `--output FORMAT` | Controller stream; default `jsonl`, alternative `human` |
| `--color MODE` | Human colors; default `always`, alternatives `auto`, `never` |
| `--json` | One startup result; requires `resume --detach` |

Use `resume` for detached startup and explicit recovery selectors. The direct
`run --config FILE` form is foreground and does not accept `--detach` or
`--json`. Repeating execution preserves completed slices, merge intent, gate
evidence, conversation and budgets. A completed checkpoint is reverified
without additional worker turns or merge submissions.

### list, status and register

```text
looper list [--project NAME] [--set NAME] [--all] [--json]
            [--color auto|always|never]
looper status [--run ID | --project NAME | --config FILE] [--set NAME]
              [--json] [--color auto|always|never]
looper register --config FILE [--json]
```

`list` returns the current project's sets unless `--project` or `--all` changes
its scope. `status` inspects one selected run, including process ownership and
blockers. Both are read-only. `register` writes a discovery index without
executing work. `--json` selects the structured response; human output uses
`--color always` by default.

For compatibility, **`status --config FILE` without `--json` returns the raw
saved checkpoint**, not the ownership-aware run envelope. Use `status --config
FILE --json` for recovery decisions. The raw checkpoint's `next_slice_index` is
zero-based; the envelope's optional `slice` is one-based.

### watch

```text
looper watch [--run ID | --project NAME | --config FILE] [--set NAME]
looper --watch [--run ID | --project NAME | --config FILE] [--set NAME]
```

Opens an active run's recorded native view. It does not start, resume or migrate
work. `--json` is unavailable; use `status --json` for metadata.

### activity

```text
looper activity --config FILE [--on | --off | --toggle]
                [--color auto|always|never]
```

Omit the mode flag to query visibility. An explicit `--color` changes future
human output in the running controller. These controls preserve execution and
checkpoint state.

### check and validate

```text
looper check --config FILE --slice N --pr N [--color auto|always|never]
looper validate --config FILE --worktree DIR --command COMMAND
                [--command COMMAND ...] [--color auto|always|never]
```

`check` verifies the live canonical PR contract for a one-based configured
slice and PR number. It does not run checks or merge. `validate` executes
commands in the specified clean worktree, in their supplied order, and saves
successful evidence bound to its exact HEAD. Repeat `--command` for each check;
use shell quoting that preserves the exact command string. The worktree must
belong to the configured Git repository.

These direct utility commands, `activity` and `recover` default to
`./looper.json` when `--config` is omitted. An explicit path avoids dependence
on the invoking directory.

### recover

```text
looper recover --config FILE --retry-unmerged
```

`--retry-unmerged` explicitly confirms that a previously uncertain merge did
not apply. Use it only after independent canonical PR inspection. It clears the
pending intent when the PR is still open at the same HEAD; a confirmed merge is
reconciled instead. It does not itself start the next worker or submit a merge.

## PR and validation contract

Looper checks the canonical PR and actual Git state before accepting a slice.
Its title, branch and report path must match configuration literally. The PR
must target `dev` in the configured repository, be non-draft and use the
canonical repository rather than a fork.

The body has exactly this shape:

```markdown
## Summary

Account sign-in follows the approved flow and reports invalid credentials.

## Changes

- Add the approved account sign-in behavior and focused regression coverage.
- [Slice 1 report](https://forge.example.org/team/app/src/commit/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/ops/default/report/slices/account-access/slice-1-account-flow.md)

## Validation

- `make check`
- `git diff --check`
```

Replace the illustrative SHA with the full **40-character report-introduction
commit**, which must be an ancestor of the PR HEAD. Looper verifies that the
report was introduced in that commit; a branch URL, abbreviated hash or arbitrary
recent commit is insufficient.

Body rules are literal:

- Headings are only `## Summary`, `## Changes`, `## Validation`, in that order.
- Summary is one prose paragraph. Changes has at least two nonempty single-line
  `- ` items; its last item is the exact `[Slice N report](URL)` link.
- Validation contains only unique backtick-wrapped command items. Any configured
  `required_commands` must be the exact prefix; `git diff --check` must be last.
- Use one blank line after headings and between sections. No CRLF, extra
  headings, nested lists, trailing whitespace or extra blank lines. One final
  newline is accepted. Bodies are limited to 128 KiB.

The worker can record successful final checks through Looper instead of running
them twice:

```sh
looper validate --config /path/to/local/account-access.json \
  --worktree /path/to/worktrees/account-flow \
  --command 'make check' \
  --command 'git diff --check'
looper check --config /path/to/local/account-access.json --slice 1 --pr 42
```

Commands execute through `sh -ec` in the clean slice worktree. Evidence is
reusable only for the exact HEAD and exact ordered command list. A changed or
dirty candidate invalidates it. Without matching evidence, the controller runs
the published PR checks before merging. Validation accepts 1–100 commands;
each must be a nonempty single line of at most 8192 bytes.

Before merge, the tested PR HEAD must already include current `dev`. The
controller rechecks live title, body, base, head and mergeability, then submits
Forgejo's rebase merge with the exact `head_commit_id` and `force_merge: false`.
Forgejo's own protections still apply. Title/body checks and merge are separate
API operations, so avoid editing an active handoff concurrently.

After each confirmed merge, Looper fast-forwards the clean primary `dev`
checkout and records activity through Acta. An already merged canonical PR is
reconciled rather than submitted again.

After all slices, the worker prepares full-gate prerequisites and leaves primary
`dev` clean and pushed. The controller runs the configured full gate once on
that immutable candidate and stores evidence. The worker then closes approved
documents, changing only `closure_paths`. Completion verifies:

- Every configured PR is canonically merged and its recorded merge commit is
  present in `origin/dev`.
- Primary `dev` is clean and synchronized with `origin/dev`.
- Successful gate evidence belongs to an ancestor of final `origin/dev`, and
  all subsequent changes are confined to `closure_paths`.
- Reports are delivered; plan and tracker are completed with no unchecked tasks.

A failing full gate blocks closure. Additional implementation requiring a new
slice identity needs a newly approved configuration; Looper does not invent
replacement slices. Delivery ends at `dev`: main integration, tagging, release,
publication and deployment require their own workflow and authority.

## Recovery and blockers

The normal recovery entrypoint is project-local:

```sh
cd /path/to/app
looper status --json | jq .
looper resume --detach --activity --json | jq .
```

Read the blocker before attempting resume. Recovery reacquires the repository
lock, reloads the checkpoint and verifies live Git, canonical PR and worker
ownership. It preserves the exact worker conversation, counters, integrated
slices and evidence. A worker turn is reserved before launch, so a crash does
not refund its budget.

| Observed `run.status` | Meaning |
| --- | --- |
| `ready` | Valid configuration/checkpoint ready for execution |
| `starting` | Detached launch is active; ownership acknowledgement is pending |
| `running` | A controller currently holds the run lock |
| `interrupted` | Saved state was running, but its controller is no longer active |
| `blocked` | A failure, ownership issue or budget requires attention |
| `complete` | Checkpoint records completion; executing it again verifies the result |
| `invalid` | Configuration, registry or runtime metadata cannot be safely inspected |

`status` is a local checkpoint/ownership inspection, not an exhaustive forge
or completion audit. `resume` performs the live execution checks. A saved
`running` checkpoint by itself is not proof of a live process.

Common issues and the required response:

| Code | Response |
| --- | --- |
| `project_required` | Enter the product worktree or provide an explicit selector |
| `not_found` | Inspect local discovery or register the exact existing configuration |
| `selection_required` | Choose a returned local `--set`, exact `--run` or configuration |
| `repository_busy` | Inspect the other repository owner; wait for its work to finish |
| `worker_not_idle` | Inspect the active/orphaned worker, daemon turn or validation; do not duplicate it |
| `ownership_unknown`, `invalid_runtime` | Resolve unreadable or inconsistent ownership metadata |
| `invalid_checkpoint`, `invalid_registry_entry`, `identity_changed` | Inspect the exact configuration, state or index before registration/reconfiguration |
| `previous_failure` | Resolve the recorded failure, then resume the same checkpoint |
| `progress_required` | Resolve the workspace blocker; resume requires observable progress |
| `turn_limit` | Review the exhausted total budget and deliberately reconfigure approved work |
| `pending_merge` | Resume may reconcile a canonically confirmed merge |
| `merge_confirmation_required` | Inspect the canonical PR before explicitly clearing an uncertain intent |
| `backend_unavailable` | Install or select a supported backend for a new detached launch |
| `launch_failed` | Inspect status and backend state before retrying startup |
| `not_running`, `view_unavailable` | Inspect status; watch requires an active detached view |

Looper does not kill active or orphaned workers, gate processes or repository
owners to make recovery possible. Budget and ownership failures must be
resolved explicitly. A stalled-turn limit is lifted only by observable state
progress; repeatedly relaunching does not reset it. Do not delete checkpoints
or locks to bypass recovery checks.

### Uncertain merge outcome

Merge intent is saved **before** submission. If the response is lost, Looper
queries Forgejo and verifies the canonical merge commit in `origin/dev`.
An unconfirmed intent is never blindly resubmitted.

After independently confirming that the submission did not apply:

```sh
looper recover --config /path/to/local/account-access.json --retry-unmerged
looper resume --config /path/to/local/account-access.json --detach --json | jq .
```

If the canonical PR cannot be queried, restore forge access first. If its state
or HEAD differs from the pending intent, inspect that discrepancy rather than
clearing it. A confirmed merge is reconciled through the normal integration
checks. A crash between an Acta write and its checkpoint can duplicate an
activity record, but cannot submit a second merge.

### Exit codes

The agent inspection and recovery commands use these codes:

| Code | Meaning |
| --- | --- |
| `0` | Successful inspection, registration, startup or already-running result |
| `1` | Unexpected I/O/output error; direct execution and utility failures also use this code |
| `2` | Invalid arguments, missing project or no matching run |
| `3` | Selection required; inspect returned candidates |
| `4` | Recovery or view blocked; inspect `error` and `run.blocker` |
| `5` | Detached backend/startup failure; inspect status before retrying |

A successful `status` call can still report a blocked run. With `--json`, errors
are structured on stdout and the process returns a nonzero code. Foreground
controller execution emits JSONL rather than a single `--json` response.

## Files and compatibility

Looper stores configuration separately from execution state:

```text
/path/to/local/account-access.json
/path/to/local/account-access.json.state/
├── state.json
├── runtime.json
├── run.lock
├── launch.lock
├── launch.json
├── launch-ready.json
├── sequence.json
├── sequence-owner.json
├── validation.json
├── validate.lock
├── worker-schema.json
├── worker-result.json
└── checks/
    ├── <head>.json
    └── <head>.log
```

Files appear as their corresponding operations run. `state.json` is the atomic
checkpoint, including configuration hash, next slice, worker session, counters,
merge records, pending merge and gate evidence. Runtime metadata includes PID
and process start time, so PID reuse does not establish ownership. Check
diagnostics are bounded to 1 MiB per capture and kept beside HEAD-bound evidence.
State directories and files are created with private permissions.
`sequence.json` appears only on the sequence entry; reserved successors have
`sequence-owner.json`. These records contain config references, hashes,
completion receipts and blockers. Runtime metadata also pins the worker home.
Existing configuration hashes and checkpoints remain compatible.

The discovery root is `$XDG_STATE_HOME/looper`, or `$HOME/.local/state/looper`
when `XDG_STATE_HOME` is unset. Registration writes `runs/*.json` there.
When set, `XDG_STATE_HOME` must be an absolute path.
Configurations directly under the root are also discoverable. `list` and
`status` do not register configurations or create checkpoints. Discovery is
bounded to 1024 configuration/index files across supported roots.

A run ID binds the canonical configuration path and immutable decoded content.
Symlink aliases resolve to the same identity. Moving or replacing a
configuration changes its ID. Configuration and its adjacent checkpoint remain
authoritative; the registry is an index.

Existing configurations and indices under the historical `slice-loop` state
root remain discoverable with their original checkpoints and IDs. Duplicate
old/new indices are deduplicated, preferring the current root. Discovery does
not move or rewrite old state. The repository lock remains
`<git-common-dir>/slice-loop.lock`, and tmux keeps the `slice-loop` socket so
older controllers and Looper share ownership protection.

The old `slice-loop` executable can coexist until its active sessions finish.
Use `looper` for new executions, retain old executable paths while they are in
use, and remove the old installation only after those controllers have exited.
There is no separate watcher dependency: `looper --watch` opens the native view.

## Development checks

```sh
make test
make check
make build
```

`make test` runs uncached race-enabled Go tests. `make check` adds `go vet` and
a `gofmt` check. `make build` writes `bin/looper`. The Makefile sets `GOWORK=off`
so a parent Go workspace does not change this module's build.

Checks cover PR and slice-table contracts, real Git validation evidence,
checkpoint recovery, process/daemon ownership, scoped project discovery, native
view arguments, filtered activity, runtime controls and live JSONL framing.
Loop scenarios include failed checks, false worker completion, lost merge
responses, failed full gates and idempotent completion. Stream checks exercise
`jq --unbuffered`, concurrent events and ownership after a reader closes.

Integration fixtures simulate Codex and Forgejo against a real local Git remote.
Detached checks exercise installed supported backends in isolated sessions,
including JSONL and human output. They do not constitute a live autonomous
product delivery or a real canonical forge merge.
