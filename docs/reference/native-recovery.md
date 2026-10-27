# Native recovery

Native recovery handles recoverable technical failures inside the controller
that already owns the run and repository. It preserves the delivery identity,
worker conversation, merge intent, validation evidence and consumed budgets.
It does not launch another controller or repeatedly invoke `resume`.

## Delegation

looper authorize-recovery --config FILE --policy FILE

The run must be idle and unfinished, with no active worker, validator or
uncertain merge. Delegation starts no worker. Repeating an identical policy is
idempotent; replacing or widening a pinned policy is rejected. The policy is
stored separately from the frozen configuration. Existing runs must explicitly
adopt it while idle; no running controller or installed binary is changed.

```json
{
  "version": 1,
  "profile": "bounded-v1",
  "gate_repair": {
    "branch_prefix": "fix/acceptance",
    "title": "fix: correct approved acceptance",
    "report_slice": 1,
    "max_repairs": 8,
    "allowed_paths": ["src", "tests"],
    "required_commands": ["go test ./..."]
  },
  "excluded_paths": ["tests/protected-contracts"],
  "diagnostics_per_family": 2,
  "diagnostics_per_run": 8,
  "failed_repairs_per_family": 3,
  "environmental_retries": 3
}
```

`gate_repair` uses the existing repair-policy contract: exact branch prefix,
title, existing report slice, allowed roots and ordered focused command prefix.
Its omitted or zero `max_repairs` defaults to eight. An existing policy must
match exactly; omit `gate_repair` to retain previously delegated repair
authority. Omitting it when no repair policy exists grants no repair PRs.

The profile's omitted or zero numeric fields use the values in the example.
Overrides are finite: 1–8 diagnostics per family, up to 100 per run, 1–100
failed repairs per family and 1–100 environmental retries. The per-run
diagnostic limit must cover its per-family limit. Gate-repair policies and
supplemental grants retain their existing combined ceiling of 100 PRs.

The ordinary defaults remain 200 total worker turns and five stalled turns.
Delegation does not change the actual limits in an existing configuration.
Every diagnostic, implementation, repair and provider-retry launch consumes
the same hard worker-turn budget before launch. Spare turns cannot create
repair grants. Diagnostic reservations and all run-wide allowances survive
new commits, restart and explicit resume; they are never refunded or topped up.

Use a controller build whose help includes `authorize-recovery` for adopted
checkpoints. Older builds do not enforce this delegation and must not resume
those checkpoints. Ordinary runs without delegation retain their existing
behavior and checkpoint format.

## Classification and continuation

| Condition | Controller behavior |
| --- | --- |
| Missing eligible exact-file authority | Verify provenance and append a scoped grant, then continue the same repair |
| Ordinary worker stall or technical blocker | Invoke a bounded diagnostic in the same conversation |
| Attributed output blocking cleanliness | Preserve it in quarantine before ordinary repair admission |
| Confirmed provider capacity | Retain existing five-retry backoff: 1, 2, 4, 8 and 10 minutes |
| Verified active validation | Observe its existing owner without worker turns or process termination |
| Positively reported temporary full-gate failure | Reserve an environmental retry on the same candidate |
| Missing semantic authority or exhausted hard budget | Retain `needs_decision` with its exact blocker |
| Uncertain ownership, effects or evidence | Stop safely with a typed blocker |

Remote reads retain their existing three attempts with 15-second deadlines.
There is no additional extended-access wait policy. Authentication,
configuration and integrity errors are not automatically retried just because
the older `retryable` inspection flag is true.

`status --json` exposes `recovery` counters and the active incident's code,
phase, action and status. `recovering` means a controller still owns the run;
an inactive controller is reported as interrupted. Recovery stream events are
`recovery.started`, `recovery.action`, `recovery.resolved` and `recovery.blocked`.
Native capacity and validation waits keep their existing visibility.

The controller retains the interrupted instruction and reconciles actual Git,
canonical PR, worker and validator state before further actions. A diagnosis
with a reserved but unknown outcome is not launched again automatically. A
retained provider waiting deadline can continue through its existing policy.
An uncertain merge remains subject to the existing explicit reconciliation
and `recover --retry-unmerged` contract; recovery never resubmits it blindly.

Actual controller or host death does not start a new supervisor. Missing or
stale terminal panes remain observation concerns and cannot trigger execution.

## Worker protocol

Existing successful statuses remain available. A worker can return:

```json
{
  "status": "recovery_requested",
  "message": "The integrated fixture needs exact-file repair authority",
  "recovery": {
    "version": 1,
    "kind": "paths",
    "paths": ["tests/fixture.txt"],
    "candidate_head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "validation_id": "",
    "incident_id": 0,
    "action_id": 0
  }
}
```

Kinds are `paths`, `repair`, `quarantine`, `wait` and `decision`. New structured
results include every schema field; use `recovery: null` for ordinary results,
and empty values or zero IDs when fields do not apply. Legacy result files
without a recovery field remain readable. The controller validates every
request. Unknown or malformed diagnostic responses consume
their allowance and cannot resolve an incident. A legacy `blocked` response
or ordinary stall can receive diagnosis when delegation permits it.

