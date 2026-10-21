# Changelog

Notable changes to Looper are documented here.

## Unreleased

### Added

- Environmental final-gate failures can be retried once on the same clean
  candidate with `retry-gate`, including after code-repair authority is exhausted.
  The original failure, configuration, repair history and worker budgets remain
  retained; completion requires the unchanged full gate to pass.
- Exhausted final-gate repair authority can receive a separate bounded approval
  with `authorize-additional-gate-repairs`, tied to the exact failed candidate.
  Earlier policies, integrated repairs, conversations and worker budgets remain
  unchanged; completion still requires the full gate.

## 0.4.3 - 2026-10-14

### Fixed

- Slice maps ignore unrelated tables and reject malformed or duplicate
  canonical tables. PR candidates must preserve approved plan and tracker
  identities before integration.
- Herdr text watching shows the retained checkpoint and blocker when the
  recorded pane disappears.

## 0.4.2 - 2026-10-14

### Fixed

- Validation that outlives a worker turn keeps the controller waiting and
  continues delivery automatically when checks finish. Recovery retains the
  same batch and budgets; status and watch show its progress and outcome.
- Temporary model-capacity failures automatically retry the same conversation
  up to five times. Retry timing and budgets survive controller restarts and
  remain visible in status and watch; other worker failures stay blocked.

## 0.4.1 - 2026-10-11

### Added

- Final-gate failures can trigger scoped repair PRs and automatic revalidation
  after explicit, bounded authorization with `authorize-gate-repairs`.
  Existing runs retain their conversation, slice history and worker budgets.
- Optional agent skills prepare specifications, plans, trackers and tickets
  from requests or existing artifacts, and guide operation of approved loops.
- Explicit sequential delivery with `resume --then FILE`. Each approved set
  completes integration, full validation and closure before its successor;
  interrupted handoffs retain progress, conversations and budgets.
- Host execution contexts and `preflight --config FILE` check tools, Git,
  Forgejo permissions and provider access in the executing environment.
- `looper --watch` opens the current project's active loop through its recorded
  terminal backend, without a separate script or a terminal session name.
  Multiple active matches are limited to that project's named delivery sets.
- Detached recovery supports Herdr and Zellij alongside tmux. Select a backend
  with `--backend`; `auto` prefers Herdr, then Zellij, then tmux. Agent JSON
  reports the selected backend and a command to open its existing view.
- Human-readable output supports `--color auto|always|never`. `always` enables
  colors by default even when `NO_COLOR` is inherited, including over SSH.
  Choose `auto` or `never` to override this preference. Color control can also
  change future output during a running execution. JSON stays plain.
- Global and subcommand help describe the preferred defaults, available commands,
  agent recovery workflow, JSON results, view commands, and exit codes. Help
  succeeds without a configuration or an active controller.

### Changed

- The application and executable are named `looper`. Existing Slice Loop
  registrations, checkpoints, active ownership locks and terminal views remain
  compatible; new registrations use the `looper` state directory.
- Run selection defaults to the current Git project, including subdirectories
  and linked worktrees. `status` and `resume` prefer its active execution over
  completed history. Use `--set NAME` for local ambiguity and `list --all` for
  explicit discovery across projects.
- Controller streams use JSONL by default, with one event per line for live
  consumption through `jq --unbuffered .`. Use `--output human` for colored log
  lines. Detached recovery applies the selected format to its controller pane;
  `--json` continues to return a separate single startup result. Closing a log
  consumer does not interrupt the active delivery.

### Fixed

- Completed runs remain complete when another delivery owns the repository.
- Closure instructions identify the incomplete artifact and expected metadata.
  Access and integrity failures block closure without requesting blind edits.
- Git access retains the default SSH executable when validation scripts narrow
  `PATH`, while preserving explicit `GIT_SSH_COMMAND` settings.
- Loops end with an explicit success or failure message covering the whole
  delivery sequence, including failures during startup and between sets.
- Detached startup checks access before reporting `started`. Authentication
  loss and failed continuation expose actionable blockers; temporary remote
  read failures use bounded retries without repeating merge submissions.
- `looper --watch` uses valid Herdr attachment arguments. Inside Herdr, it
  follows the recorded pane as text, preserving slice, phase and message
  without opening a nested client or interrupting delivery.
