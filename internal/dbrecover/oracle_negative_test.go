package dbrecover

// The oracle comparison must be able to fail (Task 13): these tests feed it
// wrong answers and require a refusal, so a comparison that accepts anything
// cannot hide behind a passing TestSQLiteRecoverMatchesOracle.

import (
	"encoding/json"
	"testing"
)

func TestOracleColumnComparisonRejectsWrongValues(t *testing.T) {
	num := func(s string) json.Number { return json.Number(s) }
	good := []struct {
		name string
		col  map[string]any
		v    oVal
	}{
		{"int", map[string]any{"type": "integer", "value": num("42")}, oVal{"int", "42"}},
		{"text", map[string]any{"type": "text", "value": "abc"}, oVal{"text", "abc"}},
		{"null", map[string]any{"type": "null"}, oVal{T: "null"}},
		{"float bits", map[string]any{"type": "real", "float_bits": "3ff8000000000000"}, oVal{"float", "3ff8000000000000"}},
		{"blob", map[string]any{"type": "blob", "value_b64": "AQI="}, oVal{"blob", "AQI="}},
	}
	for _, c := range good {
		if err := colMatches(c.col, c.v); err != nil {
			t.Errorf("%s: the equal value was refused: %v", c.name, err)
		}
	}
	bad := []struct {
		name string
		col  map[string]any
		v    oVal
	}{
		{"int differs", map[string]any{"type": "integer", "value": num("43")}, oVal{"int", "42"}},
		{"kind differs", map[string]any{"type": "text", "value": "42"}, oVal{"int", "42"}},
		{"text differs", map[string]any{"type": "text", "value": "abd"}, oVal{"text", "abc"}},
		{"float bits differ", map[string]any{"type": "real", "float_bits": "3ff8000000000001"}, oVal{"float", "3ff8000000000000"}},
		{"blob differs", map[string]any{"type": "blob", "value_b64": "AQM="}, oVal{"blob", "AQI="}},
		{"omitted column", map[string]any{"type": "text", "omitted": true}, oVal{"text", "abc"}},
		{"clipped column", map[string]any{"type": "text", "value": "ab", "clipped": true}, oVal{"text", "ab"}},
	}
	for _, c := range bad {
		if err := colMatches(c.col, c.v); err == nil {
			t.Errorf("%s: a wrong value was accepted", c.name)
		}
	}
}

func TestOracleFramesAndRecordsComeFromTheOracleOnly(t *testing.T) {
	var f oFixture
	if _, ok := f.frameOf(100); ok {
		t.Error("a frame was found without a WAL oracle")
	}
	if _, ok := f.journalRecordOf(100, 1024); ok {
		t.Error("a journal record was found without a journal oracle")
	}
	f = *loadOracleFixture(t, "builder-wal-generations")
	if _, ok := f.frameOf(5); ok {
		t.Error("an offset inside the WAL header was assigned to a frame")
	}
	f = *loadOracleFixture(t, "builder-hot-journal")
	if _, ok := f.journalRecordOf(1, f.pageSize()); ok {
		t.Error("an offset inside the journal header was assigned to a record")
	}
}

func TestFixturePinsDetectAnyChange(t *testing.T) {
	f := loadOracleFixture(t, "basic-utf8-4k")
	changed := append([]byte(nil), f.db...)
	changed[len(changed)/2] ^= 1
	if shaHex(changed) == pinnedFixtureSHA256["basic-utf8-4k.db"] {
		t.Error("a changed fixture keeps its pin")
	}
	if shaHex(f.db) != pinnedFixtureSHA256["basic-utf8-4k.db"] {
		t.Error("the pin is not the fixture's hash")
	}
}

func TestRequireWrittenFlagsAnInventedRow(t *testing.T) {
	l := newGenLog()
	l.add(1, 1000, "written")
	n1, n2 := json.Number("1"), json.Number("1000")
	rowid, bad := json.Number("2"), json.Number("1001")
	ok := genRecord{rowid: &n1, a: &n2, b: "written", bOK: true}
	if !l.rows[ok.key()] || !l.tuples["1000|written"] {
		t.Fatal("the written row is not in the log")
	}
	for name, g := range map[string]genRecord{
		"another rowid": {rowid: &rowid, a: &n2, b: "written", bOK: true},
		"another value": {rowid: &n1, a: &bad, b: "written", bOK: true},
		"another text":  {rowid: &n1, a: &n2, b: "writtem", bOK: true},
	} {
		if l.rows[g.key()] {
			t.Errorf("%s: an invented row is in the log", name)
		}
	}
}
