package ioswacalls

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
)

// env is what Parse hands the mapping: the place of the database, its app label and the aggregate set. It is nil in Mappings(),
// the typed recovery pass, which has no Parse context (4I2-P8, PF5): the mapping then derives the label from the Input of its
// MapContext and a recovered call takes no direction from the live aggregates.
type env struct {
	Place dbPlace
	Label appLabel
	Aggs  *aggSet
}

// callSpecs is the ZWACDCALLEVENT column list; the Required columns are the ones Probe insists on (spec 6.11 signature).
// Z1CALLEVENTS is not required: its absence is stated per call (no-aggregate-column), never a finding.
func callSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "Z_PK", Required: true, Affinity: "integer"},
		{Name: "ZDATE", Required: true, Affinity: "real"},
		{Name: "ZDURATION", Required: true, Affinity: "real"},
		{Name: colOutcome, Required: true, Affinity: "integer"},
		{Name: "Z_ENT", Affinity: "integer"},
		{Name: "Z1CALLEVENTS", Affinity: "integer"},
		{Name: colVideo, Affinity: "integer"},
		{Name: "ZBYTESSENT", Affinity: "integer"},
		{Name: "ZBYTESRECEIVED", Affinity: "integer"},
		{Name: "ZCALLIDSTRING", Affinity: "text"},
		{Name: "ZGROUPCALLCREATORUSERJIDSTRING", Affinity: "text"},
		{Name: "ZGROUPJIDSTRING", Affinity: "text"},
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// callMappingWith maps ZWACDCALLEVENT with the environment e (nil: the typed recovery pass): a closure over an immutable value,
// so the mapper stays a pure function of the row and its context.
func callMappingWith(e *env) parse.TableMapping {
	names := specNames(callSpecs())
	return parse.TableMapping{Table: tableCall, Columns: callSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return mapCall(m, row, e, names)
	}}
}

// rowLabel is the app label of the rows of this mapping: Parse's when it has one, else derived from the place (PF5): a domain
// names its app; a recovered row of an app group states "unknown" and "not-evaluated" (4I2-P8).
func rowLabel(m parse.MapContext, e *env) appLabel {
	if e != nil {
		return e.Label
	}
	if p := classifyPlace(m.Input().Primary.Logical); p.Place == placeDomain {
		return appLabel{App: p.App, Basis: "backup_domain"}
	}
	return appLabel{App: "unknown", State: "not-evaluated"}
}

// mapCall turns one ZWACDCALLEVENT row and its aggregate into the call record (spec 6.11, 7; 4I2-R4, R7). The counterparty is
// never stated: the participant table is not joined and the JIDs on the row are raw only. ZVIDEO is raw only (PF6): the kind is
// unknown.
func mapCall(m parse.MapContext, row parse.Row, e *env, names []string) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	raw, unknown := map[string]any{}, map[string]any{}
	rowLabel(m, e).raw(raw)
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl
	}
	r := cellReader{row: row, cols: rowrun.Resolve(m, tableCall, names...), raw: raw, unknown: unknown, prefix: prefixCall}
	pk, hasPK := row.Rowid()

	outcome := readCode(r, colOutcome)
	readCode(r, colVideo) // not mapped (PF6): the stored value is in raw, the kind is unknown
	for _, n := range []string{"Z_ENT", "ZBYTESSENT", "ZBYTESRECEIVED"} {
		if v, ok := r.int(n); ok {
			raw[n] = v
		}
	}
	for _, n := range []string{"ZGROUPJIDSTRING", "ZGROUPCALLCREATORUSERJIDSTRING", "ZCALLIDSTRING"} { // the call row's own JIDs: raw only (4I2-R4)
		if b, ok := r.text(n); ok {
			raw[n] = cleanField(raw, n, b)
		}
	}

	dir, err := directionOf(m, r, e, pk, hasPK)
	if err != nil {
		return nil, err
	}
	out := call.OutcomeUnknown
	switch l, _ := outcome.label(); {
	case l == labelAnswered:
		out = call.OutcomeAnswered
	case l == labelMissed && dir == call.DirIn:
		out = call.OutcomeMissed
	case l == labelMissed:
		raw["label_"+colOutcome] = labelMissed // a missed code on a call that is not known incoming is kept, not an outcome
	}

	cl := call.Call{Direction: dir, Outcome: out, Kind: call.KindUnknown, Service: channelWA}
	rec := records.Record{Type: call.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	var start *records.Time
	if n, ok := r.number("ZDATE"); ok {
		in := cellx.CocoaSeconds(n)
		in.Record(raw, "ZDATE")
		if in.Valid() {
			start = &records.Time{T: in.T}
			rec.Time = start
		}
	} else {
		r.nanStatus("ZDATE")
	}
	if dur, ok := duration(r); ok {
		cl.DurationS = &dur
		if start != nil {
			if end := endTime(start.T, dur); end != nil {
				rec.TimeEnd = end
				raw["time_end_basis"] = "start+duration"
			}
		}
	}

	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	cl.Raw = raw
	rec.Summary = call.Summary(cl.Direction, cl.Outcome, "")
	rec.Payload = cl.Payload()
	return []records.Record{rec}, nil
}

