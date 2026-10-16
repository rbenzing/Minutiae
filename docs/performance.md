# Records performance (sub-project 5, plan 5D)

Numbers recorded by `BenchmarkIngest`, `BenchmarkOpenArtifact`, `BenchmarkReadRunLines` and the manual
scale test (`go test -tags scale -run TestScale ./internal/records/`, see `docs/testing.md`). The JSON
block below is the record; `TestPerformanceRecordIsConsistent` recomputes every pass or miss from it and
requires each gated decision to follow the numbers. Nothing here is asserted by a timing test.

## Targets (spec section 11) and how each is measured
| Row | Target | Measured as |
|---|---|---|
| `ingest_rate` | >= 40,000 records/s | records divided by the time spent inside `Writer.Add` and `End` (generator CPU excluded) |
| `list_p95_ms` | <= 50 ms | p95 of a 100-row page over the cells of `matrix.list.clean.*` that hit a time-ordered index (none, one to three types, deleted, time window, type + time window, min confidence + type, a page at depth) |
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
(filled by the first run: CPU model, cores, RAM, disk type, OS, Go version. The spec's reference machine is 8 cores, NVMe, 16 GiB.)

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
  "runs": [],
  "decisions": {
    "stats_plan": "pending",
    "search_plan": "pending",
    "open_artifact_cache": "pending",
    "ingest_hot_path": "pending",
    "parallel_verify": "pending"
  }
}
```

## Verify phases
(`verify_phases_s` of each run that ran the verify phase, as a table: phase, seconds, share of wall.)

## Decisions
(One line per decision: the rule, the rows, the run, the outcome.)

## Profiles
(Task 9: the top of the ingest CPU profile when the ingest decision is `apply`.)
