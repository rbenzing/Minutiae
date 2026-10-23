package evidence

import (
	"database/sql"
	"time"
)

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

// AllowUndescribedRecoveredKinds lets this Case write a recovered kind without a Recovery, so tests can
// plant what a buggy writer would have produced and prove that verify catches it. Test-only.
func (c *Case) AllowUndescribedRecoveredKinds() { c.noRecoveredKindGate = true }

// SetPhaseHook installs an observer of the verify phase timings. Test-only.
func (c *Case) SetPhaseHook(f func(string, time.Duration)) { c.verifyPhaseHook = f }

// ManifestReads is the number of times the manifest file was read and decoded by this Case.
func (c *Case) ManifestReads() int64 { return c.manifestReads.Load() }

// FTSReplicaToken is one token of a Go replica of an FTS5 tokenizer: the term and its position in the column.
type FTSReplicaToken struct {
	Term string
	Pos  int
}

// FTSReplicaTokens returns the tokens the replica of the tokenizer of an FTS table (FTSWordTable or
// FTSSubTable) produces for text, or nil for any other table name.
func FTSReplicaTokens(table, text string) []FTSReplicaToken {
	tok := ftsTokenizer(table)
	if tok == nil {
		return nil
	}
	var out []FTSReplicaToken
	tok(text, func(term []byte, pos int) { out = append(out, FTSReplicaToken{string(term), pos}) })
	return out
}

// FTSProbeCorpus is the fixed corpus of the tokenizer self-test.
func FTSProbeCorpus() []string { return ftsProbeCorpus() }

// FTSReplicaMaxTokenSize is the byte length at which FTS5 cuts a token.
const FTSReplicaMaxTokenSize = fts5MaxTokenSize

// NormalizeCorpusInputs returns the input strings of TestNormalizeTable and of
// TestNormalizeAndTokenizersAgree, so the tokenizer replicas can be run over them from the external test package.
func NormalizeCorpusInputs() []string {
	var out []string
	for _, c := range normalizeTableCases {
		out = append(out, c.in, c.want)
	}
	for _, class := range normalizeAgreeClasses {
		out = append(out, class...)
	}
	return out
}

// SetFTSReplicaOverride makes the tokenizer of a table answer f (nil keeps the replica) until the returned
// function is called. It is the only way outside this package's own tests to set the override, and it
// lives in a test file so that no production code can (D142).
func SetFTSReplicaOverride(f func(table string) func(string, func([]byte, int))) (restore func()) {
	old := ftsReplicaOverride
	ftsReplicaOverride = f
	return func() { ftsReplicaOverride = old }
}

// FTSBucketReport is what the index pass of P11 concluded: the number of (table, bucket) pairs whose digests
// were compared, and the number of failing buckets of each table (word, substring).
type FTSBucketReport struct {
	Compared int
	Failing  [2]int
}

// SetFTSBucketObserver installs a test seam called once, when the index pass of P11 is done.
func (c *Case) SetFTSBucketObserver(f func(FTSBucketReport)) {
	c.verifyFTSInspect = func(r *ftsVerify) {
		f(FTSBucketReport{Compared: r.bucketsCompared, Failing: [2]int{len(r.failing[0]), len(r.failing[1])}})
	}
}

// SetFTSHook installs a test seam called at the named points of P11: "after-index" (the index pass is over),
// "after-open" (the scratch area of the first localization round exists), "after-rebuild" (the records of a
// localization round are rebuilt) and "compared:<table>" (a round's comparison of one table is over).
func (c *Case) SetFTSHook(f func(point string)) {
	c.verifyFTSHook = func(point string, _ *sql.DB) { f(point) }
}

// FTSLocalizeReport is what P11 concluded about localization.
type FTSLocalizeReport struct {
	Rebuilt int // buckets rebuilt with SQLite's tokenizer
	Done    int // (table, bucket) pairs whose comparison ran
}

// SetFTSLocalizeObserver installs a test seam called once, when P11 is over.
func (c *Case) SetFTSLocalizeObserver(f func(FTSLocalizeReport)) {
	c.verifyFTSDone = func(r *ftsVerify) {
		var rep FTSLocalizeReport
		if r.lc != nil {
			rep.Rebuilt = r.lc.rebuilt
			rep.Done = len(r.lc.done[0]) + len(r.lc.done[1])
		}
		f(rep)
	}
}

// SetFTSLocalizeLimits replaces the bounds of the localization (buckets, expected postings per round, found rows
// per round) until the returned function is called; a zero keeps the default of that bound.
func SetFTSLocalizeLimits(maxBuckets, roundPostings, foundCap int) (restore func()) {
	ob, or, of := ftsLocalizeMaxBuckets, ftsLocalizeRoundPostings, ftsLocalizeFoundCap
	if maxBuckets != 0 {
		ftsLocalizeMaxBuckets = maxBuckets
	}
	if roundPostings != 0 {
		ftsLocalizeRoundPostings = roundPostings
	}
	if foundCap != 0 {
		ftsLocalizeFoundCap = foundCap
	}
	return func() { ftsLocalizeMaxBuckets, ftsLocalizeRoundPostings, ftsLocalizeFoundCap = ob, or, of }
}
