# Detached execution model

See [behavior](detached-execution.md).

## Host context

The optional host descriptor has `version: 1`, an `environment` map restricted
to location references, optional `required_executables`, and an optional
absolute `provider_probe` executable. Extra tools use names or absolute paths.
The descriptor is read again at access boundaries. It contains no credential
values. A provider probe returns 0 for access, 75 for transient failure, 77
for authentication failure, and 78 for missing configuration. Output is discarded.

## Blocker

An optional checkpoint `blocker` adds component, class, attempt count, concise
safe reason and recovery action to existing status. Checkpoint counters,
conversation, pending merge, evidence, and merged records remain unchanged.
Repair followed by explicit resume reruns prerequisites; only transient
read-only operations retry automatically.
Runtime metadata pins `worker_home`; a changed credential/conversation store
blocks recovery or another worker turn before spending its budget.

## Sequence

`sequence.json` lives in the first config's state directory. It contains a
version, an ordered list of canonical config paths and hashes, current index,
phase, completion receipts, and optional blocker. The list has 2 to 32 unique
sets for one canonical repository. Each receipt pins the completed config
hash and closure commit. A successor's `sequence-owner.json` references the
first config so inspection and recovery resolve the supervisor.

The first run lock and common repository lock stay held across all sets;
the current successor also holds its own run lock. Each set uses its existing
state file. Phases are running, handoff, blocked, and complete. A receipt is
saved before the next index is activated. Resume rechecks config hashes,
ownership, receipt ancestry, and canonical approval. Completed receipts are
never replaced by rerunning integration or resetting checkpoints.
