package records

import "time"

// Basis says how to read a timestamp.
type Basis string

const (
	// BasisUTC: the instant is known in UTC (the default when Basis is "").
	BasisUTC Basis = "utc"
	// BasisLocalOffset: a local time with a known offset from UTC (OffsetMin).
	BasisLocalOffset Basis = "local-offset"
	// BasisLocalUnknown: a local time whose zone is not known; T holds the
	// wall-clock reading as if it were UTC.
	BasisLocalUnknown Basis = "local-unknown"
)

// Time is a record timestamp. It is stored as Unix microseconds; a
// sub-microsecond remainder is truncated (parsers keep raw values in the
// payload).
type Time struct {
	T         time.Time
	Basis     Basis // "" means utc
	OffsetMin int   // minutes east of UTC; only with BasisLocalOffset, |x| < 1440
}

// NamedTime is a secondary timestamp of a record (created, accessed, ...).
type NamedTime struct {
	Kind string // [a-z][a-z0-9_]{1,31}; unique within a record, at most 16 per record
	Time Time
}

// Range is a byte range inside the artifact's content.
type Range struct{ Offset, Length int64 }

// Record is everything a parser knows about one record. Optional facts are
// pointers or empty strings so that a forgotten field never claims evidence (a
// zero byte range or a confidence of 0).
type Record struct {
	Type       string
	ArtifactID string
	SourcePath string // "" = the artifact itself (stored NULL)
	Locator    string // "" = none (stored NULL); "<scheme>:<k=v;...>", scheme [a-z][a-z0-9]{1,15}
	Range      *Range // nil = no byte range
	Time       *Time
	TimeEnd    *Time // requires Time
	Times      []NamedTime
	Deleted    bool
	Recovery   string // "" = live; else [a-z][a-z0-9-]{1,31}; requires Deleted
	Confidence *int   // nil = not stated; else 0..100
	Summary    string // "" allowed; stored as given
	Body       string // "" = NULL
	Payload    map[string]any
}

// Parser identifies the code that produced records.
type Parser struct {
	Name, Version string // [A-Za-z0-9][A-Za-z0-9._-]{0,63}
	Hash          string // "" = none (stored NULL, never as ""); else [A-Za-z0-9][A-Za-z0-9._:-]{0,127}
}
