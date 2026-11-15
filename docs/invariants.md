# Forensic invariants

Each invariant is enforced by the named tests: keep them green. The rows are split by area, so
a change reads the rows of the code it touches and not the whole set. Known gaps and from-memory
formats are in [limitations.md](limitations.md); the principles every row comes from are in
[CLAUDE.md](../CLAUDE.md) ("Forensic principles").

## How to use it

- Before changing a package, read its area file below. A change that writes to the case (an
  artifact, a record, an audit entry) or touches `case verify` also reads [case](invariants/case.md).
- A change that adds or alters an invariant adds or edits its row in the area file, in the same
  commit. A new area file is added to the table below in that commit too.
- When the code and a row disagree, stop and flag it. Never silently pick one.
- A row lives in the area whose code enforces it. A row that another area needs is linked, never
  copied.

## Areas

| Area | Covers |
|---|---|
| [case](invariants/case.md) | Artifacts, the manifest, the audit log, the case lock, anchors, `OpenArtifact`, the core of `case verify` |
| [acquisition](invariants/acquisition.md) | Device writes, serial, Android ADB and sync, iOS AFC and backup |
| [images](invariants/images.md) | Raw and E01 images, import, `image info --verify`, `extract`, derived-artifact provenance |
| [filesystems](invariants/filesystems.md) | Rules every filesystem reader shares: detection, entry IDs, names, warnings, hostile input |
| [ext4](invariants/ext4.md) | The ext2/3/4 reader |
| [fat](invariants/fat.md) | The FAT12/16/32 and exFAT readers |
| [f2fs](invariants/f2fs.md) | The F2FS reader |
| [hfsplus](invariants/hfsplus.md) | The HFS+ and HFSX reader, decmpfs |
| [apfs](invariants/apfs.md) | The APFS reader and snapshots |
| [recovery](invariants/recovery.md) | Deleted-file recovery, carving, slack, the R-series checks of `case verify` |
| [sqlite](invariants/sqlite.md) | The SQLite file library, the table decoder, `db_row` record recovery |
| [records](invariants/records.md) | The records database, the P-series checks of `case verify`, full-text search, stats, scale |
| [parsers](invariants/parsers.md) | The parse contract and host, record types, decoders, the artifact parsers |
