# ADR-0003: Parsers run in-process and compiled in, with no worker process

- **Status:** Accepted
- **Date:** 2026-10-30
- **Deciders:** the project owner, in plan 4H (spec 17 Q7)
- **Supersedes:** none
- **Superseded by:** none

Recorded after the fact from [architecture.md](../architecture.md) ("Parser isolation") and the
"Parser isolation" item of the [parser limitations](../limitations/parsers.md).

## Context

Parsers read hostile data. The threat model is hostile data, not a malicious parser author. The
question was whether parsers need a worker process (plan 4J) or whether the controls of the host
process are enough: the sealed readers, the memory budget, the soft `debug.SetMemoryLimit`, and
the abandonment of a parser that ignores cancellation.

The evidence at the time: the first decoder fuzz campaign (12 targets) found nothing; every
hostile-input test bounds its work; the audit and warning volume on hostile mutations is bounded
(`TestHostileAuditVolumeIsBounded`); and the scale run held a streamed 1.25 GiB input to 136 MiB
at a 256 MiB budget ([performance.md](../performance.md)).

## Decision

We run parsers in the host's process, compiled in, under the sealed readers, the memory budget,
the soft memory limit and abandonment. Plan 4J (a worker process) is not opened.

## Consequences

- **Good:** parsers run under the controls the host already has, with no second process to
  build, audit or ship.
- **Bad:** a stack overflow or an out-of-memory kills the process. An abandoned parser's
  goroutine keeps a core and the memory its inputs pin until the process exits, and the whole
  run stops with "restart recommended".
- **Neutral:** the limits and the measured memory are recorded in the
  [parser limitations](../limitations/parsers.md).

## Revisit if

- A fuzz or real-data finding that the in-process controls cannot contain: a stack overflow, an
  out-of-memory despite the budget, or a goroutine that cannot be abandoned.
- The post-LX-8 parser fuzz campaigns (pending a Docker VM of at least 4 GiB) find such a case.
- The first app parser needs a native decoder.

## Alternatives considered

- **A worker process for parsers (plan 4J):** not opened, because the in-process controls held on
  every piece of evidence listed under Context.
