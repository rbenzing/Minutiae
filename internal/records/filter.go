package records

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Filter selects records. The zero value selects everything except the records
// of superseded runs.
type Filter struct {
	Types          []string
	From, To       *time.Time // [From, To) on ts
	IncludeUntimed bool       // NULL-ts rows match only when set and a time bound is present
	ArtifactIDs    []string
	PathPrefix     string // literal; compared with substr(), never LIKE
	Deleted        Tri
	Recovered      Tri
	Parsers        []ParserRef
	MinConfidence  *int // records without a confidence never match
	IngestID       string
	// IncludeSuperseded shows the records of superseded runs too; IngestID
	// implies it (asking for one run shows that run).
	IncludeSuperseded bool
}

// Limits on what a filter may carry.
const (
	maxInList     = 1000 // values in one IN list
	maxPathPrefix = 4096 // bytes; the cap on a stored source path
)

// The table aliases every reader query uses: r records, p parsers, b record_batches.
const (
	fromRecords = ` FROM records r`
	joinParsers = ` LEFT JOIN parsers p ON p.id = r.parser_id`
	joinBatches = ` LEFT JOIN record_batches b ON b.batch_id = r.batch_id`

	// supersededExists holds for a record whose run was superseded for its
	// artifact (record_superseded is the stored form of evidence.SupersededPairsSQL).
	supersededExists = `EXISTS (SELECT 1 FROM record_superseded s WHERE s.ingest_id = b.ingest_id AND s.artifact_id = r.artifact_id)`
)

// where is a compiled filter: constant SQL fragments joined with AND, with every
// value a ? placeholder bound in args. Nothing the caller supplies is ever
// spliced into the SQL text.
type where struct {
	conds      []string
	args       []any
	needParser bool // a condition reads p
	needBatch  bool // a condition reads b
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

// placeholders returns "?,?,?" for n values (n >= 1).
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func strArgs(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func invalidFilter(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFilter, fmt.Sprintf(format, a...))
}

// microsOf converts a filter time to Unix microseconds; a time that does not
// fit is an invalid filter.
func microsOf(name string, t time.Time) (int64, error) {
	us, ok := unixMicros(t)
	if !ok {
		return 0, invalidFilter("%s time does not fit in Unix microseconds", name)
	}
	return us, nil
}

// compile validates f and turns it into conditions. haveSuperseded says the
// record_superseded table is not empty: only then are the records of superseded
// runs hidden (one cheap EXISTS probe per call instead of a join per row).
func (f Filter) compile(haveSuperseded bool) (where, error) {
	var w where
	if len(f.Types) > maxInList || len(f.ArtifactIDs) > maxInList || len(f.Parsers) > maxInList {
		return w, invalidFilter("an IN list holds at most %d values", maxInList)
	}
	switch len(f.Types) {
	case 0:
	case 1:
		w.add("r.type = ?", f.Types[0])
	default:
		w.add("r.type IN ("+placeholders(len(f.Types))+")", strArgs(f.Types)...)
	}
	switch len(f.ArtifactIDs) {
	case 0:
	case 1:
		w.add("r.artifact_id = ?", f.ArtifactIDs[0])
	default:
		w.add("r.artifact_id IN ("+placeholders(len(f.ArtifactIDs))+")", strArgs(f.ArtifactIDs)...)
	}
	if f.From != nil || f.To != nil {
		var parts []string
		var args []any
		if f.From != nil {
			us, err := microsOf("From", *f.From)
			if err != nil {
				return w, err
			}
			parts, args = append(parts, "r.ts >= ?"), append(args, us)
		}
		if f.To != nil {
			us, err := microsOf("To", *f.To)
			if err != nil {
				return w, err
			}
			parts, args = append(parts, "r.ts < ?"), append(args, us)
		}
		cond := strings.Join(parts, " AND ")
		if f.IncludeUntimed {
			cond = "(" + cond + ") OR r.ts IS NULL"
		}
		w.add("("+cond+")", args...)
	}
	if f.PathPrefix != "" {
		if len(f.PathPrefix) > maxPathPrefix || !utf8.ValidString(f.PathPrefix) {
			return w, invalidFilter("path prefix is not valid UTF-8 within %d bytes", maxPathPrefix)
		}
		// substr counts characters; the comparison is binary, so case matters and
		// %, _ and \ are ordinary characters.
		w.add("substr(r.source_path, 1, ?) = ?", utf8.RuneCountInString(f.PathPrefix), f.PathPrefix)
	}
	for _, t := range []struct {
		name string
		v    Tri
		col  string
	}{{"Deleted", f.Deleted, "r.deleted"}, {"Recovered", f.Recovered, "r.recovered"}} {
		switch t.v {
		case Any:
		case Only:
			w.add(t.col + " = 1")
		case None:
			w.add(t.col + " = 0")
		default:
			return w, invalidFilter("%s is not Any, Only or None", t.name)
		}
	}
	if len(f.Parsers) > 0 {
		// IN lists, not a chain of ORs: SQLite limits an expression tree to
		// 1000 levels.
		var names []string
		var pairs []any
		for _, p := range f.Parsers {
			if p.Name == "" {
				return w, invalidFilter("a parser needs a name")
			}
			if p.Version == "" {
				names = append(names, p.Name)
			} else {
				pairs = append(pairs, p.Name, p.Version)
			}
		}
		var alts []string
		var args []any
		if len(names) > 0 {
			alts, args = append(alts, "p.name IN ("+placeholders(len(names))+")"), append(args, strArgs(names)...)
		}
		if len(pairs) > 0 {
			rows := strings.TrimSuffix(strings.Repeat("(?, ?),", len(pairs)/2), ",")
			alts, args = append(alts, "(p.name, p.version) IN (VALUES "+rows+")"), append(args, pairs...)
		}
		w.add("("+strings.Join(alts, " OR ")+")", args...)
		w.needParser = true
	}
	if f.MinConfidence != nil {
		if *f.MinConfidence < 0 || *f.MinConfidence > 100 {
			return w, invalidFilter("MinConfidence %d is outside 0..100", *f.MinConfidence)
		}
		w.add("r.confidence >= ?", *f.MinConfidence)
	}
	if f.IngestID != "" {
		// The ids of one ingest's batches lie in the span of its batch ranges, so
		// the span lets the planner skip the rest of the table; b.ingest_id is
		// the exact condition.
		w.add(`r.id BETWEEN (SELECT min(first_id) FROM record_batches WHERE ingest_id = ?)
			AND (SELECT max(first_id + count - 1) FROM record_batches WHERE ingest_id = ?)`, f.IngestID, f.IngestID)
		w.add("b.ingest_id = ?", f.IngestID)
		w.needBatch = true
	} else if haveSuperseded && !f.IncludeSuperseded {
		w.add("NOT " + supersededExists)
		w.needBatch = true
	}
	return w, nil
}

// whereSQL returns " WHERE a AND b" or "".
func (w where) whereSQL() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}
