package evidence

import "time"

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
func FTSProbeCorpus() []string { return ftsProbeCorpus }

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
