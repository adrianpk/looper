# Delivery planning

Prepare a delivery set when the user requests planning for the agreed scope.
Reuse a sufficiently clear spec or existing tickets; do not require a new
discovery cycle. Follow the repository's planning conventions while preserving
Looper's identity contract below.

## Define the delivery units

Prefer the smallest independently reviewable increments that leave the
repository valid. Use behavior-first slices when practical. Use layers only
when they provide a concrete review or dependency benefit, and state why.

For each slice, define its outcome, acceptance criteria, inputs, dependencies,
bounded tasks, focused checks, exact branch, exact PR title, and report path.
Dependencies must be delivered prerequisites or earlier slices; identify an
external blocker instead of scheduling work as if it were available.

Create or reuse one ticket per slice by default. Preserve an existing useful
ticket decomposition when every ticket has one owning slice. Link each ticket
under that slice and map spec acceptance IDs to the owning work. Use the
[ticket procedure](tickets.md) for the actual files.

## Write the plan and tracker

Use repository templates when available. Otherwise adapt the bundled
[plan](../assets/plan.md) and [tracker](../assets/tracker.md). The tracker
records tasks and progress; it repeats the plan's canonical identity table.

Looper requires:

- A Markdown table whose first header is `Slice`.
- Columns `Exact branch` (or `Branch`), `Expected PR title`, and `Report`.
- Consecutive rows `Slice 1` through `Slice N`, with 1–100 slices per set.
- Identical slice numbers, branches, titles, and reports in both documents.
- Clean repository-relative report paths, with no absolute path or `..` escape.
- Unique slice branches and report paths. Branches must be valid Git branch
  names and cannot be `dev` or `main`.

Keep literal `|` characters out of table values because Looper splits on that
character. Use `git check-ref-format --branch` to validate proposed branch
names. Assign report paths now; implementation creates the reports later.

Record the actual full-gate command from repository tooling and any required
environment or fixtures. Distinguish per-slice checks from the integrated
full gate and closure. If `init` is subsequently requested, its `--gate`
argument must match this command; it does not read `Full gate:` prose.
Likewise, required per-slice commands need the configuration's
`required_commands` fields; prose alone does not configure enforcement.

Use `Spec: [Title](relative-path)` only when that spec must be an allowed
closure path. Looper also recognizes `Concern:`, `Model:`, and
`Specification:` link metadata. Ordinary supporting links can remain in
the slice inputs.

Finish ticket transitions before the full gate. If ticket paths must change
during closure, include each affected old and new path in the configuration's
`closure_paths` before execution; directory globs do not authorize closure.

## Review and handoff

Resolve every local input link relative to its document. Compare plan and
tracker identities literally, and inspect acceptance coverage and dependency
order. Correct conflicting existing artifacts rather than adding another
copy of the same delivery.

Leave `Status: draft` and omit `Approved:` until actual maintainer approval.
After explicit approval, record the actual approver and date in both documents.
A nonempty `Approved:` line is required by Looper; the line must represent
real approval, not sample text.

Execution additionally requires those approved artifacts committed and pushed
to `origin/dev`, the primary checkout clean on `dev`, and delegated execution
and Rebase + Fast-forward merge authority. Preparing documents does not perform
those actions. Present the files and any remaining decisions for review.
