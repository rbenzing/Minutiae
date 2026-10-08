package evidence

// V1Statements exposes the schema v1 DDL to the external test package, which
// may import internal/records/recordstest (an internal test cannot: import cycle).
var V1Statements = v1Statements

// SetReindexHook installs a test seam of ReindexText, called outside any transaction at
// "after-start-audit" (the records.reindex entry is written, the index untouched), "after-reset" (the
// tables are rebuilt empty and the state is building), "after-chunk" (once per committed chunk),
// "before-meta" (every record is indexed, the state is still building) and "after-meta" (the state is
// current, records.reindex.done is not written yet). An error returned at any point fails the reindex
// as a fault there would; a panic simulates a process that died there.
func (c *Case) SetReindexHook(f func(point string) error) { c.reindexHook = f }
