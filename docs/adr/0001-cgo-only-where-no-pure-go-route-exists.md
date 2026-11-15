# ADR-0001: cgo is allowed only where no pure-Go route exists, fenced in its own packages, and the default release stays pure Go

- **Status:** Accepted
- **Date:** 2026-10-07
- **Deciders:** the project owner
- **Supersedes:** none
- **Superseded by:** none

Recorded after the fact from the cgo policy in [architecture.md](../architecture.md) ("cgo policy
(user decision 2026-10-07)"). The context and alternatives are what that policy states or
directly implies.

## Context

Minutiae is pure Go and ships as a static `CGO_ENABLED=0` binary on every OS. Some capabilities
on the [roadmap](../ROADMAP.md) have no pure-Go route: decryption (sub-project 10), hooking into a
device or host environment (OS or device services, drivers, vendor libraries), and native
libraries that reveal artifacts no pure-Go reader can.

C code that reads evidence or device bytes brings a risk Go cannot contain: a crash in C code
cannot be recovered, so it would kill the process in the middle of a write to the case.

## Decision

We allow cgo when a capability needs it and no pure-Go route works. cgo code lives only in
dedicated `//go:build cgo` packages, each with a `!cgo` fallback that returns an error wrapping
`errors.ErrUnsupported`. A C library that reads evidence is fuzzed through its Go wrapper and
contained, by default in a helper subprocess. The forensic invariants bind cgo code like any
other. Parsers stay free of cgo (rule P4), and the default release stays pure Go. The rules are
the cgo policy in [architecture.md](../architecture.md).

## Consequences

- **Good:** decryption and device-hooking capabilities become possible without changing the
  default release or the parsers.
- **Bad:** each cgo capability needs a spec that names the C dependency, its version or pin, its
  licence and why pure Go is not enough, plus a fuzzed wrapper and a containment design. A
  cgo-capable build is per OS and needs a C toolchain.
- **Neutral:** no cgo package exists yet; the architecture test gains an allow-list entry per
  package when one is added.

## Revisit if

Nothing recorded with the decision.

## Alternatives considered

- **Pure Go only:** closes the capabilities listed under Context, which have no pure-Go route.
- **cgo wherever convenient, unfenced:** would lose the `CGO_ENABLED=0` build on every OS and put
  C code that Go cannot recover from in the evidence path; the fencing, containment and build
  rules of the policy exist to prevent exactly that.
