package main

import (
	"fmt"
	"os"
)

func printHelp() error {
	_, err := fmt.Fprintf(os.Stdout, `looper %s: complete explicitly approved slice sets through dev.

Usage:
  looper COMMAND [OPTIONS]
  looper --watch [--set NAME]
  looper --help
  looper COMMAND --help

Preferred defaults:
  backend: %s (herdr > zellij > tmux), for resume --detach.
  output: %s, one JSON object per line for run and resume controller streams.
  color: %s, for human-readable output, even with NO_COLOR=1 or over SSH.
  JSON, raw checkpoints, help and copyable view commands always remain plain.
  Use --color auto for terminal detection or --color never to disable styling.
  Activity starts off; --activity enables it. Use --detach for persistent recovery.
  Project: the current Git worktree, including subdirectories and linked worktrees.
  Only list --all searches every project. Explicit selectors override the current project.
  These preferences apply to new commands; an active loop keeps its backend and settings.

Commands:
  init      --repo DIR --plan PATH --tracker PATH --gate COMMAND [--session ID] [--out FILE]
            Create and register a local configuration outside the target repository.
  run       [--config FILE | --set NAME] [--activity] [--output jsonl|human] [--color auto|always|never]
            Run or resume that approved delivery set in the foreground.
            Without --config, resolve a registered set in the current project.
  activity  --config FILE [--on | --off | --toggle] [--color auto|always|never]
            Query or change runtime visibility and colors without restarting work.
  list      [--project NAME [--set NAME] | --all] [--json] [--color auto|always|never]
            List the current project's sets; --all explicitly includes other projects.
  register  --config FILE [--json]
            Add an existing configuration to discovery without starting it.
  status    [--run ID | --project NAME | --config FILE] [--set NAME] [--json] [--color auto|always|never]
            Inspect progress, ownership, blockers and view metadata.
            status --config FILE without --json returns the original raw checkpoint.
  resume    [--run ID | --project NAME | --config FILE] [--set NAME]
            [--detach] [--backend auto|herdr|zellij|tmux]
            [--then FILE ...]
            [--activity] [--output jsonl|human] [--color auto|always|never] [--json]
            Continue the exact checkpoint and worker conversation, preserving budgets.
            --backend belongs to --detach. A single --json result requires --detach.
            --then pins each approved successor in order; resume the original entry after interruption.
  preflight --config FILE
            Check executables, Git, Forgejo permissions and provider access without starting work.
  watch     [--run ID | --project NAME | --config FILE] [--set NAME]
            Open the current project's active loop. Also available as looper --watch.
            One active loop opens directly; several require a local --set NAME.
            Requires a detached terminal view; never starts or resumes work.
  check     --config FILE --slice N --pr N [--color auto|always|never]
            Inspect the canonical PR contract without running validation or merging.
  validate  --config FILE --worktree DIR --command COMMAND [--command COMMAND ...]
            [--color auto|always|never]
            Execute the exact ordered checks and save evidence for the clean HEAD.
  recover   --config FILE --retry-unmerged
            Clear an uncertain merge intent only after confirming it did not apply.
  version   Print the controller version.

Agent workflow using the preferred defaults:
  cd /path/to/project
  looper status --json
  looper resume --detach --activity --json
  looper --watch

  No session name or run_id is needed for a unique current-project execution.
  status/resume prefer the live owner, then unfinished sets, over completed history.
  Several local matches return selection_required with only this project's candidates.
  Use --set NAME; duplicate set names require an exact --run ID or --config FILE.
  Outside a Git worktree, use an explicit selector or list --all --json.
  looper list --json lists local sets; looper list --all --json is global discovery.
  Selection never falls back to another project or the most recent execution.
  An explicit --backend never falls back. auto skips unavailable backends during
  capability checks; a failure after selection never launches a different backend.
  Concurrent resume calls do not duplicate a controller; they return already_running.
  Active or orphaned workers, validations and unresolved ownership block recovery.

Execution context and continuation:
  LOOPER_CONTEXT selects a host-owned JSON descriptor; default: $HOME/.config/looper/context.json.
  It holds location references and an optional provider_probe executable, never credentials.
  The detached controller acknowledges started only after checking its own actual environment.
  starting means preflight or acknowledgement is pending. Restore host access, then resume.
  Read-only access checks have 15-second deadlines and at most three transient attempts.
  Access is checked again before worker turns and at handoff; mutations are never retried blindly.
  resume --config FIRST --then SECOND --detach --json runs a fixed approved sequence.
  One repository lease spans integration, the full gate, closure and successor startup.
  status --json includes sequence phase, current progress, blocker and recovery action.

Streaming output:
  run and foreground resume emit JSONL by default, immediately, without ANSI colors.
  Fields: schema_version (1), time (UTC RFC3339), type, level, run_id, message;
  activity also includes label. --activity adds filtered worker/tool events and heartbeats.
  Types: controller.started, controller, controller.completed, controller.error,
  loop.completed, loop.failed,
  worker.started, worker.message, worker.error, tool.started, tool.completed,
  tool.failed, validation.started, validation.check, heartbeat, activity.control.
  Levels: info, warning, error, debug. Raw worker payloads are never forwarded.
  looper run --config FILE --activity | jq --unbuffered .
  looper resume --run RUN_ID --activity | jq --unbuffered .
  looper run --config FILE --activity --output human
  For resume --detach, --output selects the controller pane's stream format.
  --output human enables colored log lines using the color preference above.
  Output format is selected at startup; activity control changes visibility and colors.
  The final loop.completed or loop.failed event covers the whole delivery sequence.
  Human output ends with Loop completed successfully. or Loop failed: REASON.
  Detached outcomes appear in the controller pane; the launcher returns startup status.
  Startup failures and final diagnostics go to stderr. A closed reader does not
  interrupt owned work; output failure is returned after that work finishes.

JSON results (--json):
  Exactly one uncolored object with schema_version: 1 and status.
  run describes an exact selection; runs lists discovery or ambiguous candidates.
  error includes code, message, retryable and an optional next_action.
  Inspect run.status and its blocker even when the top-level status is ok.
  started confirms ownership acknowledgement; starting is pending; neither means complete.
  Poll status until run.status is complete or a blocker needs resolution.

Views and copying:
  looper --watch opens the selected project's native backend; no separate script is needed.
  run.view_command is looper --watch --run RUN_ID for an exact returned run.
  Inside Herdr, watch follows the recorded pane as text, retaining slice, phase and message.
  Ctrl+C closes that text viewer without interrupting the controller.
  Otherwise Herdr opens its interactive client; Zellij and tmux use read-only clients.
  Opening a view preserves the existing worker. Use native selection/copy controls.
  tmux read-only attachment restricts keyboard copy mode; interactive attachment is:
  tmux -L slice-loop attach-session -t '=SESSION'
  Existing Slice Loop state remains discoverable; locks and terminal sockets stay compatible.

Exit codes:
  0 success/already running; 1 unexpected I/O; 2 invalid arguments/not found;
  3 selection required; 4 recovery blocked; 5 detached startup/backend failure.

Only run and resume execute the approved delivery set and perform its delegated merges.
init/register/list/status/watch/check do not execute worker turns or submit merges.
run/resume merge canonical PRs with Rebase + Fast-forward into dev and require
the configured full gate and closure evidence before completion. Neither broadens authority.
`, version, backendAuto, defaultOutput, defaultColor)
	return err
}
