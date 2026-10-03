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
`go build ./cmd/minutiae`, `CGO_ENABLED=0` cross-builds for linux/amd64 and
darwin/arm64 (binaries discarded), `go test ./...` (with `-race` when cgo is available).
Never claim work is complete, fixed or passing without that output from the
current code. Auto-fix formatting with `go tool golangci-lint fmt`.

## 4. Forensic invariants (each is enforced by a named test — keep them green)
| Invariant | Test |
|---|---|
| Bytes from a device reach disk only via `evidence.Case.NewArtifact`/`Capture` (review rule; any stray file under `artifacts/` fails `case verify`) | `TestVerifyDetectsUnmanifestedFile` |
| Only exception: the iOS backup working dir `<case>/staging/<acq>/` (the device moves/overwrites files mid-backup); it is audited (`acquire.staging`), promoted file-by-file via `Capture`, and deleted only after full promotion; a leftover staging directory is a `case verify` problem | `TestBackupPromotesStagedFiles`, `TestBackupDeviceErrorStillPromotes`, `TestVerifyFlagsLeftoverStaging` |
| Artifacts are never overwritten (`O_EXCL`) | `TestNewArtifactRefusesOverwrite` |
| Every artifact is hashed (SHA-256 + MD5) and in manifest + artifacts.db | `TestNewArtifactHashesAndRecords` |
| Partial/failed acquisitions are kept and flagged `incomplete` | `TestAbortKeepsPartialFlagged`, `TestPullToCaseCancelledKeepsPartial`, `TestBackupInterruptedFileIsFlaggedIncomplete`, `TestPullShortReadIsFlaggedIncomplete` |
| Audit log is append-only and hash-chained; tampering is detected | `TestAuditVerifyDetectsEdit`, `TestAuditVerifyDetectsDeletedLine`, `TestAuditVerifyDetectsReorder` |
| Device writes need explicit permission and are audited (size + sha256) before the write; the bytes sent are hashed and must match | `TestPushAuditedRequiresPermission`, `TestPushAuditedAuditsBeforeWrite`, `TestPushAuditedDetectsShortSend` |
| `case verify` detects altered/missing/extra evidence and exits 4 | `TestVerifyDetectsModifiedArtifact`, `TestCaseVerifyExitCodeOnTamper` |
| `audit.jsonl` and `artifacts.db` are created only by `Create`; a missing, empty, corrupt or torn audit log, or a missing db, fails closed (exit 4) and is never recreated | `TestOpenMissingAuditOrDBIsIntegrityError`, `TestCaseVerifyMissingAuditExits4`, `TestCaseVerifyCorruptAuditLineExits4`, `TestAuditVerifyFlagsEmptyLog` |
| An open case holds an exclusive OS lock on `case.lock` (never evidence); a second `Create`/`Open` fails until `Close` | `TestCaseLockExcludesSecondOpen`, `TestCaseCommandRefusesCaseInUse` |
| `case verify` cross-checks every manifest record against its hash-chained `artifact.create` audit entry (both directions, duplicate ids flagged), so a consistent rewrite or erasure of file + manifest + db is still caught; unreadable parts are reported, never abort verify | `TestVerifyDetectsConsistentForgeryViaAudit`, `TestVerifyDetectsArtifactErasedFromManifestAndDB`, `TestVerifyDetectsManifestRecordWithoutAudit`, `TestCaseVerifyArtifactErasedEverywhereExits4` |
| Serial writes: every chunk is audited (`device.modify`) before it reaches the port and its outcome after (`device.modify.done`/`.error` with `bytes_sent`; short write = error); an audit failure blocks the write; no write starts or is in flight after `Raw.Run` returns; a finished recorder refuses TX; writing needs a real (auditing) recorder | `TestRawTXAuditFailureBlocksWrite`, `TestRawNoPortWriteAfterRunReturns`, `TestCaseRecorderTXAfterFinishRefused`, `TestCaseRecorderAuditsWriteOutcome`, `TestRawWriteRefusedWithoutRecorder` |
| Serial ports open with DTR and RTS de-asserted; `--dtr`/`--rts` need `--allow-device-write` and are audited (`device.modify` `serial.line_state`) before the open | `TestOpenModeNeverLeavesModemLinesToDriverDefault`, `TestSerialConsoleModemLinesOffByDefault`, `TestSerialConsoleModemLinesNeedAllowWrite`, `TestSerialConsoleModemLinesAuditedBeforeOpen` |
| Image analysis never modifies source evidence: the parent artifact is opened read-only via `Case.OpenArtifact`, and its hash is unchanged after info/ls/stat/extract/unalloc | `TestExamineNeverModifiesSource`, `TestOpenArtifactReadOnly` |
| `OpenArtifact` refuses a parent whose size differs from its manifest record (integrity error, exit 4), a path that is not a regular file, and a manifest path that points outside the case | `TestOpenArtifactSizeMismatchIsIntegrityError`, `TestImageTamperedParentExits4`, `TestOpenArtifactRefusesNonRegularFile`, `TestOpenArtifactRefusesPathOutsideCase` |
| Image imports are hashed through `Capture` and record the original path and the segment index/total; a source inside the case (including Windows extended-length spellings) is refused | `TestImportRecordsOriginalAndSegments`, `TestImportRefusesInsideCase`, `TestImportRefusesExtendedLengthPathInsideCase` |
| A partially imported or inconsistent multi-segment image (missing last segment, disagreeing totals, no total) cannot be opened; re-import it | `TestOpenPartialOrInconsistentImportIsIntegrityError` |
| Derived artifacts record full provenance (parent id + sha256, partition, filesystem type/path/id, image runs; every segment of a split parent) | `TestExtractRecordsProvenance`, `TestExtractFromSplitImageRecordsAllParentSegments` |
| Extracted bytes equal the image bytes at the recorded runs | `TestExtractRunsReproduceContent` |
| `case verify` flags a derived artifact whose parent (or any parent segment) is missing or whose recorded parent hash differs | `TestVerifyFlagsDerivedArtifactWithBadParent`, `TestVerifyFlagsDerivedArtifactWithBadParentSegment` |
| Deleted entries are listed and flagged, never extracted as live files (the skip is an `analysis.warning`; recovery is sub-project 3) | `TestExtractSkipsDeletedWithWarning` |
| Extraction from an incomplete parent is flagged `parent_incomplete` on the derived artifacts | `TestExtractFromIncompleteParentIsFlagged` |
| A failure to write to the case aborts an analysis; it is never downgraded to a per-file warning | `TestExtractCaseWriteFailureIsNeverDowngradedToWarning` |
| Hostile images never panic the process: panics in container, partition-table and filesystem parsers are recovered into errors, and the parsers are fuzzed | `TestDetectRecoversPanic`, `TestSessionRecoversFSPanic`, `TestOpenRecoversContainerAndTablePanics`, `FuzzRead`, `FuzzOpen` |

