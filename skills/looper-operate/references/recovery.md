# Recovery

Use the installed command help and returned blocker as the first operational
contract. Optional structured fields differ by version. Preserve the exact
configuration, checkpoint, worker conversation, counters, and merge intent.

## Classify before acting

| Evidence | Action |
| --- | --- |
| `starting` | Inspect acknowledgement and access-check progress; do not duplicate the launch. |
| Active controller or worker | Observe that owner. A stale-looking message alone does not establish interruption. |
| Authentication or missing configuration | Restore the host-owned prerequisite. Repeating launch cannot supply missing access. |
| Recognized transient remote failure | Let supported bounded read retries finish; inspect the retained blocker if they exhaust. |
| Validation failure | Read exact diagnostics and candidate evidence; distinguish a code failure from unavailable fixtures or access. |
| `repository_busy`, `worker_not_idle`, uncertain ownership | Identify the existing owner or unresolved process. Wait or seek explicit intervention; do not remove locks or send signals. |
| `turn_limit`, stalled progress | Inspect the exhausted budget or missing progress. Relaunching must not reset counters. |
| Pending or uncertain merge | Inspect the canonical PR and its head; preserve intent until the outcome is confirmed. |
| Completed predecessor, successor not started | Inspect the root sequence blocker and current phase; recover through the root configuration. |

An access check passing now does not prove it passed in the service environment
or will still pass at handoff. Inspect the effective host context references,
including executable paths, credential helpers or agent sockets, and provider
configuration. Check only the required access; never dump the environment or
credential files into diagnostic output.

`retryable` is not permission to repeat an unsafe mutation. Only recognized
transient reads receive automatic bounded retries. Workers, merges, builds,
and full gates are not interchangeable read probes.

## Resume without replaying work

1. Confirm that the selected config still describes the approved work and that
   its blocker has been resolved. Recheck local ownership and canonical state
   relevant to that failure.
2. Use the same `resume` selection. For a pinned sequence, select its original
   entry. Let the controller reload and reconcile its persisted state.
3. Inspect the returned startup result and subsequent status. Stop retrying if
   another owner is active, ownership is uncertain, or the same prerequisite
   remains unresolved. Do not issue identical resumes without new evidence.
4. Verify progress through the existing checkpoint. Do not reinitialize,
   delete locks, change config hashes, reset budgets, move worker homes, or
   manufacture missing merge evidence to force execution.

`recover --config FILE --retry-unmerged` clears an uncertain intent only after
the canonical PR is confirmed still open and unmerged at the recorded head,
and that recovery action is explicitly authorized. An unavailable forge leaves
the outcome uncertain. A canonically confirmed merge is reconciled by resume;
it must not be submitted again.

Completed work remains retained when access fails at a later boundary. A
predecessor is not complete merely because its final worker returned success:
integration, the configured full gate, and operational closure must all be
verified before its successor starts.
