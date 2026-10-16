# Records performance (sub-project 5, plan 5D)

Numbers recorded by `BenchmarkIngest`, `BenchmarkOpenArtifact`, `BenchmarkReadRunLines` and the manual
scale test (`go test -tags scale -run TestScale ./internal/records/`, see `docs/testing.md`). The JSON
block below is the record; `TestPerformanceRecordIsConsistent` recomputes every pass or miss from it and
requires each gated decision to follow the numbers. Nothing here is asserted by a timing test.

## Targets (spec section 11) and how each is measured
| Row | Target | Measured as |
|---|---|---|
| `ingest_rate` | >= 40,000 records/s | records divided by the time spent inside `Writer.Add` and `End` (generator CPU excluded) |
| `list_p95_ms` | <= 50 ms | p95 of a 100-row page over the cells of `matrix.list.clean.*` that hit a time-ordered index (none, one to three types, deleted, time window, type + time window, min confidence + type, a page at depth). The set includes `type2` and `type3`, whose plans end in `USE TEMP B-TREE FOR ORDER BY`: they do not get the time order from the index, so a "hit a time-ordered index" cell is not always one that avoids a sort |
| `search_word_p95_ms` | <= 300 ms | max p95 over word and prefix searches (first 100 hits) with a type or a time filter |
| `search_substring_p95_ms` | <= 1000 ms | max p95 over `--substring` searches (first 100 hits) |
| `stats_type_s` | <= 3 s | `Overview` + `Stats --by type` + `IndexStatus`, the work of `records stats --by type` |
| `verify_rate` | >= 100,000 records/s | records checked by `case verify` (every check P1-P14, R-series, P11 included) divided by its wall time |
| `db_ratio` | <= 1.6 | `artifacts.db` bytes over the row data plus 64 bytes per row (exact definition below) |
| `open_artifact_ms` | <= 1 ms | plan threshold (not a spec target): `OpenArtifact` with 10,000 manifest lines |

### `db_ratio`, exactly as measured (ruling D45)
`db_ratio = size of artifacts.db in bytes / (rowBytes + 64 * n)`, where `n` is the number of records
and `rowBytes` is the sum over every record of the bytes of its summary, body, payload JSON, source path
and locator (`recordstest.RowBytes`). The size is the file as `os.Stat` reports it after the ingest has
ended: free pages, the full-text indexes, every secondary index and the audit-side tables of the case
database are all inside it.

This differs from the spec's wording ("the sum of row data"): the spec does not say whether the fixed
columns of a row (id, type, artifact, times, confidence, flags) count as data. Counted as data they
would make the target easier, counted as overhead they would make it harder with no way to tell the two
apart. The measurement therefore adds a flat 64 bytes per row for those columns to the denominator, a
stated and constant allowance, and every run is judged against it. The number is not the spec's ratio
under another reading; it is this definition, and a change of it is a change of this document.

### Verify spill and the OS temp directory (rulings D46, D50, D52)
Implemented in the scale harness (5D-5b). The harness samples the size of `<case>/tmp` every 500 ms
while `case verify` runs (`verify_tmp_bytes`, and `verify_scratch_bytes` for `<case>/tmp/verify-*`
alone). Both are lower bounds and a sample of 0 is "not measured", never "nothing spilled": SQLite
spill files need not be visible as directory entries (they are unlinked on Linux and removed at close
on Windows). Spill is therefore measured two ways and the record says which one produced each number:

1. OS temp directory unchanged (`ostemp_added_<phase>`): the directory the process uses as its temp
   directory is listed (every file, with its size) before and after the `ingest`, `reindex` and
   `verify` phases, and the two listings must be equal. The run refuses to start unless `TMP`, `TEMP`
   and `TMPDIR` point to a directory under the scale directory and outside both cases, so the listing
   covers this run only. A difference fails the run; it is a leak of evidence text and a defect, not a
   number. A recorded 0 means "no file appeared in or vanished from the directory between the two
   listings" (the run record names the measured directory as `os_temp_dir`); a file created and removed inside the phase is not seen (the unit-level watcher in
   `internal/records/tempspill_test.go` polls for those, and its limit is stated there).
2. Size of the scratch space inside the case: `verify_scratch_bytes`, the largest size of
   `<case>/tmp/verify-*` the 500 ms sampler read while verify ran. It is read through directory
   listing only: a reading at cleanup or through open handles is not implemented, so a spill whose
   files are not visible by name stays "not measured".

