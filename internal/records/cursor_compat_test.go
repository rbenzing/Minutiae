package records

import (
	"strings"
	"testing"
)

// The golden values below were produced on BASE 028f386 (before the scan query existed) for the
// case identity "case-1\x00created-1".
const (
	goldSubCanon  = "records_fts_sub|any|false|\"abc\""
	goldSubFP     = "718f824ecb7934ac97175754dc74c98f"
	goldSubCursor = "v1.eyJkIjowLCJuIjowLCJ0cyI6MTcwMDAwMDAwMDAwMDAwMCwiaWQiOjcsImYiOiI3MThmODI0ZWNiNzkzNGFjOTcxNzU3NTRkYzc0Yzk4ZiJ9"

	goldWordCanon  = "records_fts|any|false|(\"hello\")"
	goldWordFP     = "095262160722cc75402430654438159e"
	goldWordCursor = "v1.eyJkIjowLCJuIjowLCJ0cyI6MTcwMDAwMDAwMDAwMDAwMCwiaWQiOjcsImYiOiIwOTUyNjIxNjA3MjJjYzc1NDAyNDMwNjU0NDM4MTU5ZSJ9"
)

// TestPlainQueryCursorIsUnchanged (M26b): the canonical form, fingerprint and cursors of word and
// substring queries are byte-identical to BASE, so a cursor issued before the scan existed is still
// accepted; only a scan query is canonically different.
func TestPlainQueryCursorIsUnchanged(t *testing.T) {
	const caseID = "case-1\x00created-1"
	for _, c := range []struct {
		in                 string
		o                  TextOptions
		canon, fp, encoded string
	}{
		{"abc", TextOptions{Substring: true}, goldSubCanon, goldSubFP, goldSubCursor},
		{"hello", TextOptions{}, goldWordCanon, goldWordFP, goldWordCursor},
	} {
		q, err := CompileQuery(c.in, c.o)
		if err != nil {
			t.Fatal(err)
		}
		if got := q.canonical(); got != c.canon {
			t.Errorf("canonical(%q) = %q, want %q", c.in, got, c.canon)
		}
		fp := Filter{Text: q}.fingerprint(caseID, false)
		if fp != c.fp {
			t.Errorf("fingerprint(%q) = %s, want %s", c.in, fp, c.fp)
		}
		if got := (cursor{ID: 7, TS: 1700000000000000, F: fp}).encode(); got != c.encoded {
			t.Errorf("cursor(%q) = %s, want %s", c.in, got, c.encoded)
		}
		dec, err := decodeCursor(c.encoded)
		if err != nil || dec.F != fp || dec.ID != 7 {
			t.Errorf("golden cursor of %q: %+v, %v", c.in, dec, err)
		}
	}
	sub, _ := CompileQuery("abc", TextOptions{Substring: true})
	scan, err := CompileQuery("abc", TextOptions{Substring: true, Scan: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(scan.canonical(), "scan|") {
		t.Errorf("scan canonical = %q, want the prefix scan|", scan.canonical())
	}
	if (Filter{Text: scan}).fingerprint(caseID, false) == (Filter{Text: sub}).fingerprint(caseID, false) {
		t.Error("a scan query has the fingerprint of the substring query")
	}
}