Serial modem lines: on Linux/macOS the kernel may still pulse DTR/RTS during
`open()` and drops them on close (HUPCL); userspace can only de-assert them
right after open. Treat a serial open as able to reset DTR/RTS-wired targets.

## 5. Architecture rule (enforced by `TestArchitectureDependencyRule`)
- `internal/evidence` and `internal/version` import no other Minutiae package except `evidence → version`.
- `internal/device` imports only `evidence`, `version`.
- Backends (`transport/serial`, `protocol`, `android`, `ios`) import only `device`, `evidence`, `version` and their own sub-packages; never `cli`, never each other.
- `internal/android/adb` imports no Minutiae package.
- `cmd/minutiae` imports only `internal/cli`.
- Parser packages `internal/image`, `internal/volume` and `internal/filesys` import no Minutiae package: they work on `io.ReaderAt` and can never write to a case, so they never import `evidence`.
- Parser test helpers (`volume/volumetest`, `filesys/fstest`) import only their parent package; `internal/filesys/detect` imports only `filesys` (plus each filesystem package as it lands).
- `internal/examine` is the only bridge between the parsers and the case: it imports `evidence`, `version`, `device`, `image`, `volume`, `filesys` and `filesys/detect`.
- `internal/cli` may import anything.

## 6. Testing
- TDD: write the failing test, see it fail, implement, see it pass.
- Default tests never need hardware: use fakes (fake ADB server, in-memory serial, fake iOS).
- Hardware tests use `//go:build hardware` and are run manually: `go test -tags hardware ./...`.
- Parsers have synthetic in-memory builders (`volumetest`, `fstest`) and fuzz targets; fuzz seeds run under `go test`, `go test -fuzz` is run manually.
- Real-image fixtures (`*.img.gz` plus an independent oracle `*.expect.json`) are committed under each package's `testdata/`. Regenerating them needs Docker (`docker build -t minutiae-fixtures tools/fixtures`, then `gen.sh all` in the container; see `tools/fixtures/README.md`); running the tests never does.

