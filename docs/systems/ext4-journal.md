# ext4 journal (jbd2): system design

Reads, reports and recovers from the jbd2 journal of an ext2/3/4 volume: the transactions that
are still in the log, live (what the kernel's mount would replay) or stale (what an earlier use
of the log left beyond the live region). It never replays the journal and never lets it reach the
live view of the filesystem. The deliberate sibling is [F2FS roll-forward](f2fs-rollforward.md).

**Lives in:** `internal/filesys/ext4` (`journal_super.go`, `journal_scan.go`, `journal_stale.go`,
`journal_view.go`, `recover_journal.go`, with the builder in `ext4test/journal.go`), the optional
interfaces `filesys.Journaler` and `filesys.JournalRecoverer`, `internal/examine` (`journal.go`,
`journal_jbd2.go`, `journal_report.go`, `journal_blocks.go`, `verify_journal.go`) and
`internal/cli` (`image_journal.go`, `image_journal_jbd2.go`, the `--use-journal` flag of
`image_recover.go`).

## The model

- `jsuper` is the journal superblock (v1 or v2, 32- or 64-bit tags, checksum version, the
  fast-commit area size). `journalMap` maps a journal block to a filesystem block through inode
  8, read through the same extent code as live files but quietly (no warnings, no caches shared
  with the live view).
- `jindex` is the journal as decoded once per `FS` (`FS.journalIndex`, mutex-guarded; a corrupt,
  unsupported or over-limit result is cached, an I/O error is not). It holds `jtxn` (sequence,
  region live or stale, start and commit block, commit and checksum state), `jtag` (the logged
  filesystem block, the journal slot, flags, copy state, revoke state) and the revoke list, plus
  the bitmap of consumed journal blocks.
- A tag's copy state is `verified`, `unverified` (no tag checksum), `mismatch`, `missing` (slot
  not available) or `outside` (block beyond the filesystem). A transaction's checksum is the
  commit block's; a transaction with no commit block is `not-recorded`.
- `filesys.JournalTxn`/`JournalInfo` are the generic view; `Region` is `live` or `stale`, a
  sequence may name one live and one stale transaction (`filesys.ErrAmbiguous`).
- Artifact kinds: the report (`journal-report`, `reports/p<N>-ext4/journal.jsonl`, method
  `ext4-journal`), block copies (`journal-block`), both scope `journal`; and recovered files of
  method `ext4-journal-inode` (class `deleted-file`, confidence at most 40 on a real image).

## How it runs

1. `openJournal` locates inode 8 (or refuses: an external journal device and unknown incompatible
   features are `ErrUnsupported`), checks the superblock and builds the block map.
2. The live scan starts at `s_start` with `s_sequence` and follows the kernel's rule: a block
   whose magic or sequence is wrong ends the region; descriptors and revokes are decoded, data
   slots are consumed, a commit block closes the transaction. A damaged revoke or descriptor
   stops the scan and marks the journal incomplete. Caps (`cap-txns`, `cap-tags`, `cap-revokes`)
   and a read past the image end are named stops, also incomplete.
3. The stale pass reads the fragments the live scan did not consume, starting at the first block
   it can read; a stale transaction is committed only if its commit block says so, and a stale
   tag whose slot was overwritten shows as `mismatch` where a checksum exists and `unverified`
   where it does not. The pass is skipped when the live scan hit a cap.
4. Revokes: a revoke record applies to a logged block of a committed transaction of equal or
   lower sequence; the tag keeps its own revoked flag.
5. `PrepareJournalRecovery(use)` indexes the copies of every inode-table block once.
   `journalCandidates` then offers, per deleted entry, the newest live-state copy of the same
   generation (see the invariants row) with its map blocks taken from journal copies not newer
   than the inode copy. Rejections that leave the answer unknown give `ErrUndecided`, not "no
   candidate". Caps and confidence are applied by `examine` (at most 40 on a real image).
6. `examine`: `JournalList`/`JournalTxns` read only; `JournalReport` renders deterministic JSONL
   (`journal`, `txn`, `tag`, then `end` or `cut`), every transaction marked `"applied":"unknown"`;
   `JournalBlocks` copies the exact bytes of the log slots of ONE transaction, skipping a slot
   that lies in free space and marking the copy `alloc-basis=journal-inode` plus
   `bitmap-unconfirmed` when the bitmap view is unknown (a warned filesystem; rulings C15, C17).
   `RecoverOptions.UseJournal` (`none`, `committed`, `stale`) makes the 3A planner add the journal
   source.
7. `case verify` (`JournalCheck`, R13, the last composed check) renders every report again,
   requires every block copy to be a tag of a fresh transaction with the recorded sequence,
   region, commit state, slot and markers, and every `ext4-journal-inode` file to match a fresh
   journal candidate of the recorded sequence and region. R6 and R9 still reproduce and recheck
   what they can.
8. CLI: `image journal ls|show|report|blocks <ref> [-p N] [--stale|--no-stale] [--region
   live|stale]` and `image recover <ref> --use-journal none|committed|stale`; see the README.

## Invariants

[invariants/recovery](../invariants/recovery.md) holds the rows (search "jbd2" and "ext4 journal");
they name the tests. The hostile-input bounds are the scan caps, the journal size bound
(`maxJournalBytes`), the consumed-block bitmap (a cycle is a stop) and per-entry copy bound
(`maxJournalCopiesPerEntry`, which makes the source unknown, not empty).

## Interactions

`ext4` imports only `filesys`. `examine` reaches it through `filesys.As` to `Journaler` and
`JournalRecoverer`, never by importing `ext4` (no new import edge,
[architecture.md](../architecture.md)). The CLI imports `examine`. `evidence` knows only the class
table (`journal-block` and `journal-report` take scope `unallocated` or `journal`) and the kinds.

## Cost and bounds

One pass of at most the journal's blocks (a journal of more than 4 GiB is refused), one block read
at a time with a one-block peek cache. `PrepareJournalRecovery` reads the index again for the
copies, once per run. A report is cut by `--max-bytes` and flagged incomplete.

## Where it breaks

- The journal is history. A transaction never says it was applied: `applied` is unknown, and
  nothing here is merged into `image ls`.
- Fast commits are not decoded, so such a journal is incomplete and its tail is unknown.
- Without tag checksums a stale copy cannot be told from an overwrite.
- A reader change that alters a transaction or tag field changes the report rendering, so older
  reports then fail R13 by design.
- Files that exist only in unreplayed transactions are listed in the report and not recovered.
- The format knowledge is from memory, confirmed on two kernels
  ([limitations/recovery](../limitations/recovery.md), "ext4 journal (plan 3G)").
