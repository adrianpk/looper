# Detached execution and approved continuation

Status: accepted

## Contract

Launchers, controllers, workers, validation, and continuation use the same
host execution context. `LOOPER_CONTEXT` selects a host-owned JSON descriptor;
the default is `$HOME/.config/looper/context.json`. Its environment overrides
are restricted to executable search paths, home/config/state directories, and
credential references. Credentials remain in the SSH agent and the tools'
host stores. Environment snapshots and credential values are never persisted.

The detached controller checks executable resolution, Git remote access,
authenticated Forgejo access and repository permissions, and provider access
before acknowledging startup. It repeats access checks before worker turns
and between sets. Read-only remote operations have deadlines and at most three
attempts for recognized transient errors. Authentication, configuration,
validation, and unknown ownership failures require intervention. An unknown
remote error is not assumed transient. Merge submissions and worker turns are
never automatically retried by this mechanism.

Codex's default provider check uses account and online rate-limit reads. This
checks ChatGPT authentication and available account quota, without creating a
thread. Other providers require a host-owned executable probe. Account access
does not guarantee entitlement to every model or future request success.

## Sequential delivery

`resume --then <config>` explicitly lists each approved successor in order.
The list is pinned before execution and cannot be replaced during recovery.
Each config must identify the same repository and retain canonical approval.
One controller keeps the repository lease across the entire sequence. A set's
integration, full gate, canonical merge verification, and operational closure
must succeed before handoff. Each set retains its original checkpoint,
conversation, merge intents, and budgets.

The sequence persists handoff intent before starting the successor. Status
exposes its current config, completed sets, phase, failing component, failure
class, and recovery action. Loss of the supervisor during handoff is visible
as an interrupted handoff. Recovery uses the original sequence entry, checks
process ownership, revalidates prerequisites and retained closure, and resumes
the same checkpoint. It never clears an uncertain merge or live worker.

## Acceptance

- Different launcher and terminal-server environments cannot hide failed
  access behind a successful launch acknowledgement.
- Missing or expired agent access blocks without spending worker turns.
- Transient read failures have bounded retries; mutations retain their
  existing reconciliation rules.
- Interruption before and after a handoff preserves completed integration,
  conversations and turn budgets, and cannot create concurrent repository owners.
- Existing configs and checkpoints remain loadable without changing hashes.
- Validation uses isolated fixtures; existing executions remain untouched.

See [stored model](detached-execution-model.md).
