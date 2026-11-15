# <System>: system design

One sentence on what this system is responsible for, and one on what it is explicitly not
responsible for.

**Lives in:** `internal/…`, the packages that make up this system.

## The model

The data structures the system owns, and why they are shaped that way. Name the real types. If a
decision dictates the shape, link its record in [adr/](../adr/README.md) instead of re-arguing it.

## How it runs

The main path, traced concretely: what is called, in what order, what it reads and what it
writes. A reader should be able to follow it against the source with the file open. Name real
functions.

## Invariants

Link the rows of the [invariants](../invariants.md) area file this system must keep; do not
copy them. Name here only what is merely convention, and say so.

## Interactions

What this system reads from other packages and what it hands back, within the import rules of
[architecture.md](../architecture.md).

## Cost and bounds

Where the time and memory go, how they scale with the input, and which budget or cap stops a
hostile input.

## Where it breaks

The failure modes, the edge cases that have actually bitten, and the parts a newcomer reliably
gets wrong. This section is the reason the document is worth writing: do not leave it thin.
