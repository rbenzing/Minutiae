# F2FS roll-forward: system design

Finds, reports and recovers the data that the F2FS kernel driver wrote after the last checkpoint and
that only its mount-time replay would apply: the fsync'd node chain in the warm-node log. It never
replays that chain and never lets it reach the live view of the filesystem.

**Lives in:** `internal/filesys/f2fs` (`rollforward*.go`, with the builder helpers in `f2fstest`),
the optional interfaces `filesys.RollForwarder` and `filesys.Journaler`, `internal/examine`
(`journal*.go`, `recover_rollforward.go`, `verify_rollforward.go`) and `internal/cli`
(`image_journal.go`, the `--rollforward` flag of `image_recover.go`).

## The model

- `chainEntry` (20 bytes: block, nid, ino, flag word, next) is one node of the chain; `rfChain`
  holds the entries plus the per-inode replay facts `live`, `created` and `lastFsync`. It is built
  once per `FS` (`FS.chain`, mutex-guarded; a corrupt result is cached, an I/O error is not).
- `filesys.RollForwardChain` is the public summary (start, stop, `Complete`, node count, checkpoint
  flags, warnings). `RollForwardNode` carries the per-node facts, `PostCheckpointFile` a file the
  replay would create, with its `Candidate` (the runs the 3D map machinery produces).
- A post-checkpoint file has the ID `rollforward:<ino>`. It is not an `Entry` of the tree.
- Three artifact kinds: the report (`journal-report`, `reports/p<N>-f2fs/rollforward.jsonl`), block
  copies (`journal-block`) and recovered files (class `post-checkpoint-file`, method
  `f2fs-rollforward`).

## How it runs

1. `buildChain` starts at the chosen checkpoint's warm-node cursor (`curNode[1]`) and follows each
   footer's `next` address. A block is taken only if its 64-bit footer version equals what
   `checkpoint.footerVersion` computes (the CRC half is in play under `CP_CRC_RECOVERY_FLAG`). Every
   other end is a named stop: `version@`, `outside-main@`, `cycle@`, `footer@`, `image-end@`,
   `cap@`. Only the last three (and `footer@`) make the chain incomplete.
2. `deriveChain` classifies each node by `nodeKind(ofs)` and applies the replay rules:
   `wouldReplay(i)` holds for a dnode of an inode that is live or created (fsync inode node with the
   dentry mark), at or before its last fsync dnode. `fileKind` names each inode number:
   post-checkpoint, live update or not replayed.
3. `PostCheckpointFiles` turns each created inode into a candidate: the inode is the last inode node
   at or before the last fsync, and `chainSource.nodeFor` serves only direct nodes of that owner up
   to the same point, so the 3D `staleMap` builds the block map. A file that needs an indirect node
   stops there as a prefix (`node-missing@`). Parent and name come from the last dentry-marked inode
   node; the parent must be a live directory.
4. `Journal`, `JournalTransactions` and `JournalBlock` present the chain as one transaction in the
   generic `Journaler` view; `JournalBlock` re-resolves the transaction and never trusts it.
5. `examine`: `JournalList`/`JournalShow` read only; `JournalReport` writes the deterministic JSONL
   report (field order fixed, address words as `[first,count]` runs); `JournalBlocks` copies a node
   block only when the checkpoint view reports it free; `RecoverOptions.RollForward` makes the 3A
   planner enumerate `PostCheckpointFiles` instead of deleted entries, caps by `capRollForward`
   (prefix or cut 20, overlap 30, chain cut 20) and sets `FSPath` by one bounded live walk.
6. `case verify` (`RollForwardCheck`, R12) renders each report again with the recorded limit and
   compares hash and size; requires each block copy to be a node of the chain with the recorded
   sequence and basis, and each recorded file to be a post-checkpoint file whose runs are a prefix
   of its map now. R6 and R9 reproduce and recheck the copies as for any recovered artifact.
7. CLI: `image journal ls|show|report|blocks <ref>` and `image recover <ref> --rollforward
   (--all | id:rollforward:<ino>)`; see the README.

## Invariants

[invariants/recovery](../invariants/recovery.md) holds the rows (search "roll-forward"); they name
the tests. The hostile-input bounds are the node cap and the per-node footer checks.

## Interactions

`f2fs` imports only `filesys`. `examine` reaches it through `filesys.As` to `RollForwarder` and
`Journaler`, never by importing `f2fs` (no new import edge, [architecture.md](../architecture.md)).
The CLI imports `examine`. `evidence` knows only the class table (ceiling 60) and the artifact kinds.

## Cost and bounds

At most `1<<20` nodes of 20 bytes (20 MiB) and never more than the main-area block count. Each step
reads one 4 KiB block. The per-inode index is built once per `FS`. A report is cut by `--max-bytes`
and flagged incomplete.

## Where it breaks

- "Unclean checkpoint" is not the signal of a chain: the fixture's checkpoint is an unmount
  checkpoint followed by a chain. Read `Journal().NeedsRecovery` or `image journal ls`.
- The replay rules and the chain rule are from memory of the kernel, confirmed on one kernel
  (`docs/limitations/recovery.md`, "F2FS roll-forward").
- Indirect nodes are never in the warm log of a crash image, so large files are prefixes.
- A reader change that alters a node field changes the report rendering, so older reports then fail
  R12 by design.
- Post-checkpoint data blocks are free in the checkpoint view, so unallocated export and carving can
  also see them; that is why the confidence ceiling is 60.