### Peak memory (ruling D50)
`peak_rss_<phase>_bytes` is the peak resident set of the whole test process when the phase ended
(Windows: peak working set; Linux: `VmHWM`; other platforms: not measured). It is a high-water mark
of the process, so a phase run alone (ingest in one run, reindex in another on the copy, verify in
another) reads as that phase's peak, and a phase that follows others in one run reads as "at most this".
`temp_store = MEMORY` keeps SQLite temporary data in memory: if the peak at 10 million records does not
fit the machine, the decision (ruling D50) is to move the scratch space to `<case>/tmp` through
`temp_store_directory` and to document its one-case-per-process assumption beside `docs/limitations.md`
(full-text temporary files).

## Machine
Baseline (commit 2724e52): Intel i7-1265U (10 cores, 12 threads), 15.4 GB RAM, NVMe SK hynix PC801 512 GB, Windows 11 Pro, go1.26.8. The spec's reference machine is 8 cores, NVMe, 16 GiB; this one is close in size.

Background load: this is the user's working laptop, not an idle machine. The user's applications and Docker Desktop (WSL) were running during every run, so every figure is what the tool does beside normal desktop load. During the verify run free RAM briefly fell to about 910 MB; the test process held about 70 MB and the rest was outside Minutiae.

## Recorded numbers
```json minutiae-perf-v1
{
  "schema": "minutiae-perf-v1",
  "targets": [
    {"id": "ingest_rate", "op": ">=", "value": 40000, "unit": "records/s"},
    {"id": "list_p95_ms", "op": "<=", "value": 50, "unit": "ms"},
    {"id": "search_word_p95_ms", "op": "<=", "value": 300, "unit": "ms"},
    {"id": "search_substring_p95_ms", "op": "<=", "value": 1000, "unit": "ms"},
    {"id": "stats_type_s", "op": "<=", "value": 3, "unit": "s"},
    {"id": "verify_rate", "op": ">=", "value": 100000, "unit": "records/s"},
    {"id": "db_ratio", "op": "<=", "value": 1.6, "unit": "x"},
    {"id": "open_artifact_ms", "op": "<=", "value": 1, "unit": "ms"}
  ],
  "runs": [
    {
      "commit": "2724e52",
      "label": "baseline",
      "machine": "Intel i7-1265U 10 cores/12 threads, 15.4 GB RAM, NVMe SK hynix PC801 512GB, Windows 11 Pro, go1.26.8",
      "records": 10000000,
      "values": {
        "bench.ingest.batch1000_rate": 1969,
        "bench.ingest.batch20000_rate": 2814,
        "bench.ingest.batch5000_rate": 2452,
        "bench.read_run_lines_s": 1.469758,
        "db_ratio": 3.9014258415427174,
        "get_p95_ms": 172.345,
        "ingest_rate": 1450.9638038965286,
        "ingest_s": 6891.970684,
        "list_p95_ms": 266198.959,
        "matrix.list.clean.artifact_big.p50_ms": 88206.094,
        "matrix.list.clean.artifact_big.p95_ms": 92160.103,
        "matrix.list.clean.artifact_big.samples": 2,
        "matrix.list.clean.artifact_tiny.p50_ms": 0,
        "matrix.list.clean.artifact_tiny.p95_ms": 4.682,
        "matrix.list.clean.artifact_tiny.samples": 200,
        "matrix.list.clean.artifacts3.p50_ms": 181720.124,
        "matrix.list.clean.artifacts3.p95_ms": 181720.124,
        "matrix.list.clean.artifacts3.samples": 1,
        "matrix.list.clean.deep.p95_ms": 4.303,
        "matrix.list.clean.deleted.p50_ms": 0,
        "matrix.list.clean.deleted.p95_ms": 5.538,
        "matrix.list.clean.deleted.samples": 200,
        "matrix.list.clean.minconf+type.p50_ms": 5.19,
        "matrix.list.clean.minconf+type.p95_ms": 13.052,
        "matrix.list.clean.minconf+type.samples": 200,
        "matrix.list.clean.none.p50_ms": 0.696,
        "matrix.list.clean.none.p95_ms": 3.027,
        "matrix.list.clean.none.samples": 200,
        "matrix.list.clean.parser.p50_ms": 330759.78,
        "matrix.list.clean.parser.p95_ms": 330759.78,
        "matrix.list.clean.parser.samples": 1,
        "matrix.list.clean.path+artifact.p50_ms": 88326.27,
        "matrix.list.clean.path+artifact.p95_ms": 88383.622,
        "matrix.list.clean.path+artifact.samples": 2,
        "matrix.list.clean.time+untimed.p50_ms": 436.928,
        "matrix.list.clean.time+untimed.p95_ms": 862.49,
        "matrix.list.clean.time+untimed.samples": 200,
        "matrix.list.clean.time.p50_ms": 344.631,
        "matrix.list.clean.time.p95_ms": 706.135,
        "matrix.list.clean.time.samples": 200,
        "matrix.list.clean.type+time.p50_ms": 40.399,
        "matrix.list.clean.type+time.p95_ms": 240.305,
        "matrix.list.clean.type+time.samples": 200,
        "matrix.list.clean.type1.p50_ms": 1.625,
        "matrix.list.clean.type1.p95_ms": 5.497,
        "matrix.list.clean.type1.samples": 200,
        "matrix.list.clean.type2.p50_ms": 37706.797,
        "matrix.list.clean.type2.p95_ms": 225434.146,
        "matrix.list.clean.type2.samples": 4,
        "matrix.list.clean.type3.p50_ms": 266198.959,
        "matrix.list.clean.type3.p95_ms": 266198.959,
        "matrix.list.clean.type3.samples": 1,
        "matrix.search.prefix.mid.type.p95_ms": 73.469,
        "matrix.search.prefix.mid.type.samples": 200,
        "matrix.search.prefix.mid.type.timeouts": 0,
        "matrix.search.substring.common.p95_ms": 20004.879,
        "matrix.search.substring.common.samples": 6,
        "matrix.search.substring.common.timeouts": 9,
        "matrix.search.substring.mid.p95_ms": 233.52,
        "matrix.search.substring.mid.samples": 200,
        "matrix.search.substring.mid.timeouts": 0,
        "matrix.search.substring.mid.type.p95_ms": 41.429,
        "matrix.search.substring.mid.type.samples": 200,
        "matrix.search.substring.mid.type.timeouts": 0,
        "matrix.search.word.common.time.p95_ms": 20006.149,
        "matrix.search.word.common.time.samples": 6,
        "matrix.search.word.common.time.timeouts": 9,
        "matrix.search.word.common.type.p95_ms": 643.21,
        "matrix.search.word.common.type.samples": 192,
        "matrix.search.word.common.type.timeouts": 0,
        "matrix.search.word.mid.time.p95_ms": 222.398,
        "matrix.search.word.mid.time.samples": 200,
        "matrix.search.word.mid.time.timeouts": 0,
        "matrix.search.word.mid.type.p95_ms": 24.897,
        "matrix.search.word.mid.type.samples": 200,
        "matrix.search.word.mid.type.timeouts": 0,
        "matrix.search.word.rare.type.p95_ms": 49.837,
        "matrix.search.word.rare.type.samples": 200,
        "matrix.search.word.rare.type.timeouts": 0,
        "open_artifact_ms": 31.736368,
        "ostemp_added_ingest": 0,
        "peak_rss_ingest_bytes": 261042176,
        "search_substring_p95_ms": 20004.879,
        "search_word_p95_ms": 20006.149,
        "start_s": 0.0940397,
        "stats.group_s": 5.5515808,
        "stats.index_s": 4.3313005,
        "stats.overview_s": 94.1453441,
        "stats_type_s": 104.0282254
      }
    }
  ],
  "decisions": {
    "stats_plan": "apply",
    "search_plan": "apply",
    "open_artifact_cache": "apply",
    "ingest_hot_path": "apply",
    "parallel_verify": "pending"
  }
}
```

