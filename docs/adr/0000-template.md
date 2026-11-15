# ADR-0000: Short declarative title, the decision and not the topic

- **Status:** Proposed
- **Date:** YYYY-MM-DD
- **Deciders:** who agreed
- **Supersedes:** ADR-XXXX, or none
- **Superseded by:** ADR-XXXX, or none

## Context

The forces in play, written so someone who joins in a year understands the pressure without
having been here: the constraints, the evidence-integrity risk, the numbers that mattered. State
the problem, not the answer.

## Decision

One paragraph, active voice, present tense: **we do X**. When the decision produces a rule the
code must obey, link the rule in [architecture.md](../architecture.md) or the
[invariants](../invariants.md) instead of restating it.

## Consequences

What follows from this, honestly, including what it costs.

- **Good:** what this buys.
- **Bad:** what it forecloses or makes harder.
- **Neutral:** what merely changes shape.

## Revisit if

The findings that would reopen the decision. Write "Nothing foreseen" when there are none.

## Alternatives considered

- **The alternative:** why it was not taken. An alternative with no stated reason is a list item,
  not an alternative: leave it out.

---

<!--
How to use this template

1. Copy to `NNNN-kebab-case-title.md`, taking the next free number. Numbers are never reused or
   renumbered.
2. Fill in every section. Delete none; write "None" where a section is genuinely empty.
3. Add the row to the index in README.md and link the record from the rule it explains.
4. Once Accepted, the record is not rewritten. A changed decision gets a NEW record that
   supersedes this one, and this one is marked Superseded.
5. Never name competing forensic tools or their vendors.
-->
