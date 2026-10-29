package dbrecover

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Inputs names the artifacts of a run. DB is an artifact id or a manifest
// path; WAL and Journal are artifact ids; NoSiblings turns the search off.
type Inputs struct {
	DB           string
	WAL, Journal string
	NoSiblings   bool
}

// ExcludedTable names a table another parser claims, and the claimant. A
// Typed exclusion is a claim of a typed pass that concluded (TypedAnalysisID is
// its analysis): the table's rows were recorded as typed records, so leaving
// them out of this run loses nothing and it is not narrowed. An operator's
// exclusion (Typed false) is narrowing.
type ExcludedTable struct {
	Table, Parser, Version string
	Typed                  bool
	TypedAnalysisID        string
}

// NotHonoredClaim records a typed job whose claims the generic run did not take into account (4G-R26):
// the tables are recorded by the generic run, and the audit says why and which typed analysis also wrote
// (or tried to write) records for them.
type NotHonoredClaim struct {
	Parser, Version string
	Tables          []string
	TypedAnalysisID string
	Reason          string
}

// MaxKeepClaimed bounds Options.KeepClaimed (4G-R25).
const MaxKeepClaimed = 1 << 16

// Options bound and narrow a run. The zero value is the default run.
type Options struct {
	Methods       []string
	MinConfidence int
	ExcludeTables []ExcludedTable
	// KeepClaimed holds LocatorDigests of claimed rows the typed pass could not map (4G-R21): such a row
	// is written although its table is claimed. At most MaxKeepClaimed; not narrowing.
	KeepClaimed [][16]byte
	// TypedNotHonored lists the typed jobs that honored no claim, for the audit (4G-R26); it changes nothing else.
	TypedNotHonored               []NotHonoredClaim
	MaxRecords, MaxBytes          int64
	MaxPayloadBytes, MaxBodyBytes int64
	// MaxFitEqualityRows bounds the live rows the fit-only equality (an annotation on rows named by the
	// column fit alone) reads in all; 0 means the library default. Past it the annotation is unknown.
	MaxFitEqualityRows int64
	AllowReingest      bool
	BatchRows          int
	Progress           func(Progress)
}

// Progress is reported while rows are mapped.
type Progress struct{ Rows, Skipped int64 }

// InputInfo describes one input of a run.
type InputInfo struct {
	Role, ArtifactID, SHA256 string
	Size                     int64
	Recovered, Incomplete    bool
	Cap                      *int
	Facts                    map[string]any
}

// ConfRange is the lowest and highest confidence of the rows of one method.
type ConfRange struct{ Min, Max int }

// Counts is what a run (or a plan) found.
type Counts struct {
	Written                                                                   int64
	ByMethod                                                                  map[string]int64
	SkippedByReason                                                           map[string]int64
	DuplicateOfLive, CellsRejected, InteriorSkipped, RelationUnknown, FitOnly int64
	FreePages, FreePagesZeroed                                                int64
	PagesByOrigin                                                             map[string]int64
	SlackCopiesOfLive, SlackUnparsedBytes, SlackDroppedCrossed                int64
	SlackDroppedOverlap, SlackCrossPageEqual, SlackWorkCapped                 int64
	SlackRowCapped                                                            bool
	FitEqualityCapped                                                         int64
	RecoveryCapUnknown                                                        int64 // written rows capped at the lowest method confidence because an input's cap is unknown (J116)
	LimitsHit                                                                 []string
	LibraryWarnings                                                           int
	Narrowed                                                                  []string
	ConfidenceRange                                                           map[string]ConfRange
	RangeOutside                                                              int64
	KeptUnmappedClaimed                                                       int64
}

// Result is the outcome of Run.
type Result struct {
	AnalysisID string
	Ingest     records.IngestResult
	// WriterStarted is true once the records writer has started (an ingest exists); false for a run that stopped before.
	WriterStarted bool
	Inputs        []InputInfo
	Siblings      SiblingReport
	Counts        Counts
	RulesHash     string
}

// Assessment is the outcome of Plan: what a Run would do, with nothing written.
type Assessment struct {
	Inputs    []InputInfo
	Siblings  SiblingReport
	Counts    Counts
	RulesHash string
	Note      string
}

// Run bounds (D12).
const (
	defaultMaxRecords = 1_000_000
	maxRecordsCeiling = 50_000_000
	defaultMaxBytes   = 8 << 30
	maxBatchRows      = 20_000
	// MaxFitEqualityRowsCeiling is the largest MaxFitEqualityRows (the library's own ceiling).
	MaxFitEqualityRowsCeiling = 1 << 29
)

func badOption(format string, a ...any) error {
	return &RefusalError{Reason: ReasonBadOption, Detail: fmt.Sprintf(format, a...)}
}

