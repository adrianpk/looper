# Specification

Start from the request and the relevant existing context. Link a dump or quiz
only if it exists and informs a decision. Neither is required.

Use the repository template. Otherwise adapt
[the spec template](../assets/spec.md) under `ops/<namespace>/spec/`.
Use its draft location when material decisions remain open; otherwise keep
`Status: draft` until acceptance is recorded from the maintainer.

## Define the behavior

Describe the problem, actors, inputs, outputs, and observable success. Inspect
the affected code or interface before asserting current behavior. Give each
acceptance criterion a stable identifier, such as `AC1`, so planning can name
the behavior it delivers.

For each meaningful flow, define the successful path and the failures that
change its outcome. Include boundary values, permissions, state transitions,
ownership, cancellation, retries, or partial effects when the behavior needs
them. Avoid generic sections for risks that do not apply.

State invariants precisely enough to test. For example, replace "safe retries"
with which operation can be repeated, the stopping condition, and how an
already-applied result is recognized. Replace "handle errors" with the
observable state, retained progress, and available recovery action.

Keep implementation choices at the level needed to preserve a contract or
accepted constraint. Mark unconfirmed choices as proposals. If the change
introduces durable entities or persistence contracts, add a sibling
`<topic>-model.md` defining identity, stored fields, relationships, uniqueness,
migrations, and lifecycle as applicable. A behavior-only change needs no model.

## Establish acceptance

Write acceptance criteria in observable terms: initial conditions, action, and
expected result. Include counterexamples that distinguish the intended
behavior from a plausible incorrect implementation. Do not define acceptance
as matching the proposed code structure.

Associate criteria with available validation methods. Read commands from the
repository; do not invent a full gate or claim planned checks have passed.
Flag a missing validation method as a planning gap.

Before handoff, check that the behavior, examples, invariants, and criteria
agree. List only open decisions that affect the work. A contradictory or
underspecified behavior stays draft; do not hide its uncertainty in tickets.