## Verify phases
Baseline, 10,000,000 records: `case verify` did not finish. The scale test was given 240 minutes and `go test` timed out after 3 h 57 min (14,227 s, the `go test` package time of 14226.689 s on the FAIL line of the verify run) with the stack inside `verifyFTS` -> `compareVocab` (`verify_fts.go:570`), the comparison of the full-text vocabulary. There is no phase table, no `verify_rate` and no `verify_phases_s` for this run: none is computed or estimated from a run that did not end (ruling D68b).

Spill: the SQLite temporary files under `<case>/tmp/verify-*` peaked at about 25 GB against a 38 GB `artifacts.db` (read by the 500 ms directory sampler, so a lower bound). Free RAM briefly fell to about 910 MB while it ran.

Not measured at 10M (by decision of 2026-10-16, they wait for the verify fix): copy, supersede, superqueries and tamper. Tamper detection is proven at 301,000 records (Task 4 smoke).

## Decisions
One line per decision: the rule, the rows, the run, the outcome. All read the `baseline` run (10,000,000 records).

- `stats_plan` = apply: `stats_type_s` 104.03 s against <= 3 s (overview 94.15 s, group 5.55 s, index status 4.33 s).
- `search_plan` = apply: `search_word_p95_ms` 20006 ms against <= 300 ms and `search_substring_p95_ms` 20005 ms against <= 1000 ms.
- `open_artifact_cache` = apply: `open_artifact_ms` 31.74 ms (median of 3, 10,000 manifest lines) against <= 1 ms.
- `ingest_hot_path` = apply: `ingest_rate` 1451 records/s against >= 40,000 (the after-pragmas run is not recorded yet; one miss is enough). `BenchmarkIngest` batch 1000/5000/20000: 1969/2452/2814 records/s. `db_ratio` is 3.90 against <= 1.6 (no decision is keyed to it).
- `parallel_verify` = pending: keyed to the `final` run, and the baseline verify did not finish.

