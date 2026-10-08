# Minutiae — project rules for Claude

Minutiae is a mobile-device forensics tool (Go). Evidence integrity is the
product: a convenient change that weakens it is a bug.

## Before any work
1. Read `docs/ROADMAP.md`. Identify the one sub-project the work belongs to.
   If it fits none, stop and ask — do not start unplanned subsystems.
2. Read that sub-project's spec and plan in `docs/superpowers/` (local only,
   gitignored). If they do not exist yet, brainstorm → spec → plan first.
3. Read the reference docs your change touches (they are binding, not background):

| Doc | Holds | Edit it when |
|---|---|---|
| `docs/invariants.md` | every forensic invariant and the named tests that enforce it | you add or change an invariant (same commit) |
| `docs/architecture.md` | the package dependency rule, purity and single-writer rules, the cgo policy | you add a package or an import edge |
| `docs/testing.md` | check forms (fast/race), test rules, fixtures, release build | you change how something is tested or built |
| `docs/limitations.md` | known limitations, residuals, from-memory formats | you close or add a limitation |

## Setup (idempotent)
```bash
go version            # must report go1.26.x (go.mod toolchain auto-downloads it)
go mod download
go tool golangci-lint version   # pinned in go.mod as a tool
```

## Definition of done (empirical)
Run `go run ./tools/check` and observe `CHECK PASSED`. It runs `go mod tidy -diff`, `go vet`,
`tools/parserhash -check`, golangci-lint (gofumpt/goimports), the builds (host and
`CGO_ENABLED=0` cross-builds for linux/amd64 and darwin/arm64) and `go test ./...`.
- FAST form, `CGO_ENABLED=0 go run ./tools/check` (no `-race`): every task commit.
- RACE form, plain `go run ./tools/check` with a C toolchain: required before a plan merges
  to main (how the race step is split: `docs/testing.md`).

Never claim work is complete, fixed or passing without that output from the current code.
Auto-fix formatting with `go tool golangci-lint fmt`.

## Forensic principles (each is pinned by tests listed in `docs/invariants.md`)
- Device bytes reach a case only through `evidence.Case.NewArtifact`/`Capture`: hashed
  (SHA-256 + MD5), recorded in the manifest and `artifacts.db`, never overwritten (`O_EXCL`).
- Partial or failed work is kept and flagged `incomplete`; it never supersedes complete work.
- The audit log is append-only and hash-chained. Audit BEFORE the act (device writes,
  record batches, recoveries); an audit failure blocks the act.
- Device writes need explicit permission. Analysis never modifies source evidence.
- An integrity failure exits 4 and is never masked by another error or a cancel. A corrupt
  or missing audit log or database fails closed and is never recreated.
- Unknown is never absent: an undecidable value or lookup is reported as unknown or an
  error, never as empty, zero, a default or "not found". No value is invented or repaired.
- Recovered, historical or carved data is never presented as live, and its confidence is
  capped by its provenance. Identity comes from structure, never from a guess.
- Hostile input never panics the process, never allocates from a declared size and never
  loops; every bound is a typed error or a counted warning, never a silent cut.
- `case verify` recomputes rather than trusts; nothing exported may disable a check.

## Architecture (details: `docs/architecture.md`)
- Parsers (`image`, `volume`, `filesys`, `decode`, `sqlitefile`, the parsers) work on
  `io.ReaderAt`, import no case code and can never write to a case.
- `internal/examine` is the only bridge from parsers to the case; `internal/records` is the
  only writer of the record tables. `cmd/minutiae` imports only `internal/cli`.
- cgo only when a capability has no pure-Go route, fenced in `//go:build cgo` packages with a
  `!cgo` fallback; the default release is `CGO_ENABLED=0`.

## Testing (details: `docs/testing.md`)
- TDD: write the failing test, see it fail, implement, see it pass.
- Default tests never need hardware or Docker. Real-image fixtures carry an independent oracle.
- Kernel modules are loaded ONLY inside a QEMU guest in a container, never into the host or
  the Docker Desktop VM; fixture containers run without `--privileged` unless the fixture
  docs say otherwise.

## Git
- Use `git` only. Never `gh` (not installed). For GitHub use the web UI or REST API via `curl`.
- Never commit `cases/`, real evidence, or device dumps. `docs/superpowers/` stays local.
- Commit after each green task.
- Never name competing forensic tools or their vendors anywhere: docs, code, comments, tests,
  commit messages, specs or plans (not even in a deny-list). Describe capabilities
  generically ("commercial forensic suites"). Review every diff for this before committing.