A diagnostic receives `incident_id` and `action_id` and must echo them in its
recovery object. A `repair` result must cite a fresh `validation_id` whose
successful exact-clean-HEAD, ordered-command evidence matches the active
canonical PR. Preparation can instead verify the explicitly delegated focused
command prefix on clean, pushed primary `dev`; closure can verify the complete
existing closure contract. A previous passing batch or unrelated command cannot clear a
stall. A `decision` reports genuinely missing authority; it is not a synonym
for an unresolved technical problem. A `wait` cannot invent another waiting
policy beyond native provider and validator behavior.

All fields are claims until verified. The agent cannot grant paths, increase
budgets, clear counters, weaken checks, change the approved behavior or merge.
Fixture setup and driver corrections must preserve approved observable
assertions and resource constraints and explain that relationship in the
normal PR. Provenance and a green gate do not prove semantic equivalence.

## Failure families and progress

Validation recurrence uses the failing command and its retained failure
identity, independently of candidate hashes. An approved check can emit one
stable case identity as a complete output line:

```text
LOOPER_FAILURE_FAMILY=parser.empty-input
```

Names contain 1–128 ASCII letters, digits or `_.:/-`, starting with a letter
or digit. Conflicting or invalid markers fall back to the command's common
family. Without a marker, different failures of the same command are grouped
conservatively. Worker-supplied diagnoses and candidate churn do not identify
new families. The initial red candidate is not a failed repair attempt;
subsequent verified repair outcomes are. Three unresolved failed repairs stop
by default. A verified passing batch can establish progress for that command;
run-wide repair, diagnostic and worker counters remain consumed.

Diagnostics permit two launches per stable incident family and eight overall.
They can diagnose an ordinary stall without clearing it. Recovery counts only
verified permission resolution, attributed output preservation or relevant
successful validation. Worktrees outside the active unit do not affect native
progress detection. Repeated generated-output recovery also has the finite
per-family action bound; creating another candidate does not replenish it.

## Exact-file provenance

Existing explicitly granted roots and exact files remain valid. The stricter
provenance rule applies only to additional derived authority.

Derived paths must be exact regular tracked files changed by canonically
verified PRs integrated by this delivery, including its recorded gate repairs.
The controller checks the recorded PR head and integrated commit, membership
in the failed candidate, current canonical file inventory, exact tree entry
and absence of intervening changes after the producing integration. Renames,
unaccounted edits, restored bytes after a foreign edit and ambiguous provenance
require explicit authority. Agent contracts and approved plan/tracker paths
cannot receive derived authority. `excluded_paths` adds exact-path or directory
exclusions without revoking prior explicit grants.

The controller reuses `gate_repair_path_authorizations`: the grant is tied to
one repair, its failed base and current repair branch head, and retains the
producing integration and file entry. The external
`authorize-gate-repair-paths` command keeps its idle-blocked admission rules.
Neither path changes policy bytes, required checks or consumed counters.
New files require an existing explicit allowed root or separate authority.

## Attributed outputs

Output ownership is opt-in through exact path/producer pairs in the policy:

```json
{
  "outputs": [
    {"path": "generated-fixture", "command": "printf fixture-data >&3"}
  ]
}
```

This object illustrates the optional `outputs` field of the complete policy.
The command runs from an already owned validation batch:

looper generate-output --config FILE --path generated-fixture --command 'printf fixture-data >&3'

The producer must descend from the verified validation command. Looper creates
the output with exclusive allocation, refuses tracked/pre-existing/linked
paths, and passes the allocated descriptor as descriptor 3 and
`LOOPER_OUTPUT_PATH=/proc/self/fd/3`. Producers must write that descriptor rather
than replace the path. The receipt binds the completed producer, batch,
candidate, policy, inode, mode, length and SHA-256 digest. Output files are
bounded to 512 MiB; output policies permit 100 entries and receipt storage 512.

After every relevant producer and validator has ended, quarantine rechecks the
receipt and file immediately before an atomic move to run-owned storage
outside the checkout. It records intent before moving and reconciles an
interrupted move without replacing content. Cross-filesystem moves stop
safely. Unknown, pre-existing, tracked or concurrently modified content remains
untouched. An executable bit, a new filename or a before/after inventory alone
never establishes ownership. No cleanup command, automatic staging or ignore
rule is added.

Failed or dirty validation remains failed. Preserving an output does not
manufacture passing evidence: the ordinary controller must obtain new evidence
for the exact clean candidate and unchanged checks. Successful focused checks
do not replace the configured full gate or authorize successor handoff.

## Environmental full-gate retries

When successful checks leave a candidate dirty solely through an attributed
output, verified quarantine can authorize a fresh full-gate batch using the
same environmental retry allowance. It does not convert the dirty batch into
a pass. Recurring output cannot create additional same-candidate retries.
Focused checks with the same proven cleanup obtain fresh evidence through
their ordinary validation path and remain bounded by output-family limits.

The approved full-gate command reserves exit status **75** for a positively
identified temporary cause. Ordinary assertion failures must use another
nonzero status. Error text, unknown failures and agent claims cannot mint a
retry. Existing frozen gate commands are never rewritten to adopt this signal.

Delegation permits one automatic retry per unchanged candidate and three
across the run by default. The controller reuses `gate_retries`, reserves the
retry before execution, retains the failed batch and reconciles its actual
new outcome after interruption. It preserves the exact command and candidate.
Another temporary failure on that candidate exhausts the delegation; available
worker turns or repair slots do not reset it. Manual `retry-gate` authority
remains separate and explicit.