Notes (ruling D69):
- `search_word_p95_ms` (20006 ms) and `search_substring_p95_ms` (20005 ms) sit AT the 20 s per-query timeout. The cells behind them (`matrix.search.word.common.time`, `matrix.search.substring.common`) have 6 samples and 9 timeouts each. They are LOWER BOUNDS, not measured p95s: the true p95 is at least this, and unknown.
- `list_p95_ms` (266199 ms) is the largest value in the gated set; it comes from `type3`, a cell with 1 sample, so it is one observation, not a p95. The worst cell overall is `parser`, 330760 ms, also 1 sample and outside the gated set. Treat both as lower-confidence figures of the same kind as the search lower bounds above: an order of magnitude, not a percentile. Several list cells have too few samples for a p95: `type2` 4, `type3` 1, `artifact_big` 2, `artifacts3` 1, `parser` 1, `path+artifact` 2 (the others have 200). With 1 to 4 samples a "p95" is just the slowest sample.
- The plans of those low-sample cells (`type2`, `type3`, `artifact_big`, `artifacts3`, `parser`, `path+artifact`) all end in `USE TEMP B-TREE FOR ORDER BY`: the filter index does not deliver the time order, so the whole matching set is sorted. `type2` and `type3` are in the gated set. Plans of the worst cells, each bullet naming the cells it covers:
  - `clean.type2` and `clean.type3` (type2 4 samples, p95 225 s; type3 1 sample, 266 s; the same plan): SEARCH r USING INDEX records_type_ts (type=?) / SEARCH p USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN / SEARCH b USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN / USE TEMP B-TREE FOR ORDER BY
  - `clean.artifacts3` (182 s, 1 sample; `artifact_big` and `path+artifact` use the same plan): SEARCH r USING INDEX records_artifact (artifact_id=?) / SEARCH p USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN / SEARCH b USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN / USE TEMP B-TREE FOR ORDER BY
  - `clean.parser` (331 s, 1 sample): SEARCH p USING INDEX sqlite_autoindex_parsers_1 (name=?) / SEARCH r USING INDEX records_parser (parser_id=?) / SEARCH b USING INTEGER PRIMARY KEY (rowid=?) LEFT-JOIN / USE TEMP B-TREE FOR ORDER BY
- Cells that hit a time-ordered index are fast (`none` p95 3.0 ms, `type1` 5.5 ms, `deleted` 5.5 ms, `deep` 4.3 ms, `minconf+type` 13.1 ms), but `time` (706 ms), `time+untimed` (862 ms) and `type+time` (240 ms) miss 50 ms.
- Ingest `peak_rss_ingest_bytes` 261 MB; `ostemp_added_ingest` 0.

## Profiles
(Task 9: the top of the ingest CPU profile when the ingest decision is `apply`.)

