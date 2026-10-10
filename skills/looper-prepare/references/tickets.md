# Repository tickets

Create the requested actionable records, or reuse existing records that cover
the same outcome. Tickets created by another harness, tool, or maintainer are
equally usable. Follow the repository's ticket schema and helpers when present.

## Reuse before creating

Inspect relevant records under `ops/<namespace>/ticket/`, or the configured
repository location. Select by the requested behavior and references, not by
the newest timestamp. Compare expected outcome and acceptance before calling
two tickets duplicates.

Keep existing IDs and filenames stable. Link a selected ticket into its owning
plan slice. If it has progressed into implementation or review, inspect that
work before proposing another delivery. Do not move an unrelated ticket or
pull every ready ticket into the requested scope.

An existing ticket can be the preparation input. Supply only missing context
or delivery artifacts. Looper executes the explicit plan and tracker; the
linked ticket's producer is irrelevant, and its directory alone is not a queue.

## Create a bounded ticket

Use the repository template. Without one, adapt
[the ticket template](../assets/ticket.md) and create the record in
`ops/<namespace>/ticket/open/`.

Each ticket defines one reviewable outcome, the relevant current behavior,
expected behavior, scope, acceptance criteria, dependencies, inputs, and
validation. Link the spec criterion and owning slice when they exist. Keep
validation as planned commands until it actually runs.

Use a UTC timestamp for `YYYYMMDDHHMMSS-<slug>.md` and
`id: TKT-YYYYMMDDHHMMSS`; `reported_at` must describe that capture time.
Check all ticket state directories for an existing ID. On a collision, retry
at the next actual UTC second and derive the filename, ID, and capture time
together. Use repository allocation helpers or locking when available; use
exclusive creation and retry allocation if another writer creates the record
first. Do not overwrite a record or invent a future capture time.

Default unknown classification fields to `unclassified`. The fallback values
are:

| Field | Values |
| --- | --- |
| `kind` | `unclassified`, `bug`, `task`, `cleanup`, `follow_up`, `research`, `decision` |
| `severity` | `unclassified`, `low`, `medium`, `high`, `critical` |
| `priority` | `unclassified`, `low`, `normal`, `high`, `urgent` |
| `scope` | `unclassified`, `domain`, `api`, `persistence`, `ui`, `ops`, `docs`, `infra` |
| `source` | `chat`, `manual_test`, `review`, `trace`, `user_report`, `implementation` |

Severity describes impact; priority describes urgency. Keep tags sparse and
lowercase. Leave `commits` empty until real implementation exists.

## Establish readiness

A new capture starts `open`. Move it to `ready` with `ready_at` only when
the outcome, inputs, dependencies, and acceptance are actionable under the
repository's triage rules. Important unanswered questions remain visible and
prevent affected work from being marked ready. Ready work can depend on an
explicitly identified earlier slice; the dependency must be fulfilled before
implementation starts.

The implemented path is `open → ready → in_progress → reviewing → solved`.
Active work can become `blocked` with a reason and recovery condition. Use
`closed` for administratively resolved work without implementation. Keep the
directory and `status` aligned, update lifecycle timestamps when transitions
occur, and repair links when a path moves.

Set `branch`, `pr`, and `commits` from actual work. Only `solved` or `closed`
records have `closed_at` and `resolution`; fallback resolutions are `fixed`,
`wont_do`, `duplicate`, `obsolete`, `moved`, and `answered`.

Before handoff, run the repository ticket checker when available. Otherwise
check identity uniqueness, timestamp agreement, metadata values, directory
alignment, local links, and the acceptance-to-slice mapping. Report reused
records as reused, new records as created, and unresolved work as unresolved.
