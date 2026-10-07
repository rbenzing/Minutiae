package evidence

import (
	"slices"
	"strings"
)

// Source.Kind values of artifacts that hold recovered (not live) data.
const (
	KindRecover = "recover"
	KindCarve   = "carve"
	KindSlack   = "slack"
	KindJournal = "journal"
	KindReport  = "report"
)

// Recovery classes.
const (
	ClassDeletedFile        = "deleted-file"
	ClassPostCheckpointFile = "post-checkpoint-file"
	ClassCarved             = "carved"
	ClassSlack              = "slack"
	ClassJournalBlock       = "journal-block"
	ClassJournalReport      = "journal-report"
)

// AlgorithmRecover names the version of the rules that produce recovered artifacts.
const AlgorithmRecover = "minutiae-recover/1"

// IsRecoveredKind reports whether kind is one of the five recovered kinds.
func IsRecoveredKind(kind string) bool {
	switch kind {
	case KindRecover, KindCarve, KindSlack, KindJournal, KindReport:
		return true
	}
	return false
}

// Recovery is the explicit provenance of a recovered artifact. It is part of
// Derivation, so the artifact.create audit entry covers every field.
type Recovery struct {
	Class       string            `json:"class"`
	Method      string            `json:"method"`
	Confidence  *int              `json:"confidence,omitempty"`
	Basis       []string          `json:"basis,omitempty"`
	Assumptions []string          `json:"assumptions,omitempty"`
	Alloc       AllocSummary      `json:"alloc"`
	Excluded    []Run             `json:"excluded,omitempty"`
	Scope       string            `json:"scope,omitempty"`
	Content     string            `json:"content,omitempty"`
	Algorithm   string            `json:"algorithm"`
	Params      map[string]string `json:"params,omitempty"`
	Journal     *JournalRef       `json:"journal,omitempty"`
}

// AllocSummary counts the bytes of the RECORDED (captured) runs by allocation
// state; what the metadata named but was not captured is Recovery.Excluded
// plus the exact ExcludedRuns/ExcludedBytes counters.
type AllocSummary struct {
	Free          int64 `json:"free"`
	Allocated     int64 `json:"allocated"`
	Unknown       int64 `json:"unknown"`
	ExcludedRuns  int   `json:"excluded_runs,omitempty"`
	ExcludedBytes int64 `json:"excluded_bytes,omitempty"`
}

// JournalRef names the journal transaction a journal-derived artifact came from.
type JournalRef struct {
	Seq       uint32 `json:"seq"`
	Committed bool   `json:"committed"`
	Region    string `json:"region"` // "live" | "stale"
	Revoked   bool   `json:"revoked,omitempty"`
}

// ClassInfo is one row of the version-neutral class table.
type ClassInfo struct {
	Class, Kind, Namespace string
	MethodPrefixes         []string
	MaxConfidence          int
	ConfidenceRequired     bool
	ScopeRequired          bool
	// Scope is the one scope the class is bound to (empty: the artifact chooses, see ScopeRequired).
	Scope           string
	JournalRequired bool
}

// ClassInfos returns a copy of the class table in a stable order.
func ClassInfos() []ClassInfo {
	out := make([]ClassInfo, len(classTable))
	for i, ci := range classTable {
		ci.MethodPrefixes = slices.Clone(ci.MethodPrefixes)
		out[i] = ci
	}
	return out
}

// LookupClass returns the table row of class.
func LookupClass(class string) (ClassInfo, bool) {
	for _, ci := range ClassInfos() {
		if ci.Class == class {
			return ci, true
		}
	}
	return ClassInfo{}, false
}

// classTable is version-neutral: it lists method PREFIXES, so a new method of
// a known family needs no change here. Confidence is required for every class
// that claims a file or bytes of a file system; the journal classes describe
// transactions, for which a confidence may not apply. post-checkpoint-file
// requires one until plan 3H rules otherwise.
var classTable = []ClassInfo{
	{
		Class: ClassDeletedFile, Kind: KindRecover, Namespace: nsRecovered,
		MethodPrefixes: []string{"fat-", "exfat-", "ext-", "ext4-", "f2fs-node"},
		MaxConfidence:  80, ConfidenceRequired: true,
	},
	{
		Class: ClassPostCheckpointFile, Kind: KindRecover, Namespace: nsRecovered,
		MethodPrefixes: []string{"f2fs-rollforward"},
		MaxConfidence:  80, ConfidenceRequired: true,
	},
	{
		Class: ClassCarved, Kind: KindCarve, Namespace: "carved",
		MethodPrefixes: []string{"carve-"},
		MaxConfidence:  70, ConfidenceRequired: true, ScopeRequired: true,
	},
	{
		Class: ClassSlack, Kind: KindSlack, Namespace: "slack",
		MethodPrefixes: []string{"slack-"},
		MaxConfidence:  10, ConfidenceRequired: true, Scope: "unallocated",
	},
	{
		Class: ClassJournalBlock, Kind: KindJournal, Namespace: "journal",
		MethodPrefixes: []string{"ext4-journal", "f2fs-rollforward"},
		MaxConfidence:  100, JournalRequired: true, Scope: "unallocated",
	},
	{
		Class: ClassJournalReport, Kind: KindReport, Namespace: "reports",
		MethodPrefixes: []string{"ext4-journal", "f2fs-rollforward"},
		MaxConfidence:  100, Scope: "unallocated",
	},
}

const nsRecovered = "recovered"

// ConfidenceBand names the band of a confidence: high >= 70, medium 40-69,
// low 0-39, and "" outside 0..100.
func ConfidenceBand(c int) string {
	switch {
	case c < 0 || c > 100:
		return ""
	case c >= 70:
		return "high"
	case c >= 40:
		return "medium"
	}
	return "low"
}

// RecoveredNamespace returns the recovered namespace of an artifact path of the
// form artifacts/<device>/<acquisition>/<namespace>/<file...>. Only paths with
// at least five components have a namespace, and only the five recovered ones
// count (a live artifact is under p<N>-<fstype>, volume or a device directory).
func RecoveredNamespace(artifactPath string) (string, bool) {
	parts := strings.Split(artifactPath, "/")
	if len(parts) < 5 || parts[0] != artifactsDir {
		return "", false
	}
	for _, ci := range classTable {
		if parts[3] == ci.Namespace {
			return ci.Namespace, true
		}
	}
	return "", false
}