// normalized checks the options and returns them with the defaults filled in
// and the narrowing they cause listed (D13).
func (o Options) normalized() (Options, []string, error) {
	var narrowed []string
	valid := map[string]bool{}
	for _, m := range Methods() {
		valid[m] = true
	}
	if len(o.Methods) > 0 {
		set := map[string]bool{}
		for _, m := range o.Methods {
			if !valid[m] {
				return o, nil, badOption("unknown recovery method %q (known: %s)", m, strings.Join(Methods(), ", "))
			}
			set[m] = true
		}
		o.Methods = slices.Sorted(maps.Keys(set))
		if len(set) < len(valid) {
			narrowed = append(narrowed, "methods")
		}
	}
	if o.MinConfidence < 0 || o.MinConfidence > 100 {
		return o, nil, badOption("minimum confidence %d is outside 0..100", o.MinConfidence)
	}
	if o.MinConfidence > 0 {
		narrowed = append(narrowed, "min-confidence")
	}
	operatorExclusion := false
	for _, x := range o.ExcludeTables {
		switch {
		case x.Table == "":
			return o, nil, badOption("an excluded table has no name")
		case x.Typed && x.TypedAnalysisID == "":
			return o, nil, badOption("the typed exclusion of table %q names no typed analysis", x.Table)
		case !x.Typed && x.TypedAnalysisID != "":
			return o, nil, badOption("the exclusion of table %q names a typed analysis but is not typed", x.Table)
		}
		operatorExclusion = operatorExclusion || !x.Typed
	}
	if operatorExclusion {
		narrowed = append(narrowed, "exclude-tables")
	}
	if len(o.KeepClaimed) > MaxKeepClaimed {
		return o, nil, badOption("%d kept claimed locators exceed the limit of %d", len(o.KeepClaimed), MaxKeepClaimed)
	}
	switch {
	case o.MaxRecords < 0 || o.MaxRecords > maxRecordsCeiling:
		return o, nil, badOption("max records %d is outside 0..%d", o.MaxRecords, maxRecordsCeiling)
	case o.MaxBytes < 0:
		return o, nil, badOption("max bytes %d is negative", o.MaxBytes)
	case o.MaxPayloadBytes < 0 || o.MaxPayloadBytes > MaxPayloadBytesCeiling:
		return o, nil, badOption("max payload bytes %d is outside 0..%d", o.MaxPayloadBytes, MaxPayloadBytesCeiling)
	case o.MaxBodyBytes < 0 || o.MaxBodyBytes > MaxBodyBytesCeiling:
		return o, nil, badOption("max body bytes %d is outside 0..%d", o.MaxBodyBytes, MaxBodyBytesCeiling)
	case o.MaxFitEqualityRows < 0 || o.MaxFitEqualityRows > MaxFitEqualityRowsCeiling:
		return o, nil, badOption("max fit equality rows %d is outside 0..%d", o.MaxFitEqualityRows, MaxFitEqualityRowsCeiling)
	case o.BatchRows < 0 || o.BatchRows > maxBatchRows:
		return o, nil, badOption("batch rows %d is outside 0..%d", o.BatchRows, maxBatchRows)
	}
	if o.MaxRecords == 0 {
		o.MaxRecords = defaultMaxRecords
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.MaxPayloadBytes == 0 {
		o.MaxPayloadBytes = DefaultMaxPayloadBytes
	}
	if o.MaxBodyBytes == 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return o, narrowed, nil
}

// filter decides which mapped rows are written (Task 10). The checks run in a
// fixed order, so a row is counted under exactly one reason: its method, then
// its capped confidence, then a claim on its table.
type filter struct {
	methods map[string]bool // nil: every method
	minConf int
	claimed map[string]bool // ASCII-folded names of the tables another parser claims
	live    *sqlitefile.Schema
	kept    map[[16]byte]struct{} // digests of claimed rows written anyway (4G-R21)
}

// reasonKeptClaimed is what keep reports, with true, for a claimed row that is written because its digest is in KeepClaimed.
const reasonKeptClaimed = "kept-unmapped-claimed"

// newFilter builds the filter of a run; live is the LIVE schema, against which
// a claimed row must strictly fit (sqlitefile.ClaimMatch).
func newFilter(o Options, live *sqlitefile.Schema) filter {
	f := filter{minConf: o.MinConfidence, live: live}
	if len(o.Methods) > 0 {
		f.methods = map[string]bool{}
		for _, m := range o.Methods {
			f.methods[m] = true
		}
	}
	if len(o.ExcludeTables) > 0 {
		f.claimed = map[string]bool{}
		for _, x := range o.ExcludeTables {
			f.claimed[asciiFold(x.Table)] = true
		}
	}
	if len(o.KeepClaimed) > 0 {
		f.kept = make(map[[16]byte]struct{}, len(o.KeepClaimed))
		for _, d := range o.KeepClaimed {
			f.kept[d] = struct{}{}
		}
	}
	return f
}

// keep reports whether the row is written and, when not, the reason it is
// counted under. conf is the confidence after the input cap. A row is excluded
// as claimed only when sqlitefile.ClaimMatch says so: a table row of basis
// schema or fit whose values strictly fit the live definition of its table. A
// guess, a row of no table, an index entry and a row carved from page slack are
// kept (D7, 4G-R3, 4G-R22).
func (f filter) keep(r sqlitefile.RecoveredRow, conf int) (bool, string) {
	switch {
	case f.methods != nil && !f.methods[r.Method]:
		return false, "method-filtered"
	case conf < f.minConf:
		return false, "below-min-confidence"
	case f.claimed != nil && f.live != nil && f.live.ClaimMatch(r, f.isClaimed) == sqlitefile.ClaimYes:
		if _, ok := f.kept[LocatorDigest(locatorFor(r))]; ok {
			return true, reasonKeptClaimed
		}
		return false, "excluded-by-claim"
	}
	return true, ""
}

// isClaimed reports whether the table is one a parser claims (ASCII fold).
func (f filter) isClaimed(table string) bool { return f.claimed[asciiFold(table)] }

// asciiFold lowercases the ASCII letters only, as SQLite compares names.
func asciiFold(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
