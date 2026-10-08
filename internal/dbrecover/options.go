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

// ExcludedTable names a table another parser claims, and the claimant.
type ExcludedTable struct{ Table, Parser, Version string }

// Options bound and narrow a run. The zero value is the default run.
type Options struct {
	Methods                       []string
	MinConfidence                 int
	ExcludeTables                 []ExcludedTable
	MaxRecords, MaxBytes          int64
	MaxPayloadBytes, MaxBodyBytes int64
	AllowReingest                 bool
	BatchRows                     int
	Progress                      func(Progress)
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
	SlackCopiesOfLive, SlackUnparsedBytes                                     int64
	LimitsHit                                                                 []string
	LibraryWarnings                                                           int
	Narrowed                                                                  []string
	ConfidenceRange                                                           map[string]ConfRange
	RangeOutside                                                              int64
}

// Result is the outcome of Run.
type Result struct {
	AnalysisID string
	Ingest     records.IngestResult
	Inputs     []InputInfo
	Siblings   SiblingReport
	Counts     Counts
	RulesHash  string
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
	for _, x := range o.ExcludeTables {
		if x.Table == "" {
			return o, nil, badOption("an excluded table has no name")
		}
	}
	if len(o.ExcludeTables) > 0 {
		narrowed = append(narrowed, "exclude-tables")
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

// filter decides which mapped rows are written. Task 10 gives it behaviour;
// until then it keeps every row.
type filter struct{}

func newFilter(Options) filter { return filter{} }

// keep reports whether the row is written and, when not, the reason it is
// counted under.
func (filter) keep(sqlitefile.RecoveredRow, int) (bool, string) { return true, "" }