### Ingest CPU profile (5D Task 9, step 1)
Taken at 400,000 records, not the plan's 1,000,000 (ruling D30 caps implementer runs at 400k), with `-cpuprofile` through `heavy.sh`, label `profile`, 245 s, 251 s of samples, code of 273e1d1 (the Task 8 cache of 256 MiB included). Top 15 by cumulative time (rows elided: the table is not the full top 15 and its sums are of the full profile, so they do not add up across the rows shown):

```
      flat  flat%   sum%        cum   cum%
     0.02s 0.008% 0.008%    234.31s 93.19%  records_test.(*scaleState).ingest
     0.02s 0.008% 0.016%    231.19s 91.95%  records.(*Writer).Add
     0.02s 0.008% 0.024%    229.18s 91.15%  records.(*Writer).flush
         0     0% 0.024%    212.35s 84.46%  evidence.(*Case).StoreTxTuned
         0     0% 0.024%    212.33s 84.45%  records.(*Writer).insertBatch
     0.04s 0.016%  0.04%    208.16s 82.79%  sqlite/lib.Xsqlite3_step
     1.87s  0.74%  0.84%    208.01s 82.73%  sqlite/lib._sqlite3VdbeExec
         0     0%  0.84%    157.06s 62.47%  records.(*Writer).insertBatch.func1
         0     0%  0.87%    156.45s 62.22%  sqlite.(*stmt).exec
         0     0%  0.99%    142.67s 56.74%  evidence.InsertFTSDocs
         0     0%  1.04%    140.96s 56.06%  sqlite/lib._fts5StorageInsert
     0.30s 0.12%  1.16%    107.28s 42.67%  sqlite/lib._fts5FlushOneHash
     4.71s 1.87%  3.03%     91.73s 36.48%  sqlite/lib._fts5IndexMergeLevel
                                      ... (rows elided) ...
         0     0% 35.46%     53.44s 21.25%  database/sql.(*Tx).Commit
    42.9s  ..    ..         42.90s 17.06%  windows.FlushFileBuffers
```

Shares that decide the sub-steps of Task 9:
- Preparation (`evidence.RowDigest`, `evidence.NewFTSDoc`/`NormalizeText`, `canonicalPayload`, `Writer.prepareRecord`): about 0.1 s of 251 s, 0.04 %. Far below 25 %, so parallel preparation (sub-step A) does not apply.
- The `records` and `record_times` insert statements: 14.0 s, 5.6 %. The 56.7 % under `stmt.exec` is one statement per batch, `InsertFTSDocs`, and it is FTS5 index maintenance (`fts5FlushOneHash`, segment merges 36.5 %), not driver overhead. A multi-row INSERT (sub-step B) cannot remove it.
- Commit and fsync: 21.3 % (`FlushFileBuffers` 17.1 %). The rest of the time is index maintenance inside SQLite.

The miss is in the engine (full-text and B-tree index maintenance); this plan does not change it. The FTS merge share (36.5 % here) only grows with index size: each merge level rewrites more segments as the index grows, so at 400,000 records this is a floor for larger runs, not a ceiling.

### Relaxed sync during ingest (5D Task 9b, rulings D89, D90 f)
A profile, not a spec-target run. Same machine as the baseline (Intel i7-1265U, 15.4 GB RAM, NVMe, Windows 11 Pro, go1.26.8), under normal desktop load, code of a557479, `go test -tags scale -run TestScale` with `MINUTIAE_SCALE_PHASES=ingest`, 400,000 records (ruling D30), each run alone through `heavy.sh`; NORMAL is `MINUTIAE_SCALE_RELAXED_SYNC=1` (`WriterOptions.RelaxedSync`).

| Ingest sync | Records | Ingest time | Rate | Peak RSS |
|---|---|---|---|---|
| FULL (default) | 400,000 | 255.2 s | 1,567.5 records/s | 479 MB |
| NORMAL (`--fast-sync`, write-ahead log) | 400,000 | 184.5 s | 2,167.5 records/s | 486 MB |

NORMAL is 1.38 times the FULL rate (-28 % ingest time), in line with the 21 % commit and fsync share of the Task 9 profile plus the cheaper write-ahead commits. The price is the power-loss window in `docs/limitations.md`. One run of each, so the difference carries run-to-run noise of a shared laptop; the End step (checkpoint and journal restore) is inside the NORMAL time.
