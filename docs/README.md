# Minutiae documentation

Find your question below; the document it names has the rest. Read only what your change
touches: the reference docs are split by area so that no task needs all of them.

| I want to know… | Go to |
|---|---|
| The project rules: process, forensic principles, definition of done | [../CLAUDE.md](../CLAUDE.md) |
| How to use the tool | [../README.md](../README.md) |
| What is built, what is next, and which sub-project my work belongs to | [ROADMAP.md](ROADMAP.md) |
| What must never be broken, and which tests pin it | [invariants.md](invariants.md), then the area file |
| What the build does not do yet, or does only approximately | [limitations.md](limitations.md), then the area file |
| Which formats were written from memory | [limitations.md](limitations.md) ("Formats written from memory") |
| Which package may import which; purity, single-writer and cgo rules | [architecture.md](architecture.md) |
| Why a costly choice was made, and what else was considered | [adr/](adr/README.md) |
| How one subsystem works inside | [systems/](systems/README.md) |
| How to run the checks, write tests, fuzz and build a release | [testing.md](testing.md) |
| What the records database and the parsers measured at scale | [performance.md](performance.md) |
| How the real-image fixtures are built, and their oracles | [../tools/fixtures/README.md](../tools/fixtures/README.md) |
| The design and the task plan of a sub-project | `docs/superpowers/specs/` and `docs/superpowers/plans/` (local only, gitignored; listed in [ROADMAP.md](ROADMAP.md)) |

## What to read for a change

Always: [../CLAUDE.md](../CLAUDE.md), and the sub-project's spec and plan. Then, by the code you
change:

| You change… | Read |
|---|---|
| `internal/evidence` (the case core) or anything that writes to a case | [invariants/case](invariants/case.md), [limitations/case](limitations/case.md) |
| `internal/device`, `internal/transport/serial`, `internal/protocol`, `internal/android`, `internal/ios` | [invariants/acquisition](invariants/acquisition.md), [limitations/acquisition](limitations/acquisition.md) |
| `internal/image`, `internal/image/ewf`, `internal/volume`, import and extract in `internal/examine` | [invariants/images](invariants/images.md), [limitations/images](limitations/images.md) |
| A filesystem reader under `internal/filesys/<fs>` | [invariants/filesystems](invariants/filesystems.md) and that reader's file ([ext4](invariants/ext4.md), [fat](invariants/fat.md), [f2fs](invariants/f2fs.md), [hfsplus](invariants/hfsplus.md), [apfs](invariants/apfs.md)), and the same two in [limitations](limitations.md) |
| `internal/filesys`, `filesys/detect`, `filesys/utf8fold`, `filesys/fstest` | [invariants/filesystems](invariants/filesystems.md), [limitations/filesystems](limitations/filesystems.md) |
| Recovery, carving or slack: `internal/carve`, a reader's `Recoverer`, recovery in `internal/examine` | [invariants/recovery](invariants/recovery.md), [limitations/recovery](limitations/recovery.md), and the reader's files |
| `internal/sqlitefile`, `internal/decode/sqlitedb/...`, `internal/dbrecover` | [invariants/sqlite](invariants/sqlite.md), [limitations/sqlite](limitations/sqlite.md) |
| `internal/records`, or the record tables and full-text index in `internal/evidence` | [invariants/records](invariants/records.md), [limitations/records](limitations/records.md); [performance.md](performance.md) for scale work |
| `internal/parse`, `internal/artparse`, `internal/recordtypes`, `internal/decode/{ts,plist,typedstream,iosbackup,tl}`, `internal/parsers/...` | [invariants/parsers](invariants/parsers.md), [limitations/parsers](limitations/parsers.md); add [sqlite](invariants/sqlite.md) when a parser maps tables |
| `internal/cli` | The area of the command you change |
| A new package or a new import edge | [architecture.md](architecture.md) |
| `tools/check`, `tools/fuzzcampaign`, `tools/parserhash`, `tools/recoveryaccept`, how tests or fixtures run | [testing.md](testing.md) |

## Kinds of document

| Kind | Documents | Changes how |
|---|---|---|
| Reference: the product as it is now | `invariants/`, `limitations/`, `architecture.md`, `testing.md`, `systems/` | Edited in the same commit as the code it describes |
| Decision log | `adr/` | Appended; an accepted record is superseded, never rewritten |
| Status | `ROADMAP.md` | The only place delivery status lives |
| Measurements | `performance.md` | Runs are added with their machine and commit; its machine-readable record is checked by `TestPerformanceRecordIsConsistent` |
| Specs and plans | `docs/superpowers/` (local only) | Per sub-project: spec, then plan, then implementation |

## The rules that keep this working

**One fact, one place.** A doc set rots when a fact is written twice, one copy is updated, and
the two contradict each other. When you are about to restate something, link to it instead.

**Status lives in the roadmap.** Whether something is done, pending or next belongs in
[ROADMAP.md](ROADMAP.md), not in a reference doc.

**A rule names what enforces it.** An invariant names its tests; a convention says it is held
by review.

**Split by area, index at the top.** When an area file grows too long to read in one sitting,
split it by package (as the filesystem files are) and add the new file to its index in the same
commit. Moved text stays word for word.

**Links resolve.** `TestDocLinksResolve` (`internal/archtest`) fails on any relative link in
these files that does not resolve.

**Never name competing forensic tools or their vendors**, in any document. Say "commercial
forensic suites".
