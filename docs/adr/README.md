# Architecture decision records

A decision record captures **one choice, the pressure that forced it, and what it cost**, at the
moment it was taken. It is a log entry, not a description of the system. How Minutiae works
today is in the reference docs (start at the [documentation map](../README.md)); why it is that
way, and what else was on the table, is here. Read the record before arguing for one of its
alternatives: a settled decision is reopened by a new record, not by a change that quietly
works around it.

## The rules that make this folder worth trusting

**One decision per record.** A record that decides three things cannot be superseded, because two
thirds of it may still be right.

**Numbers are permanent.** Take the next free one. Never reuse, renumber or fill a gap: the
numbers appear in commit messages, plans and code review.

**An accepted record is never rewritten.** Not to fix its reasoning, not to bring it up to date.
A decision that changes gets a new record that supersedes the old one; the old one is marked
`Superseded by ADR-NNNN` and otherwise left exactly as it was. Typos and broken links may be
fixed. Reasoning may not.

**Status is one of:** `Proposed` · `Accepted` · `Superseded by ADR-NNNN` · `Deprecated` (the
decision no longer applies and nothing replaced it).

**Write the title as the decision.** "Parsers run in-process", not "Parser isolation". A reader
scanning the index should learn the architecture from the titles alone.

**The rule lives in the reference doc; the record holds the why.** When a decision produces a
rule the code must obey, the rule is stated once, in [architecture.md](../architecture.md) or
the [invariants](../invariants.md), and the record links to it.

**Not everything is a record.** A choice earns one when it is costly to reverse and not obvious
from the code. Which package owns the case connection is; a flag name is not.

## Adding one

Copy [0000-template.md](0000-template.md) to `NNNN-kebab-case-title.md`, fill it in, add the row
below in numeric order, and link the record from the rule it explains.

## Index

| # | Decision | Status | Date |
|---|---|---|---|
| [0001](0001-cgo-only-where-no-pure-go-route-exists.md) | cgo is allowed only where no pure-Go route exists, fenced in its own packages, and the default release stays pure Go | Accepted | 2026-10-07 |
| [0002](0002-the-case-database-has-one-connection.md) | The case database is reached through one connection; only `case verify` adds read-only connections | Accepted | 2026-10-23 |
| [0003](0003-parsers-run-in-process.md) | Parsers run in-process and compiled in, with no worker process | Accepted | 2026-10-30 |