## 7. Git
- Use `git` only. Never `gh` (not installed). For GitHub use the web UI or REST API via `curl`.
- Never commit `cases/`, real evidence, or device dumps. `docs/superpowers/` stays local.
- Commit after each green task.
- Never name competing forensic tools or their vendors anywhere: docs, code, comments, tests, commit messages, specs or plans (not even in a deny-list). Describe capabilities generically ("commercial forensic suites"). Review every diff for this before committing.

## 8. Release build
```bash
go build -ldflags "-X github.com/rbenzing/minutiae/internal/version.Version=v1.0.0 -X github.com/rbenzing/minutiae/internal/version.Commit=$(git rev-parse --short HEAD)" -o bin/minutiae.exe ./cmd/minutiae
```

## 9. Known limitations
- Android sync v1: `LIST` of an unreadable directory returns `DONE` with no entries (indistinguishable from an empty directory, so no `acquire.warning` is possible), and `LIST`/`STAT` report sizes and mtimes as 32-bit values (sizes of files ≥ 4 GiB are truncated in listings and in `source.remote_size`; `RECV` still transfers every byte).
- Android hostile device: a malicious or faulty device can stream an unbounded number of `LIST` entries, unbounded directory depth, and arbitrarily large files (`RECV` size is inherent to evidence, so it is not capped). The examiner can stop an acquisition with Ctrl-C: partial artifacts are kept and flagged incomplete.
- iOS backup: the sync-lock / `notification_proxy` step is not performed, and `Info.plist` is not generated; the lockdown values are saved as `device/lockdown.json`.
- iOS backup transfer: a `0x0b` (remote error) block that follows file data is treated as the normal end of that file, mirroring libimobiledevice, so a device-side read failure in the middle of a file can look like a complete file. Files cut off by a transport failure or cancellation are kept and flagged `incomplete`.
- iOS encrypted backups (a backup password set on the device) are captured as-is; decryption belongs to roadmap sub-project 10.
- iOS AFC (`ios ls`/`ios pull`) parsing robustness depends on go-ios: its panics are converted to errors and a pull whose byte count differs from the AFC-reported size is flagged `incomplete`, but malformed AFC responses are otherwise not validated by Minutiae.
- iOS lockdown/service setup: plists the device returns during lockdown `GetValue` and `StartService` (used by `ios info`, `ios ls/pull` and `ios backup` setup) are decoded inside go-ios without Minutiae's bplist validator or panic recovery, so a hostile device could crash the process at that stage (before any artifact is written).
- Image analysis (sub-project 2, in progress): filesystem readers arrive progressively (ext4, FAT/exFAT, F2FS, E01, APFS, HFS+). Until the ext4 plan lands the binary has no filesystem driver (`detect.Drivers` is empty; the only filesystem parser is the MTFS test filesystem in `filesys/fstest`, used by tests), so `image info` shows partitions but no filesystem, and `image ls`/`stat`/`extract` and `unalloc -p` report that none is recognized. Raw and split-raw images, MBR/EBR and GPT partition tables, and `image unalloc --volume` (gaps outside partitions) work now.
- EWF/E01 images are detected but not yet opened (`image.ErrUnsupportedContainer`; plan 2E), and `image info --verify` only reports that verification is unavailable (raw images have no stored hashes).
- One `image import` call imports exactly one image (its files are segments 1..N in the order given; nothing is sorted). A partially imported image cannot be opened: re-import it.
- Deleted filesystem entries are listed (`image ls --deleted`) and flagged, but not recovered or extracted; recovery is sub-project 3.
- Encrypted filesystem content is extracted as ciphertext (flagged `Derived.Encrypted`); decryption belongs to roadmap sub-project 10.
- Regenerating the real-image fixtures needs Docker; running the tests never does.
- Hardware: as of v1.0.0 the device backends (ADB, iOS, serial) have NOT been validated on real hardware, only against fakes. Before relying on them in casework, run the hardware acceptance steps with authorized/trusted devices attached and confirm `case verify` reports OK:
  ```bash
  go test -tags hardware ./... -v
  ./bin/minutiae.exe case new --dir ./cases --id HW1 --examiner "<name>"
  ./bin/minutiae.exe android logical --serial <s> --case ./cases/HW1
  ./bin/minutiae.exe ios backup --udid <u> --case ./cases/HW1
  ./bin/minutiae.exe serial console --port <p> --baud <n> --case ./cases/HW1
  ./bin/minutiae.exe case verify --case ./cases/HW1
  ```
