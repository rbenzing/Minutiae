# Minutiae — project rules for Claude

Minutiae is a mobile-device forensics tool (Go). Evidence integrity is the
product: a convenient change that weakens it is a bug.

## 1. Before any work
1. Read `docs/ROADMAP.md`. Identify the one sub-project the work belongs to.
   If it fits none, stop and ask — do not start unplanned subsystems.
2. Read that sub-project's spec and plan in `docs/superpowers/` (local only,
   gitignored). If they do not exist yet, brainstorm → spec → plan first.

## 2. Setup (idempotent — safe to rerun any time)
```bash
go version            # must report go1.26.x (go.mod toolchain auto-downloads it)
go mod download
go tool golangci-lint version   # pinned in go.mod as a tool; nothing to install globally
```

## 3. Definition of done (empirical)
Run `go run ./tools/check` and observe `CHECK PASSED` in the output.
It runs: `go mod tidy -diff`, `go vet`, `golangci-lint` (incl. gofumpt/goimports),
`go build ./cmd/minutiae`, `go test ./...` (with `-race` when cgo is available).
Never claim work is complete, fixed or passing without that output from the
current code. Auto-fix formatting with `go tool golangci-lint fmt`.

## 4. Forensic invariants (each is enforced by a named test — keep them green)
| Invariant | Test |
|---|---|
| Bytes from a device reach disk only via `evidence.Case.NewArtifact`/`Capture` (review rule; any stray file under `artifacts/` fails `case verify`) | `TestVerifyDetectsUnmanifestedFile` |
| Artifacts are never overwritten (`O_EXCL`) | `TestNewArtifactRefusesOverwrite` |
| Every artifact is hashed (SHA-256 + MD5) and in manifest + artifacts.db | `TestNewArtifactHashesAndRecords` |
| Partial/failed acquisitions are kept and flagged `incomplete` | `TestAbortKeepsPartialFlagged`, `TestPullToCaseCancelledKeepsPartial` |
| Audit log is append-only and hash-chained; tampering is detected | `TestAuditVerifyDetectsEdit`, `TestAuditVerifyDetectsDeletedLine`, `TestAuditVerifyDetectsReorder` |
| Device writes need explicit permission and are audited (size + sha256) before the write; the bytes sent are hashed and must match | `TestPushAuditedRequiresPermission`, `TestPushAuditedAuditsBeforeWrite`, `TestPushAuditedDetectsShortSend` |
| `case verify` detects altered/missing/extra evidence and exits 4 | `TestVerifyDetectsModifiedArtifact`, `TestCaseVerifyExitCodeOnTamper` |
| `audit.jsonl` and `artifacts.db` are created only by `Create`; a missing, empty, corrupt or torn audit log, or a missing db, fails closed (exit 4) and is never recreated | `TestOpenMissingAuditOrDBIsIntegrityError`, `TestCaseVerifyMissingAuditExits4`, `TestCaseVerifyCorruptAuditLineExits4`, `TestAuditVerifyFlagsEmptyLog` |
| An open case holds an exclusive OS lock on `case.lock` (never evidence); a second `Create`/`Open` fails until `Close` | `TestCaseLockExcludesSecondOpen`, `TestCaseCommandRefusesCaseInUse` |

## 5. Architecture rule (enforced by `TestArchitectureDependencyRule`)
- `internal/evidence` and `internal/version` import no other Minutiae package except `evidence → version`.
- `internal/device` imports only `evidence`, `version`.
- Backends (`transport/serial`, `protocol`, `android`, `ios`) import only `device`, `evidence`, `version` and their own sub-packages; never `cli`, never each other.
- `internal/android/adb` imports no Minutiae package.
- `cmd/minutiae` imports only `internal/cli`.

## 6. Testing
- TDD: write the failing test, see it fail, implement, see it pass.
- Default tests never need hardware: use fakes (fake ADB server, in-memory serial, fake iOS).
- Hardware tests use `//go:build hardware` and are run manually: `go test -tags hardware ./...`.

## 7. Git
- Use `git` only. Never `gh` (not installed). For GitHub use the web UI or REST API via `curl`.
- Never commit `cases/`, real evidence, or device dumps. `docs/superpowers/` stays local.
- Commit after each green task.

## 8. Release build
```bash
go build -ldflags "-X github.com/rbenzing/minutiae/internal/version.Version=v0.1.0 -X github.com/rbenzing/minutiae/internal/version.Commit=$(git rev-parse --short HEAD)" -o bin/minutiae.exe ./cmd/minutiae
```
