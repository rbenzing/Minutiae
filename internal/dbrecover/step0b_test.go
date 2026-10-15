package dbrecover

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func TestOutsideRangeRowKeepsItsLocationClaim(t *testing.T) {
	r := baseRow()
	r.Loc.Offset, r.Loc.Length = 5000, 60
	m := testMapper(map[sqlitefile.FileKind]int64{sqlitefile.FileDB: 1000})
	rec := mustRecord(t, m, r)
	if rec.Range != nil {
		t.Fatalf("range %v", rec.Range)
	}
	if rec.Payload["loc_offset"] != int64(5000) || rec.Payload["loc_length"] != int64(60) {
		t.Errorf("claim lost: %v %v", rec.Payload["loc_offset"], rec.Payload["loc_length"])
	}
	if err := validateDBRow(rec.Payload); err != nil {
		t.Error(err)
	}
	in := mustRecord(t, dbOnly(), baseRow())
	if _, has := in.Payload["loc_offset"]; has {
		t.Error("an in-range row carries loc_offset")
	}
}

func TestClippedTextIsMarkedInSummaryAndBody(t *testing.T) {
	r := baseRow()
	c := textV([]byte("ab"), sqlitefile.EncUTF8)
	c.Len, c.Clipped = 10, true
	r.Values = []sqlitefile.Value{c}
	rec := mustRecord(t, dbOnly(), r)
	if !strings.Contains(rec.Summary, "[clipped]") {
		t.Errorf("summary %q", rec.Summary)
	}
	if !strings.Contains(rec.Body, "[clipped]") {
		t.Errorf("body %q lacks the inline mark", rec.Body)
	}
	if rec.Payload["body_clipped"] != nil {
		t.Errorf("body_clipped %v: the body was not cut at its limit", rec.Payload["body_clipped"])
	}
	if rec.Payload["values_clipped"] != int64(1) {
		t.Errorf("values_clipped %v", rec.Payload["values_clipped"])
	}
	if err := validateDBRow(rec.Payload); err != nil {
		t.Error(err)
	}
	clean := mustRecord(t, dbOnly(), baseRow())
	if strings.Contains(clean.Summary, "clipped") || clean.Payload["body_clipped"] != nil {
		t.Errorf("clean row marked: %q", clean.Summary)
	}
}

func TestBodyClippedMeansCutAtTheByteLimit(t *testing.T) {
	r := baseRow()
	r.Values = []sqlitefile.Value{textV([]byte(strings.Repeat("a", 5000)), sqlitefile.EncUTF8)}
	m := dbOnly()
	m.maxBody = 100
	rec := mustRecord(t, m, r)
	if rec.Payload["body_clipped"] != true {
		t.Errorf("body_clipped %v, want true (len %d)", rec.Payload["body_clipped"], len(rec.Body))
	}
	if rec.Payload["values_clipped"] != nil || strings.Contains(rec.Body, "[clipped]") {
		t.Errorf("an unclipped input value was marked clipped: %v", rec.Payload["values_clipped"])
	}
}
