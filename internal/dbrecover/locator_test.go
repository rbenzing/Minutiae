package dbrecover

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func row(table string, page uint32, cell int, o sqlitefile.Origin, f sqlitefile.FileKind) sqlitefile.RecoveredRow {
	return sqlitefile.RecoveredRow{Table: table, Origin: o, Loc: sqlitefile.Loc{File: f, Page: page, Cell: cell}}
}

func TestLocatorFormat(t *testing.T) {
	wal := row("t", 5, 2, sqlitefile.OriginWALSuperseded, sqlitefile.FileWAL)
	wal.Loc.Frame = 9
	jr := row("t", 6, 0, sqlitefile.OriginJournalBefore, sqlitefile.FileJournal)
	jr.Loc.Record = 3
	idx := row("", 7, 1, sqlitefile.OriginFreelistLeaf, sqlitefile.FileDB)
	idx.Index = "ix"
	slack := row("t", 8, -1, sqlitefile.OriginOrphan, sqlitefile.FileDB)
	slack.Loc.PageOffset, slack.Loc.Offset, slack.Loc.Length = 4096, 4096+120, 33
	for name, c := range map[string]struct {
		r    sqlitefile.RecoveredRow
		want string
	}{
		"freelist":      {row("t", 4, 0, sqlitefile.OriginFreelistLeaf, sqlitefile.FileDB), "sqlite:table=t;page=4;cell=0;freelist_page=4;file=db"},
		"wal frame":     {wal, "sqlite:table=t;page=5;cell=2;wal_frame=9;file=wal"},
		"journal":       {jr, "sqlite:table=t;page=6;cell=0;journal_rec=3;file=journal"},
		"db under wal":  {row("t", 2, 1, sqlitefile.OriginDBUnderWAL, sqlitefile.FileDB), "sqlite:table=t;page=2;cell=1;file=db"},
		"rolled back":   {row("t", 2, 1, sqlitefile.OriginDBRolledBack, sqlitefile.FileDB), "sqlite:table=t;page=2;cell=1;file=db"},
		"orphan":        {row("t", 3, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:table=t;page=3;cell=0;file=db"},
		"index entry":   {idx, "sqlite:index=ix;page=7;cell=1;freelist_page=7;file=db"},
		"empty table":   {row("", 3, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:page=3;cell=0;file=db"},
		"slack span":    {slack, "sqlite:table=t;page=8;span=120+33;file=db"},
		"uppercase pct": {row("a b/\u00e9", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:table=a%20b%2F%C3%A9;page=1;cell=0;file=db"},
	} {
		if got := locatorFor(c.r); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}
}

func pctDecode(t *testing.T, s string) string {
	t.Helper()
	var b []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b = append(b, s[i])
			continue
		}
		if i+3 > len(s) {
			t.Fatalf("truncated escape in %q", s)
		}
		h := s[i+1 : i+3]
		if strings.ToUpper(h) != h {
			t.Fatalf("lower-case escape %%%s", h)
		}
		v, err := strconv.ParseUint(h, 16, 8)
		if err != nil {
			t.Fatalf("bad escape %%%s", h)
		}
		b = append(b, byte(v))
		i += 2
	}
	return string(b)
}

func nameOf(loc string) string {
	for _, f := range strings.Split(strings.TrimPrefix(loc, "sqlite:"), ";") {
		if v, ok := strings.CutPrefix(f, "table="); ok {
			return v
		}
	}
	return ""
}

func TestLocatorPercentEncoding(t *testing.T) {
	for _, name := range []string{"a;b", "a=b", "a%b", "a\nb", "caf\u00e9", "a\u202eb", "~._-AZaz09", "a b", "\U0001F600"} {
		loc := locatorFor(row(name, 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB))
		enc := nameOf(loc)
		for i := 0; i < len(enc); i++ {
			c := enc[i]
			ok := c == '%' || c == '.' || c == '_' || c == '~' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
			if !ok {
				t.Errorf("%q: unescaped byte %q in %s", name, c, loc)
			}
		}
		if got := pctDecode(t, enc); got != name {
			t.Errorf("%q round-trips to %q", name, got)
		}
		if strings.Count(loc, ";") != 3 {
			t.Errorf("%q: field separators leaked: %s", name, loc)
		}
	}
	// a name that is not representable (NUL, invalid UTF-8) is left out, never mangled
	for _, name := range []string{"a\x00b", "a\xffb"} {
		if loc := locatorFor(row(name, 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)); loc != "sqlite:page=1;cell=0;file=db" {
			t.Errorf("%q: locator %s", name, loc)
		}
	}
}

func TestHugeTableNameLocatorIsCapped(t *testing.T) {
	for _, name := range []string{strings.Repeat("n", 1<<20), strings.Repeat("\u00e9", 1<<19), strings.Repeat("\u4e16", 300), strings.Repeat("a", 200), strings.Repeat("a", 201)} {
		loc := locatorFor(row(name, 1<<31, 1<<30, sqlitefile.OriginOrphan, sqlitefile.FileDB))
		if len(loc) > 1024 || !utf8.ValidString(loc) {
			t.Errorf("locator of %d bytes (valid %v)", len(loc), utf8.ValidString(loc))
		}
		dec := pctDecode(t, nameOf(loc))
		if len(name) > 200 {
			head, tail, ok := strings.Cut(dec, "~")
			if !ok || len(tail) != 8 || len(head) > 200 || !utf8.ValidString(head) || !strings.HasPrefix(name, head) {
				t.Errorf("long name not cut as specified: head %d bytes, tail %q", len(head), tail)
			}
		} else if dec != name {
			t.Errorf("a short name changed")
		}
	}
	a := nameOf(locatorFor(row(strings.Repeat("\u00e9", 200)+"a", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)))
	b := nameOf(locatorFor(row(strings.Repeat("\u00e9", 200)+"b", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)))
	if a == b {
		t.Error("distinct long names with a common head collide: the hash must cover the whole name")
	}
}

func FuzzLocatorFor(f *testing.F) {
	f.Add("t", "", uint32(1), 0, uint8(1), uint32(0), 0)
	f.Add("a;b=c%\x00", "ix", uint32(1<<31), -1, uint8(2), uint32(9), 5)
	f.Add(strings.Repeat("\u00e9", 500), "", uint32(7), 1<<30, uint8(3), uint32(1), 2)
	f.Fuzz(func(t *testing.T, table, index string, page uint32, cell int, file uint8, frame uint32, rec int) {
		r := sqlitefile.RecoveredRow{
			Table: table, Index: index, Origin: sqlitefile.Origin(file),
			Loc: sqlitefile.Loc{File: sqlitefile.FileKind(file), Page: page, Cell: cell, Frame: frame, Record: rec, Offset: int64(cell), Length: int64(rec)},
		}
		loc := locatorFor(r)
		if len(loc) > 1024 || !strings.HasPrefix(loc, "sqlite:") || !utf8.ValidString(loc) || strings.ContainsAny(loc, "\x00\n") {
			t.Fatalf("bad locator %q", loc)
		}
		if strings.Count(loc, "table=")+strings.Count(loc, "index=") > 1 {
			t.Fatalf("name leaked into the field structure: %q", loc)
		}
	})
}
