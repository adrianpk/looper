# Discovery

Use discovery to reduce uncertainty that affects the requested work. Reuse
existing context instead of restarting an interview.

## Dump

Let the user establish the central topic and transfer context freely. Preserve
constraints, rejected alternatives, uncertainty, and derived threads. Ask only
when meaning would otherwise be lost; a dump is not an interview.

When persistence is requested, follow the repository's discovery format.
Without one, write `ops/<namespace>/dump/<topic>.md`. Preserve a separate
`<topic>-raw.md` source capture when the transfer is long or spans messages.
Keep secrets out of both. Preserve intent when translating to the artifact
language and identify any consequential ambiguity.

Curate the material into the sections it needs: purpose, source, decisions,
constraints, observations, assumptions, open questions, and derived threads.
Keep a derived idea separate unless the user includes it in the active scope.
Do not turn preferences into requirements or proposals into accepted decisions.

End when the user finishes the transfer. Continue into another artifact only
when that stage is already part of the requested preparation.

## Quiz

Before asking, inspect the existing material and define what sufficient clarity
means for this outcome. Cover the uncertainties that can change behavior,
permissions, integration, failure handling, or acceptance.

- Prepare a finite question budget. Default to eight questions; honor a
  smaller user or repository limit and never exceed 50 questions.
- Ask one question at a time and wait for the answer. Include concise choices
  when the alternatives are understood; allow a free answer.
- Prefer the question whose answer eliminates the most consequential
  uncertainty. Do not ask the user for facts available in the repository.
- Count follow-ups against the same budget. Limit a chain to the primary
  question, one follow-up, and one final clarification.
- Track answers, remaining questions, covered topics, and unresolved decisions.
  Reconcile a changed answer with its earlier dependent decisions.
- Stop when sufficient clarity is reached, the user stops, the budget is
  exhausted, or another question would not affect the result.

For an authorized saved quiz, follow the repository format. Without one, use
`ops/<namespace>/quiz/<topic>.md`; keep a separate source capture when needed.
Record distilled decisions and the reason they matter. Do not repeat the
interview as one heading per question.

At termination, state the remaining uncertainty and whether it prevents the
next requested artifact. An unanswered question is not assent. Preserve a
partial quiz when interrupted; resume from its unanswered decisions rather
than repeating settled questions.