// aggregateColumns are the columns of the aggregate row that raw.aggregate states, as stored (4I2-A4).
func aggregateColumns() []string {
	return []string{colIncoming, colVideo, "ZMISSED", "ZMISSEDREASON"}
}

// directionOf decides the direction of a call and states raw.direction_state (the vocabulary of 4I2-R7). A call takes its
// direction only from an aggregate that links exactly this one call (4I2-R4); every other case is "unknown" with the reason. A
// recovered row (no Parse context) never borrows one from the live aggregates (Review Focus 4). raw.aggregate is stated whenever
// the aggregate row is read, whatever the number of calls it links (amendment 4I2-A4).
func directionOf(m parse.MapContext, r cellReader, e *env, pk int64, hasPK bool) (string, error) {
	raw := r.raw
	z1 := readCode(r, "Z1CALLEVENTS") // the key as stored, in raw whatever the direction
	if e == nil {
		raw["direction_state"] = stateRecover
		return call.DirUnknown, nil
	}
	state := func(s string) (string, error) {
		raw["direction_state"] = s
		return call.DirUnknown, nil
	}
	col := r.cols.At("Z1CALLEVENTS")
	if col < 0 || (e.Aggs != nil && e.Aggs.NoColumn) {
		return state("no-aggregate-column")
	}
	switch _, k := intCellOf(r.row, col); {
	case k == rowrun.CellNull || (k == rowrun.CellValue && z1.Value == 0):
		return state("no-aggregate")
	case k != rowrun.CellValue: // not an integer, or a cell that cannot be read: no key was read, so never "no-aggregate"
		return state(stateUndec)
	}
	as := e.Aggs
	// the aggregate row is read for every live call with a key, whatever the set says: only the direction is limited (4I2-A4)
	arow, st, err := getRow(m, tableAgg, z1.Value)
	if err != nil {
		return "", err
	}
	ar := cellReader{row: arow, cols: rowrun.Resolve(m, tableAgg, aggregateColumns()...), raw: raw, unknown: r.unknown, prefix: prefixAgg}
	switch st {
	case jUndecided:
		undecidedJoin(raw, tableAgg)
	case jFound:
		stated := map[string]any{"Z_PK": z1.Value}
		for _, n := range aggregateColumns() {
			ar.int(n) // flags a cell that is not an INTEGER
			if v := ar.stored(n); v != nil {
				stated[n] = v // as stored, whatever its class
			}
		}
		raw["aggregate"] = stated
	}
	if as == nil || !hasPK {
		return state(stateUndec) // an unknown rowid is never a decided reason
	}
	ent, ok := as.ByAgg[z1.Value]
	if ok && ent.Count >= 2 { // the calls that name the aggregate are the call table's own fact; on a lossy set a lower bound
		if !as.Lossy {
			raw["aggregate_calls"] = int64(min(ent.Count, 1000))
			if ent.Count > 1000 {
				raw["aggregate_calls_truncated"] = true
			}
		}
		return state("multi-call-aggregate")
	}
	if as.Lossy || !ok || ent.Count < 1 || ent.Only != pk {
		return state(stateUndec) // a lossy scan, a key the set did not read, or a set that disagrees with the row it was read from
	}
	switch st {
	case jUndecided:
		return state(stateUndec)
	case jNoRow:
		return state("no-ZWAAGGREGATECALLEVENT-row")
	}
	incoming, ik := intCellOf(arow, ar.cols.At(colIncoming))
	switch ik {
	case rowrun.CellMissing, rowrun.CellUnknown: // the column is lacking or unreadable: unknown, not "NULL"
		return state(stateUndec)
	case rowrun.CellValue:
		if l, ok := lookupEnum(colIncoming, incoming); ok {
			return l, nil
		}
	}
	return state("incoming-unknown")
}
