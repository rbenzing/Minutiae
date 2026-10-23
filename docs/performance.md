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
| `verify_rate` | >= 3,700 records/s (amendment A10) | records checked by `case verify` (every check P1-P14, R-series, P11 included) divided by its wall time |
| `db_ratio` | <= 1.6 | `artifacts.db` bytes over the row data plus 64 bytes per row (exact definition below) |
| `open_artifact_ms` | <= 1 ms | plan threshold (not a spec target): `OpenArtifact` with 10,000 manifest lines |

Amendment A10 (2026-10-22, coordinator rule C1): `verify_rate` was >= 100,000 records/s. The phases that cannot be made cheaper without weakening a check (`records.integrity_check`, `records.stream`, the artifact re-hash and the audit chain) take 359.48 s at 2,000,000 records (phases-2m), so 1,797.4 s at 10,000,000. The target is 10,000,000 / (1.5 x 1,797.4 s) = 3,709, rounded down to two significant figures: >= 3,700 records/s.

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
    {"id": "verify_rate", "op": ">=", "value": 3700, "unit": "records/s"},
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
    },
    {
      "commit": "acc067a",
      "label": "after-pragmas",
      "machine": "Intel i7-1265U 10 cores/12 threads, 15.4 GB RAM, NVMe SK hynix PC801 512GB, Windows 11 Pro, go1.26.8",
      "records": 2000000,
      "values": {
        "db_ratio": 3.900564515150823,
        "ingest_rate": 2148.0704092413634
      }
    },
    {
      "commit": "7e8c2ff",
      "label": "final",
      "machine": "Intel i7-1265U 10 cores/12 threads, 15.4 GB RAM, NVMe SK hynix PC801 512GB, Windows 11 Pro, go1.26.8",
      "records": 10000000,
      "unfinished": ["verify_rate"],
      "values": {
        "db_ratio": 3.8978788141812015,
        "get_p95_ms": 274.366,
        "ingest_rate": 1630.549279610738,
        "ingest_s": 6132.9026513,
        "list_p95_ms": 19.623,
        "matrix.list.clean.artifact_big.p50_ms": 81038.294,
        "matrix.list.clean.artifact_big.p95_ms": 81085.605,
        "matrix.list.clean.artifact_big.samples": 2,
        "matrix.list.clean.artifact_tiny.p50_ms": 0.585,
        "matrix.list.clean.artifact_tiny.p95_ms": 3.731,
        "matrix.list.clean.artifact_tiny.samples": 200,
        "matrix.list.clean.artifacts3.p50_ms": 170433.633,
        "matrix.list.clean.artifacts3.p95_ms": 170433.633,
        "matrix.list.clean.artifacts3.samples": 1,
        "matrix.list.clean.deep.p95_ms": 4.246,
        "matrix.list.clean.deleted.p50_ms": 1.011,
        "matrix.list.clean.deleted.p95_ms": 2.766,
        "matrix.list.clean.deleted.samples": 200,
        "matrix.list.clean.minconf+type.p50_ms": 5.73,
        "matrix.list.clean.minconf+type.p95_ms": 8.396,
        "matrix.list.clean.minconf+type.samples": 200,
        "matrix.list.clean.none.p50_ms": 1.025,
        "matrix.list.clean.none.p95_ms": 2.868,
        "matrix.list.clean.none.samples": 200,
        "matrix.list.clean.parser.p50_ms": 307639.941,
        "matrix.list.clean.parser.p95_ms": 307639.941,
        "matrix.list.clean.parser.samples": 1,
        "matrix.list.clean.path+artifact.p50_ms": 80652.874,
        "matrix.list.clean.path+artifact.p95_ms": 83466.33,
        "matrix.list.clean.path+artifact.samples": 2,
        "matrix.list.clean.time+untimed.p50_ms": 409.788,
        "matrix.list.clean.time+untimed.p95_ms": 825.013,
        "matrix.list.clean.time+untimed.samples": 200,
        "matrix.list.clean.time.p50_ms": 11.831,
        "matrix.list.clean.time.p95_ms": 19.623,
        "matrix.list.clean.time.samples": 200,
        "matrix.list.clean.type+time.p50_ms": 9.358,
        "matrix.list.clean.type+time.p95_ms": 14.235,
        "matrix.list.clean.type+time.samples": 200,
        "matrix.list.clean.type1.p50_ms": 1.779,
        "matrix.list.clean.type1.p95_ms": 3.917,
        "matrix.list.clean.type1.samples": 200,
        "matrix.list.clean.type2.p50_ms": 3.176,
        "matrix.list.clean.type2.p95_ms": 9.273,
        "matrix.list.clean.type2.samples": 200,
        "matrix.list.clean.type3.p50_ms": 4.415,
        "matrix.list.clean.type3.p95_ms": 7.022,
        "matrix.list.clean.type3.samples": 200,
        "matrix.search.prefix.mid.type.p95_ms": 20997.526,
        "matrix.search.prefix.mid.type.samples": 6,
        "matrix.search.prefix.mid.type.timeouts": 9,
        "matrix.search.selective.common.artifact_tiny.p95_ms": 43778.837,
        "matrix.search.selective.common.artifact_tiny.samples": 4,
        "matrix.search.selective.common.artifact_tiny.timeouts": 7,
        "matrix.search.selective.common.deleted.p95_ms": 103.83,
        "matrix.search.selective.common.deleted.samples": 200,
        "matrix.search.selective.common.deleted.timeouts": 0,
        "matrix.search.selective.common.typepair.p95_ms": 63.743,
        "matrix.search.selective.common.typepair.samples": 200,
        "matrix.search.selective.common.typepair.timeouts": 0,
        "matrix.search.substring.common.p95_ms": 72.965,
        "matrix.search.substring.common.samples": 200,
        "matrix.search.substring.common.timeouts": 0,
        "matrix.search.substring.mid.p95_ms": 4225.281,
        "matrix.search.substring.mid.samples": 33,
        "matrix.search.substring.mid.timeouts": 0,
        "matrix.search.substring.mid.type.p95_ms": 8957.068,
        "matrix.search.substring.mid.type.samples": 25,
        "matrix.search.substring.mid.type.timeouts": 0,
        "matrix.search.word.common.time.p95_ms": 58.233,
        "matrix.search.word.common.time.samples": 200,
        "matrix.search.word.common.time.timeouts": 0,
        "matrix.search.word.common.type.p95_ms": 36.57,
        "matrix.search.word.common.type.samples": 200,
        "matrix.search.word.common.type.timeouts": 0,
        "matrix.search.word.mid.time.p95_ms": 3148.635,
        "matrix.search.word.mid.time.samples": 47,
        "matrix.search.word.mid.time.timeouts": 0,
        "matrix.search.word.mid.type.p95_ms": 2551.753,
        "matrix.search.word.mid.type.samples": 76,
        "matrix.search.word.mid.type.timeouts": 0,
        "matrix.search.word.rare.type.p95_ms": 21044.128,
        "matrix.search.word.rare.type.samples": 6,
        "matrix.search.word.rare.type.timeouts": 9,
        "ostemp_added_ingest": 0,
        "peak_rss_ingest_bytes": 747417600,
        "search_substring_p95_ms": 8957.068,
        "search_word_p95_ms": 21044.128,
        "start_s": 0.0636404,
        "stats.group_s": 7.4582118,
        "stats.index_s": 0.0031243,
        "stats.overview_s": 25.7279989,
        "stats_type_s": 33.189335
      }
    },
    {
      "commit": "65be2b0",
      "label": "phases-2m",
      "machine": "Intel i7-1265U 10 cores/12 threads, 15.4 GB RAM, NVMe SK hynix PC801 512GB, Windows 11 Pro, go1.26.8",
      "records": 2000000,
      "values": {
        "db_ratio": 3.9006946544454326,
        "ingest_rate": 2233.582647950401,
        "ingest_s": 895.422429,
        "ostemp_added_ingest": 0,
        "ostemp_added_verify": 0,
        "peak_rss_ingest_bytes": 533004288,
        "peak_rss_verify_bytes": 868171776,
        "start_s": 0.0589376,
        "verify_rate": 417.1744085268196,
        "verify_s": 4794.1579328,
        "verify_scratch_bytes": 991232
      }
    }
  ],
  "decisions": {
    "stats_plan": "apply",
    "search_plan": "apply",
    "open_artifact_cache": "apply",
    "ingest_hot_path": "apply",
    "parallel_verify": "pending: P11 speed-up first (user decision 2026-10-22)"
  }
}
```

## Verify phases
Baseline, 10,000,000 records: `case verify` did not finish. The scale test was given 240 minutes and `go test` timed out after 3 h 57 min (14,227 s, the `go test` package time of 14226.689 s on the FAIL line of the verify run) with the stack inside `verifyFTS` -> `compareVocab` (`verify_fts.go:570`), the comparison of the full-text vocabulary. There is no phase table, no `verify_rate` and no `verify_phases_s` for this run: none is computed or estimated from a run that did not end (ruling D68b).

Spill: the SQLite temporary files under `<case>/tmp/verify-*` peaked at about 25 GB against a 38 GB `artifacts.db` (read by the 500 ms directory sampler, so a lower bound). Free RAM briefly fell to about 910 MB while it ran.

Not measured at 10M (by decision of 2026-10-16, they wait for the verify fix): copy, supersede, superqueries and tamper. Tamper detection is proven at 301,000 records (Task 4 smoke).

### `final` run (code of 7e8c2ff, 10,000,000 records, label `final`)
`case verify` did not finish, twice. Both attempts ran on the clean `final` case, alone on an idle machine, through `heavy.sh`, and ended in the `go test` timeout panic (the harness sets the limit 3 minutes below the wrapper's):
1. Limit 120 minutes: `panic: test timed out after 1h57m0s` (package time 7028.652 s). The stack was in `verifyFTS` -> `streamRecords` (`verify_fts.go:407` and `:447`, the full-text rebuild of P11, called from `verify_fts.go:176`). The SQLite scratch space under `<case>/tmp/verify-*` had grown to 16 GB when last read by hand at 15:50 (free disk fell from 43 to 20 GB over the attempt, read by `df`), and was gone after the process ended.
2. Limit 240 minutes: `panic: test timed out after 3h57m0s` (14,227 s). The stack was in `verifyFTS` -> `compareVocab` (`verify_fts.go:466`, `:654`, `:694`, `:584`), the comparison of the rebuilt and the stored vocabulary, the same place as the baseline.

There is no phase table, no `verify_phases_s` and no `verify_rate` for the `final` run, and none is estimated (ruling D68b); the run record lists `verify_rate` under `unfinished`. All that the second attempt proves is a bound: 10,000,000 records were not verified in 14,227 s, so `verify_rate` is below about 703 records/s, against a target of >= 100,000 (a factor of at least 140). The first attempt's 1h57m is not a rate either. Time spent in each earlier phase of `case verify` was not recorded by either attempt. Peak memory and scratch-space size of verify were not recorded (the process was killed).

Not run in `final` (stopped by the coordinator after the second timeout, user decision 2026-10-22): `copy`, `supersede`, `superqueries` and `tamper` at 10M, and therefore no `matrix.list.super.*`, `stats.super_s` or tamper detection at 10M (tamper detection stays proven at 301,000 records by the Task 4 smoke). `parallel_verify` stays pending: P11 is to be made faster first, in a new task with a spec amendment, and Task 15 does not start.


### Verify phases (2M, `phases-2m`, code of 65be2b0)
`case verify` finished on a fresh 2,000,000-record case ingested for this run (895.4 s, 2,233.6 records/s; `heavy.sh`, `go test -v`, idle machine, ingest and verify as separate runs). `verify_s` is 4,794.2 s and `verify_rate` 417.2 records/s, against a target of >= 100,000. Peak resident memory of verify was 868 MB, its SQLite scratch space 0.99 MB, and the OS temp directory was unchanged. Seconds are the `verify_phases_s` of the run; the share is of `verify_s`. The top-level rows and `other` sum to 100%; the indented rows are nested inside `fts.compare`.

| Phase | Seconds | Share |
|---|---:|---:|
| `audit` | 0.05 | 0.00% |
| `artifacts` (re-hash of the artifacts) | 0.15 | 0.00% |
| `records.schema` | 0.00 | 0.00% |
| `records.integrity_check` (P10) | 328.33 | 6.85% |
| `records.stream` | 30.96 | 0.65% |
| `records.batches` | 0.04 | 0.00% |
| `records.lifecycle` | 0.00 | 0.00% |
| `fts.rebuild` | 641.48 | 13.38% |
| `fts.compare` | 3,792.90 | 79.12% |
| &nbsp;&nbsp;`records_fts/vocab` | 326.31 | 6.81% |
| &nbsp;&nbsp;`records_fts/docsize` | 3.25 | 0.07% |
| &nbsp;&nbsp;`records_fts/config` | 0.00 | 0.00% |
| &nbsp;&nbsp;`records_fts/totals` | 0.00 | 0.00% |
| &nbsp;&nbsp;`records_fts/reachability` | 81.61 | 1.70% |
| &nbsp;&nbsp;`records_fts/unreferenced` | 0.43 | 0.01% |
| &nbsp;&nbsp;`records_fts_sub/vocab` | 2,496.12 | 52.07% |
| &nbsp;&nbsp;`records_fts_sub/docsize` | 3.46 | 0.07% |
| &nbsp;&nbsp;`records_fts_sub/config` | 0.00 | 0.00% |
| &nbsp;&nbsp;`records_fts_sub/totals` | 0.00 | 0.00% |
| &nbsp;&nbsp;`records_fts_sub/reachability` | 836.81 | 17.45% |
| &nbsp;&nbsp;`records_fts_sub/unreferenced` | 44.89 | 0.94% |
| `other` | 0.26 | 0.01% |
| Total | 4,794.16 | 100% |

P11 (the full-text check, `fts.rebuild` + `fts.compare`) is 4,434.38 s, 92.50% of `verify_s`. The two vocabulary comparisons are 2,822.44 s (58.87%), and `records_fts_sub` alone is 3,381.29 s (70.53%).

#### Projection (linear, x5; not a measurement)
This is a linear projection of the 2M seconds above to 10,000,000 records (x5). No 10M verify has finished, and a real run may differ (the `final` run did not end in 14,227 s). It is an input to the target decision, not a result.

- `records.integrity_check` (P10): 328.33 s at 2M, 1,641.7 s projected at 10M, against the 100 s that a rate of 100,000 records/s allows for the whole of 10M (16.4 times the budget by itself).
- P11: 4,434.38 s at 2M, 22,171.9 s (6.2 h) projected at 10M.
- The floor inputs, the phases that cannot be made cheaper without weakening a check (2M, then x5): P10 328.33 s, 1,641.7 s; `records.stream` 30.96 s, 154.8 s; artifact re-hash 0.15 s, 0.7 s; audit chain 0.05 s, 0.2 s. Their sum is 359.48 s at 2M and 1,797.4 s projected.
- All the phases other than P11 together (the time no task of this plan changes) are 359.78 s at 2M and 1,798.9 s projected; the rest of the 10M total is P11.

### P11 spike (5D Task 14f)
Throwaway spike of the P11 redesign (rulings C1, C5, C6). The code is not committed (it lived in `internal/evidence/p11spike*_test.go` behind the `p11spike` build tag and was deleted); it ran on the tree of 815d057. The machine is the one in "Machine" above (Intel i7-1265U 10 cores/12 threads, 15.4 GB RAM, NVMe SK hynix PC801 512GB, Windows 11 Pro, go1.26.8), idle, through `heavy.sh`, on a fresh 2,000,000-record case ingested for this task (956.4 s). The spike is single-threaded.

What was measured, as wall seconds each:
- `t_expected`: `streamRecords` over every record, `NewFTSDoc`, both tokenizer replicas and the digest into per-audited-batch buckets (400 batches).
- `t_index`: one `ReadTx` per table over `_data` in id order, a strict decode of every leaf page, the same digest into the same buckets, and a structural decode of every doclist-index page.
- `t_compare`: the bucket sums of the two sides.
- The digest is a length-prefixed AES-128 CBC-MAC of each posting (decision V6) with a per-run random key, summed mod 2^128 per bucket beside a posting count. It alone was enough: AES-GMAC (ruling C5) was not needed for the verdict.
- The tokenizer replicas are generated by probing the pinned SQLite (modernc.org/sqlite v1.60.1, SQLite 3.53.4) over every code point.

| Quantity | Run 1 | Run 2 |
|---|---:|---:|
| `P11_old` (`fts.rebuild` + `fts.compare` of `phases-2m`) | 4,434.38 s | 4,434.38 s |
| `t_expected` | 157.65 s | 152.42 s |
| of which stream and row scan | 30.76 s | 28.74 s |
| of which NFD, tokenize, digest | 126.89 s | 123.69 s |
| `t_index` | 125.87 s | 126.59 s |
| `t_compare` | 0.000 s | 0.000 s |
| `t_expected + t_index + t_compare` | 283.52 s | 279.01 s |
| `P11_old` over that sum | 15.6 | 15.9 |

- Postings: `records_fts` 138,927,925 and `records_fts_sub` 1,014,631,579 on each side (equal on the two sides, 0 delete markers). Leaf pages 1,291,265, doclist-index pages 47,812.
- Allocation peak (`HeapSys`, sampled every 250 ms): 191 MiB on the expected side, 190 MiB on the index side.
- Buckets: 800 compared (2 tables x 400 audited batches), 800 equal, 0 unequal, on the clean case.
- Tokenizer differential (the spike's own test, not kept; its committed successor is `TestFTS5ReplicaMatchesSQLiteOnCorpora`; SQLite against the replicas, tokens with column and offset, and the `_docsize` blobs): every code point from U+0001 to U+10FFFF without surrogates (1,112,063) alone, at the start, in the middle and at the end of a token, in both tables; tokens of 40,000 bytes and of 3-byte characters crossing byte 32,768; a list of edge strings (uncomposed marks, Thai, CJK, U+FFFE, emoji); 20,000 documents of the test text; 60,000 random mixes of assorted scripts, marks, digits and separators. 0 differences.
- Digest throughput (50,000,000 postings, one goroutine): AES-CBC-MAC 12.6 million postings/s with the term hashed for every posting (the expected side) and 20.0 million/s with the chain after the term cached (the index side); AES-GMAC 9.6 and 11.4 million/s. Over the whole side (including stream, tokenize and decode): 7.3 to 7.6 million postings/s expected, 9.1 to 9.2 million postings/s index.
- What the spike does not include: the recomputation of `_idx`, doclist indexes and footers (14j), the strict hostile-input checks of 14h beyond the structural ones it needed, and any multi-core use.

Verdict (mechanical rule of the plan, go bar P11 at 2M of at most 443 s): **GO**. The slower run is 283.52 s, ratio 15.6 against the required 10; the differential has 0 differences; all 800 buckets are equal on the clean case.

## Decisions
One line per decision: the rule, the rows, the run, the outcome. All read the `baseline` run (10,000,000 records).

- `stats_plan` = apply: `stats_type_s` 104.03 s against <= 3 s (overview 94.15 s, group 5.55 s, index status 4.33 s).
- `search_plan` = apply: `search_word_p95_ms` 20006 ms against <= 300 ms and `search_substring_p95_ms` 20005 ms against <= 1000 ms.
- `open_artifact_cache` = apply: `open_artifact_ms` 31.74 ms (median of 3, 10,000 manifest lines) against <= 1 ms.
- `ingest_hot_path` = apply: `ingest_rate` 1451 records/s against >= 40,000 (one miss is enough); the 2,000,000-record `after-pragmas` run (code of acc067a, 256 MiB batch page cache) ingested at 2148 records/s, also against >= 40,000, with `db_ratio` 3.90 at 2M as at 10M (no decision is keyed to `db_ratio`). The decision stays `apply` whatever the later runs say (ruling D17). `BenchmarkIngest` batch 1000/5000/20000: 1969/2452/2814 records/s. `db_ratio` is 3.90 against <= 1.6 (no decision is keyed to it).
- `parallel_verify` = pending: P11 speed-up first (user decision 2026-10-22). The `final` verify did not finish in 14,227 s (`verify_rate` below about 703 records/s against >= 100,000); the plan's rule would read `apply`, but it needs a measured `verify_rate`, none exists, and the decision is not made from a bound. Task 15 does not start.

The `final` run against the seven spec rows (baseline against final, 10,000,000 records; ruling D2: judged strictly as measured):
- `ingest_rate`: miss, 1630.5 records/s against >= 40,000 (baseline 1451.0; ingest 6132.9 s; peak RSS 747 MB against 261 MB; `ostemp_added_ingest` 0). `db_ratio` miss, 3.898 against <= 1.6 (baseline 3.901).
- `list_p95_ms`: pass, 19.6 ms against <= 50 (baseline 266,199 ms, a 1-sample cell). Outside the gated set, the plans that sort the match set still take minutes: `artifact_big` 81 s, `artifacts3` 170 s, `path+artifact` 83 s, `parser` 308 s (1 to 2 samples each) and `time+untimed` 825 ms.
- `search_word_p95_ms`: miss, 21,044 ms against <= 300 (baseline 20,006). `search_substring_p95_ms`: miss, 8957 ms against <= 1000 (baseline 20,005). The common-word cells now pass (`word.common.type` 36.6 ms, `word.common.time` 58.2 ms, `substring.common` 73.0 ms), while `word.mid.type` (2552 ms), `word.mid.time` (3149 ms), `substring.mid` (4225 ms), `substring.mid.type` (8957 ms) and the timeouts of `word.rare.type` (9) and `prefix.mid.type` (9) reproduce the `search-retune` figures on freshly ingested data, so the cold file cache named in the retune notes does not explain them. The D98 cells: `selective.common.deleted` 103.8 ms and `typepair` 63.7 ms; `artifact_tiny` 43,779 ms with 7 timeouts.
- `stats_type_s`: miss, 33.19 s against <= 3 (baseline 104.03 s; overview 25.73 s, group 7.46 s, index status 0.003 s).
- `verify_rate`: not measured (see above).
- `open_artifact_ms`: 0.209 ms, a benchmark (Task 12 notes), not part of the `final` run.
- Invariants of the harness that did run held: the ingest recorded the full count, `queries`, `search`, `stats` and `untouched` passed (queries left the case's tree hash and audit count unchanged), `ostemp_added_ingest` was 0.

What the plan did about each miss and what is left (Task 16 carries them to `docs/limitations.md` with these numbers): ingest (FTS5 maintenance and fsync dominate, the profile above; relaxed sync is available and off by default); search (index-first fixed the common words only; the selectivity of the other filters is not considered, D98); stats (index-only `Overview` cut 94.1 s to 25.7 s, and 25.7 s and `group_s` 7.5 s remain); `db_ratio` (no change made); verify (P11, above). Others of the list matrix that sort a large match set are the residual of Task 7.

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

NORMAL is 1.38 times the FULL rate (-28 % ingest time), of the same order as the 21 % commit and fsync share of the Task 9 profile. The price is the power-loss window in `docs/limitations.md`. One run of each, so the difference carries run-to-run noise of a shared laptop.

### Stats retune (5D Task 6b, ruling D80)
Phase `stats` alone on the baseline's clean 10,000,000-record case (label `stats-retune`, not a run label of the record), code of acc067a (the index-only `Overview` and the totals-record `IndexStatus` of Task 6), an idle machine, through `heavy.sh`; the case had not been opened for five days (cold file cache). Each component is the worst of three calls.

| Component | Baseline | Retune |
|---|---|---|
| `stats.overview_s` (`Overview`, six statements) | 94.145 s | 53.622 s |
| `stats.group_s` (`Stats --by type`) | 5.552 s | 8.004 s |
| `stats.index_s` (`IndexStatus`, totals record) | 4.331 s | 0.0026 s |
| `stats_type_s` (sum) | 104.028 s | 61.628 s |
| `get_p95_ms` (200 `Get` samples; not part of this task) | 172.3 ms | 265.6 ms |

`stats_type_s` is 61.6 s against <= 3 s: the target is still missed, by a factor of about 20. Only the index status reached its floor (the totals record made it a point read). `Overview` still takes 53.6 s and `Stats --by type` 8.0 s, so the `group_s` floor of D22 (0.5 s) is exceeded 16 times over and goes to the Task 16 limitations line with this number. The baseline ran under desktop load straight after the ingest, this run on an idle machine against a cold cache, so a change of a few seconds either way (`group_s` rose, `get_p95_ms` rose) is not by itself a regression of the code. Opening the case for this phase appended one `case.open` entry to its `audit.jsonl` (as every earlier phase did); nothing else of the case changed (`artifacts.db`, manifest and `case.json` unchanged).

### After-pragmas ingest run (5D Task 8b, ruling D86)
`after-pragmas`: ingest only, 2,000,000 records, label `after-pragmas`, code of acc067a (the batch transactions carry the bounded 256 MiB page cache of Task 8), FULL sync (no `--fast-sync`), alone on an idle machine (no other builder or check running), through `heavy.sh`. Ingest took 931.1 s: 2148 records/s (`ingest_rate`), `db_ratio` 3.90, `peak_rss_ingest_bytes` 533,016,576 (533 MB), `ostemp_added_ingest` 0. It is one input of the ingest decision (still `apply`, ruling D17).

Peak memory against the baseline (D86): the baseline's 10,000,000-record ingest peaked at 261 MB; this 2,000,000-record ingest peaked at 533 MB, 272 MB more at one fifth of the records, consistent with, but not proven to be, the 256 MiB page cache the baseline did not have (the cache is a ceiling held once filled; the baseline used SQLite's default). The 400,000-record profiles above read 479 MB (FULL) and 486 MB (NORMAL) on the code of Tasks 8 and 9b. This is the peak of the whole test process (ruling D50), so it also includes the generator.

Rate, not comparable like for like: the baseline's 1451 records/s is a 10,000,000-record average over 6892 s and the rate falls as the index grows, so the two figures differ in size as well as in code; the figure at 400,000 records on the Task 9b code was 1567.5 records/s (FULL). Nothing here is a claim that the cache alone moved the rate.

### Selectivity-aware search plans (5D Task 10)
A word or prefix that matches a large share of the records made `r.id IN (SELECT rowid FROM <fts> WHERE <fts> MATCH ?)` materialize every matching rowid before SQLite could order by time (the baseline `matrix.search.*` p95 cells above, `search_plan` = apply). A page now runs one bounded probe (`SELECT count(*) FROM (SELECT 1 FROM <fts> WHERE <fts> MATCH ? LIMIT 4096)`); fewer than 4096 hits keep the FTS-first plan, otherwise the page is driven by `records_ts` (or `records_type_ts` with exactly one type) with an `EXISTS` on the full-text rowid and the first hits end the scan. Both shapes select the same records in the same order (`TestSearchShapesReturnIdenticalPages`). Query plan of the index-first page: `SCAN r USING INDEX records_ts` (`SEARCH r USING INDEX records_type_ts (type=?)` with one type) and `SCAN records_fts EXISTS VIRTUAL TABLE INDEX 0:=M2`, no temporary b-tree for the order. The FTS-first plan was `SEARCH r USING INTEGER PRIMARY KEY` over `LIST SUBQUERY` plus `USE TEMP B-TREE FOR ORDER BY`.

Known limit (D98, superseded by Task 14n: three bands by hits, a prefix or multi-term query never takes `EXISTS`, and a bounded filter probe drops the hint under 4096 filtered records; see `docs/limitations.md` and the invariants row). As first built, the choice looked only at the number of full-text hits, never at the selectivity of the other filters: a common word combined with a rare filter (deleted only, one artifact, a rare type pair) was driven by `records_ts` and scanned the time index across the table with one `EXISTS` per row, so it could be slower than the FTS-first plan. The result is identical either way (`TestSearchShapesAgreeOnEveryFilter`). The D83 re-measure added cells for these combinations (a common word with deleted only, with one artifact, with a rare type pair; results below); no code change is made before those numbers exist.

The 10M re-measure (D83), phase `search` alone on the baseline's clean 10,000,000-record case, label `search-retune` (not a run label of the record), code of acc067a, an idle machine, through `heavy.sh`; the case had not been opened for five days, so the operating system's file cache was cold, which the baseline's search phase (run straight after the ingest and the list matrix) was not. p95 in ms, with the sample count and the number of 20 s timeouts (the elapsed time of a timed-out query is what is recorded, and it can exceed the 20 s timeout when the deadline is noticed late, as in the 41.7 s cell, so a cell with timeouts is a lower bound; a cell with few samples ran out of its time budget):

| Cell | Baseline p95 (samples / timeouts) | Retune p95 (samples / timeouts) |
|---|---|---|
| `word.common.type` | 643.2 (192 / 0) | 75.3 (200 / 0) |
| `word.common.time` | 20006.1 (6 / 9) | 56.7 (200 / 0) |
| `word.mid.type` | 24.9 (200 / 0) | 4169.7 (64 / 0) |
| `word.mid.time` | 222.4 (200 / 0) | 3165.2 (46 / 0) |
| `word.rare.type` | 49.8 (200 / 0) | 20559.5 (6 / 9) |
| `prefix.mid.type` | 73.5 (200 / 0) | 20958.9 (6 / 9) |
| `substring.common` | 20004.9 (6 / 9) | 76.5 (200 / 0) |
| `substring.mid` | 233.5 (200 / 0) | 3717.5 (33 / 0) |
| `substring.mid.type` | 41.4 (200 / 0) | 4438.8 (30 / 0) |
| `selective.common.deleted` (D98) | not measured | 99.0 (200 / 0) |
| `selective.common.artifact_tiny` (D98) | not measured | 41683.3 (5 / 8) |
| `selective.common.typepair` (D98) | not measured | 61.7 (200 / 0) |

`search_word_p95_ms` is 20958.9 ms (was 20006.1) against <= 300 and `search_substring_p95_ms` 4438.8 ms (was 20004.9) against <= 1000: both still miss. The common-word cells, which the plan targets, went from 643 ms and 20 s to 57 to 77 ms. The mid and rare cells, which the FTS-first plan answered in 25 to 233 ms in the baseline, went the other way (3 to 21 s). The likely cause, not proven by this run, is the choice D98 names: a word with at least 4096 hits flips to index-first, which walks the time index and probes the full-text index once per row, and a filter that keeps a small share of the rows (a type, a time window) makes that walk long; no EXPLAIN or profile was taken at 10M to confirm it. Of the three cells added for D98, `selective.common.artifact_tiny` (a common word in the one small artifact) is the worst (5 samples, 8 timeouts); `deleted` (99 ms) and `typepair` (62 ms) are within the 300 ms word bar. Whether the cold cache or the plan accounts for the mid and rare cells cannot be separated from this one run; the `final` run (fresh data, same code) is the second observation and is compared with these numbers in the Task 14 notes. No code change is made in this task (D98); what to do about it is for the coordinator and Task 16 (limitations).

### OpenArtifact manifest cache (5D Task 12)
A small benchmark, not a 10M run. Same machine as the baseline, `BenchmarkOpenArtifact`, `-count=3`, through `heavy.sh`, code of the Task 12 commit. ns/op, three runs each; the median of the 10,000-line row is the `open_artifact_ms` figure.

| Manifest lines | Before (baseline, no cache) | After (stat-validated cache) |
|---|---|---|
| 100 | 501,305 / 512,234 / 521,564 | 129,608 / 169,529 / 128,050 |
| 1,000 | 5,131,222 / 4,152,013 / 3,641,574 | 129,171 / 132,658 / 128,377 |
| 10,000 | 31,736,368 / 31,954,174 / 30,144,010 | 208,934 / 296,430 / 193,829 |

`open_artifact_ms` went from 31.74 ms to 0.209 ms (median of 3), under the <= 1 ms threshold. The remaining cost per open is the stat of the manifest, the linear scan of the cached records for the id, and the artifact file checks (link walk, open, regular-file and size checks), which the cache does not touch. The time still grows a little with the line count because the scan is linear. The cache is valid only while the manifest is the same file with the same size and modification time; the residual is in `docs/limitations.md`.
