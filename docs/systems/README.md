# System design documents

One document per system, describing **how that system works inside**: its data structures, its
main path through the code, the invariants it maintains and where it breaks. The reader is an
agent or a developer about to change that system.

## What belongs here, and what does not

| Question | Document |
|---|---|
| What must never be broken? | [invariants](../invariants.md) |
| What does the build not do yet? | [limitations](../limitations.md) |
| Which package may import which? | [architecture.md](../architecture.md) |
| How does _this one system_ work inside? | here |
| Why was this chosen over the alternative? | [adr/](../adr/README.md) |
| What is planned, and in which order? | [ROADMAP.md](../ROADMAP.md) and the local specs and plans |

An example of the difference. The ext4 invariants say a lookup never reports "not found" for a
directory it did not read to the end: that is behaviour a test pins. A system document for ext4
would say how the reader walks an htree, which budgets it charges, and where a truncated image
cuts the walk. None of that is a rule, and all of it is what the next person to change the reader
needs.

If you find yourself restating an invariant or a limitation, link to it.

## The documents

- [F2FS roll-forward](f2fs-rollforward.md): the post-checkpoint node chain, its report, block copies and recovered files.

Further candidates, in the order changes reach them most: the case and its verify
(`internal/evidence`), the records writer and full-text index (`internal/records`), the SQLite
file library (`internal/sqlitefile`), the parse host (`internal/artparse`) and each filesystem
reader.

## Writing one

Copy [_template.md](_template.md) to `<system>.md` and add it to the list above. A system earns a
document when it has internal structure a newcomer cannot infer from reading one file. A system
that is one clear package with good names does not need one: the code is the documentation.

Keep it current with the code. Unlike a decision record, a system document describes the present
and **is** rewritten as the system changes. A stale one is worse than none, because it will be
believed.
