# Changelog

Notable changes to Looper are documented here.

## Unreleased

### Added

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

- Loops end with an explicit success or failure message covering the whole
  delivery sequence, including failures during startup and between sets.
- Detached startup checks access before reporting `started`. Authentication
  loss and failed continuation expose actionable blockers; temporary remote
  read failures use bounded retries without repeating merge submissions.
- `looper --watch` uses valid Herdr attachment arguments. Inside Herdr, it
  follows the recorded pane as text, preserving slice, phase and message
  without opening a nested client or interrupting delivery.
