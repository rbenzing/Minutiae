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
| Image analysis never modifies source evidence: the parent artifact is opened read-only via `Case.OpenArtifact`, and its hash is unchanged after info/ls/stat/extract/unalloc | `TestExamineNeverModifiesSource`, `TestExtractNeverModifiesSource`, `TestOpenArtifactReadOnly` |
| `OpenArtifact` refuses a parent whose size differs from its manifest record (integrity error, exit 4), a path that is not a regular file, and a manifest path that points outside the case; an id (or path) held by more than one manifest record is an integrity error, while other open errors (permission, descriptor exhaustion) are plain I/O errors, not integrity errors | `TestOpenArtifactSizeMismatchIsIntegrityError`, `TestImageTamperedParentExits4`, `TestOpenArtifactRefusesNonRegularFile`, `TestOpenArtifactRefusesPathOutsideCase`, `TestOpenAndFindRefuseDuplicateManifestRecords`, `TestOpenErrorClassification` |
| Image imports are hashed through `Capture` and record the original path and the segment index/total; a source inside the case (including Windows extended-length spellings) is refused | `TestImportRecordsOriginalAndSegments`, `TestImportRefusesInsideCase`, `TestImportRefusesExtendedLengthPathInsideCase` |
| A partially imported or inconsistent multi-segment image (missing last segment, disagreeing totals, no total) cannot be opened; re-import it | `TestOpenPartialOrInconsistentImportIsIntegrityError` |
| Derived artifacts record full provenance (parent id + sha256, partition, filesystem type/path/id, image runs; every segment of a split parent) | `TestExtractRecordsProvenance`, `TestExtractFromSplitImageRecordsAllParentSegments` |
| Extracted bytes equal the image bytes at the recorded runs | `TestExtractRunsReproduceContent` |
| `case verify` flags a derived artifact whose parent (or any parent segment) is missing or whose recorded parent hash differs | `TestVerifyFlagsDerivedArtifactWithBadParent`, `TestVerifyFlagsDerivedArtifactWithBadParentSegment` |
| `case verify` compares every manifest record's full provenance (`source`: derivation, runs, segments, original path ...) with the `source` in its hash-chained `artifact.create` audit entry, so rewriting provenance in `manifest.jsonl` is detected (exit 4) | `TestVerifyDetectsSourceDifferingFromAudit`, `TestCaseVerifyTamperedDerivationExits4` |
| `extract` never expands a sparse file whose hole bytes exceed the partition length (skipped with an `analysis.warning`, counted as skipped) | `TestExtractSkipsSparseFileLargerThanPartition`, `TestExtractSmallSparseAndInlineFilesAreExtracted` |
| Deleted entries are listed and flagged, never extracted as live files (the skip is an `analysis.warning`; recovery is sub-project 3) | `TestExtractSkipsDeletedWithWarning` |
| Extraction from an incomplete parent is flagged `parent_incomplete` on the derived artifacts | `TestExtractFromIncompleteParentIsFlagged` |
| A failure to write to the case aborts an analysis; it is never downgraded to a per-file warning | `TestExtractCaseWriteFailureIsNeverDowngradedToWarning` |
| Hostile images never panic the process: panics in container, partition-table and filesystem parsers are recovered into errors, and the parsers are fuzzed | `TestDetectRecoversPanic`, `TestSessionRecoversFSPanic`, `TestOpenRecoversContainerAndTablePanics`, `FuzzRead`, `FuzzOpen`, `FuzzExt4Open` |
| Filesystem readers match independent oracles from real images: for each committed `mke2fs`-built fixture the ext4 reader's live tree (exact path set, entry types, sizes, modes, mtimes, symlink targets, SHA-256 of every file's content) and the deleted names the generator removed equal the recorded expectations, and `Unallocated` equals exactly the free-block ranges `dumpe2fs` reports for every fixture, including the multi-group `meta_bg`+`flex_bg`+`uninit_bg` one (`ext4-1k-metabg-uninit`) (no free run overlaps live content or group metadata); extra stale deleted records are logged, not asserted | `TestExt4MatchesOracle` |
| ext4 deleted directory entries are listed and flagged `Deleted` (names only, no content), and a stale htree copy of a live record is flagged `stale_copy=true`, never presented as live | `TestReadDirFlagsDeletedEntries`, `TestStaleCopyOfLiveRecord` |
| Filesystem anomalies found during an analysis are written to the audit log as `analysis.warning`: source `filesystem-open` for those present when the filesystem was opened, `filesystem` for later ones, each once; they are counted separately from skipped files and are written even when the analysis fails | `TestUnallocForwardsFilesystemWarnings`, `TestExtractForwardsFilesystemWarningsOnce`, `TestFilesystemWarningsAreCountedNotSkipped`, `TestFilesystemWarningsReachAuditWhenAnalysisFails` |

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
- Parser test helpers (`volume/volumetest`, `filesys/fstest`) import only their parent package; `internal/filesys/detect` imports only `filesys` and the filesystem packages it probes (today `filesys/ext4`); `filesys/ext4` imports only `filesys`, and its test builder `ext4/ext4test` only `filesys` and `ext4`.
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
- Image analysis (sub-project 2, in progress): ext4, ext3 and ext2 are recognized (`detect.Drivers` holds the ext4 driver), so `image info` shows their filesystem and `image ls`/`stat`/`extract`/`unalloc -p` work on them. The other filesystem readers arrive progressively (FAT/exFAT, F2FS, E01, APFS, HFS+); until they land, those partitions are listed but `image ls`/`stat`/`extract` and `unalloc -p` report that no filesystem is recognized. Raw and split-raw images, MBR/EBR and GPT partition tables, and `image unalloc --volume` (gaps outside partitions) work.
- EWF/E01 images are detected but not yet opened (`image.ErrUnsupportedContainer`; plan 2E), and `image info --verify` only reports that verification is unavailable (raw images have no stored hashes).
- One `image import` call imports exactly one image (its files are segments 1..N in the order given; nothing is sorted). A partially imported image cannot be opened: re-import it.
- Deleted filesystem entries are listed (`image ls --deleted`) and flagged, but not recovered or extracted; recovery is sub-project 3.
- `image ls`/`stat`/`extract` check the parent artifact's size only (`OpenArtifact`), not its hash: a same-size content swap of the parent is not caught at open time, but `case verify` detects it (derived artifacts record the parent's SHA-256, and the parent is re-hashed).
- Sparse-expansion guard: `extract` skips a file whose hole bytes (sum of `Offset -1` runs, or the whole size when the filesystem reports no runs) exceed the partition length, with an `analysis.warning` ("sparse size exceeds partition length; not extracted"). A sparse file within the partition length is still expanded to its full size, and overlapping data runs are not bounded by this guard.
- Encrypted filesystem content is extracted as ciphertext (flagged `Derived.Encrypted`); decryption belongs to roadmap sub-project 10.
- ext4 (ext2/3/4) reader, read-only and as found: the journal is not replayed, so an image that needs recovery is read as it lies on disk (the reader warns that the filesystem may be inconsistent); journal analysis belongs to sub-project 3. `bigalloc`, an external journal device and unknown incompatible features are refused as unsupported.
- ext4 deleted entries are names only: a deleted directory record is listed and flagged (`image ls --deleted`) with what the on-disk record still holds, but its content is not recovered or extracted (sub-project 3). A stale copy of a live record left behind by an htree leaf split stays listed with attribute `stale_copy=true`.
- ext4 names: encrypted (FBE) directory names are shown as `~enc~<base64url of the raw name>` and their file content is extracted as ciphertext (decryption is sub-project 10); names that are not valid UTF-8 are shown as `~raw~<base64url>`. Path lookup accepts both displayed forms.
- ext4 casefold directories: name lookup is approximate (`strings.EqualFold` on valid UTF-8 names, not the kernel's casefold table), so a path may resolve differently than on a live system; directory listing is unaffected.
- ext4 directory record cap: at most 262144 records are read per directory (live and deleted). A lookup beyond the cap, or in a damaged region of a directory, returns not found with a warning, not an error. Extended attributes are read from the inode and the xattr block, but xattr block checksums are not verified and values held in an EA inode (`ea_inode`) are not read.
- ext4 unallocated export: a block group whose bitmap fails its checksum (or cannot be read) is omitted from the export and reported as a filesystem warning (`analysis.warning`); its blocks are neither exported nor claimed as free. Any group whose descriptor checksum is wrong is skipped whatever its flags (`TestBadDescriptorChecksumGroupIsSkippedWhateverItsFlags`), and when the descriptor table is truncated the `BLOCK_UNINIT` groups are skipped too (`TestUnreadableDescriptorsSkipBlockUninitGroups`), each with a warning. The scan for deleted names in directory slack is capped at 16 MiB of slack per directory (`TestSlackScanCap`); past the cap the reader warns and stops searching, so a hostile directory cannot cost unbounded time.
- Analysis warning ordering: a filesystem warning is written to the audit log as `analysis.warning` after the artifact whose processing triggered it (never before): the reader notices an anomaly only while doing that work. Warnings about directories met while walking are attributed through the `after` field, which names the last extracted path, not the directory itself. Warnings the filesystem already had when the analysis began are written once as `source: filesystem-open`; later ones as `source: filesystem` and counted in the summary (`fs_warnings`) apart from skips.
- Filesystem readers keep at most 1000 distinct warnings (de-duplicated by text; ext4 then adds one "further warnings suppressed" note), so the audit log is not an exhaustive list of every anomaly in a hostile image. `image ls`/`stat` print the warnings a command raised (escaped, at most 20 lines) on stderr; `image stat --json` carries them in a top-level `warnings` field.
- ext4 hard links: every directory path to a multiply-linked inode is a separate entry, so `extract` writes one copy per path (the entries share the filesystem id, so provenance shows they are the same inode). Directory entries stored beyond a directory's `i_size` are listed as deleted entries with attribute `beyond_isize=true` (`TestEntriesBeyondISizeAreDeletedNotLive`). A live directory entry whose inode has `links_count` 0 and a `dtime` gets attribute `inode_freed=true` and a warning (`TestLiveEntryOfFreedInode`).
- A file whose extents run past the end of a truncated image cannot be read in full: `extract` skips it with an `analysis.warning` rather than extracting a prefix. Local artifact names are capped at 200 bytes per path component (cut on a rune boundary, then `~` plus the first 8 hex digits of the SHA-256 of the original name, so distinct long names stay distinct; this applies to `image extract` and `android logical` alike); the original path is kept in `Source.RemotePath` and `Derived.FSPath`. An OS path-syntax or name-too-long error creating one artifact becomes a per-file `analysis.warning` and a skip.
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
