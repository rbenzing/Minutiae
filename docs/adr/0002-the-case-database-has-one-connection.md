# ADR-0002: The case database is reached through one connection; only `case verify` adds read-only connections

- **Status:** Accepted
- **Date:** 2026-10-23
- **Deciders:** the project owner, in plan 5D (Task 15c)
- **Supersedes:** none
- **Superseded by:** none

Recorded after the fact from [architecture.md](../architecture.md) ("The case database has one
connection"), which was written with the verify exception in commit `93291c9`. The single
connection itself predates that commit.

## Context

`artifacts.db` holds the records of a case. The pragmas that protect the evidence (foreign keys,
recursive triggers, rollback journal, full fsync, `temp_store = MEMORY`) are settings of a
connection, and statements on separate connections can meet each other's transactions.

At 10,000,000 records, P10 (`records.integrity_check`) is 55% of a `case verify` run. The target
verify time cannot be met while the longest phase waits for the case connection.

## Decision

We reach `artifacts.db` through one connection: every `ReadTx` and `StoreTx` queues on it, and
nesting is refused. The one exception is `case verify`, which opens, for the record part of its
own run, at most three more connections that are read-only, so that P10 runs beside the record
stream and the index pass. The rule, why the exception is safe and the tests that guard it are in
[architecture.md](../architecture.md).

## Consequences

- **Good:** every statement runs with the protecting pragmas and never meets another transaction.
  The longest verify phase no longer waits for the case connection.
- **Bad:** every other reader and writer queues on one connection. Verify has a second code path,
  which needs its own guards (`TestVerifyIntegrityCheckConnectionIsReadOnly`,
  `TestVerifyConcurrentPhasesEqualSerial`, `TestVerifyConcurrentCancelKeepsFindings`).
- **Neutral:** the report is byte-identical to the serial order, and verify falls back to that
  order on the case connection when the pool cannot be opened.

## Revisit if

Nothing recorded with the decision. The measured verify times and the open follow-ups are in
[performance.md](../performance.md) and the [roadmap](../ROADMAP.md).

## Alternatives considered

- **Serial verify on the case connection only:** kept as the fallback, but it cannot meet the
  target verify time.
