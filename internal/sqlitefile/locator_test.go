package sqlitefile_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func locRow(table string, page uint32, cell int, o sqlitefile.Origin, f sqlitefile.FileKind) sqlitefile.RecoveredRow {
	return sqlitefile.RecoveredRow{Table: table, Origin: o, Loc: sqlitefile.Loc{File: f, Page: page, Cell: cell}}
}

// TestRecoveredLocatorGolden pins the grammar of the recovered-row locator: the
// cases are those of dbrecover's TestLocatorFormat, which stays beside it.
func TestRecoveredLocatorGolden(t *testing.T) {
	wal := locRow("t", 5, 2, sqlitefile.OriginWALSuperseded, sqlitefile.FileWAL)
	wal.Loc.Frame = 9
	jr := locRow("t", 6, 0, sqlitefile.OriginJournalBefore, sqlitefile.FileJournal)
	jr.Loc.Record = 3
	idx := locRow("", 7, 1, sqlitefile.OriginFreelistLeaf, sqlitefile.FileDB)
	idx.Index = "ix"
	span := locRow("t", 8, -1, sqlitefile.OriginOrphan, sqlitefile.FileDB)
	span.Loc.PageOffset, span.Loc.Offset, span.Loc.Length = 4096, 4096+120, 33
	long := strings.Repeat("a", 199) + "\u00e9" + strings.Repeat("b", 100) // byte 200 is inside the e-acute: the cut falls back to 199
	sum := sha256.Sum256([]byte(long))
	nul := locRow("a\x00b\xff", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)
	for name, c := range map[string]struct {
		r    sqlitefile.RecoveredRow
		want string
	}{
		"freelist":         {locRow("t", 4, 0, sqlitefile.OriginFreelistLeaf, sqlitefile.FileDB), "sqlite:table=t;page=4;cell=0;freelist_page=4;file=db"},
		"freelist trunk":   {locRow("t", 4, 1, sqlitefile.OriginFreelistTrunk, sqlitefile.FileDB), "sqlite:table=t;page=4;cell=1;freelist_page=4;file=db"},
		"wal frame":        {wal, "sqlite:table=t;page=5;cell=2;wal_frame=9;file=wal"},
		"journal":          {jr, "sqlite:table=t;page=6;cell=0;journal_rec=3;file=journal"},
		"db under wal":     {locRow("t", 2, 1, sqlitefile.OriginDBUnderWAL, sqlitefile.FileDB), "sqlite:table=t;page=2;cell=1;file=db"},
		"rolled back":      {locRow("t", 2, 1, sqlitefile.OriginDBRolledBack, sqlitefile.FileDB), "sqlite:table=t;page=2;cell=1;file=db"},
		"orphan":           {locRow("t", 3, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:table=t;page=3;cell=0;file=db"},
		"index entry":      {idx, "sqlite:index=ix;page=7;cell=1;freelist_page=7;file=db"},
		"empty table":      {locRow("", 3, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:page=3;cell=0;file=db"},
		"span":             {span, "sqlite:table=t;page=8;span=120+33;file=db"},
		"uppercase pct":    {locRow("a b/\u00e9", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:table=a%20b%2F%C3%A9;page=1;cell=0;file=db"},
		"nul and bad utf8": {nul, "sqlite:table=a%00b%FF;page=1;cell=0;file=db"},
		"long name":        {locRow(long, 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB), "sqlite:table=" + strings.Repeat("a", 199) + "~" + hex.EncodeToString(sum[:4]) + ";page=1;cell=0;file=db"},
	} {
		if got := sqlitefile.RecoveredLocator(c.r); got != c.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, c.want)
		}
	}
	if sqlitefile.LocatorNameBytes != 200 {
		t.Errorf("LocatorNameBytes %d, want 200", sqlitefile.LocatorNameBytes)
	}
}

func TestRecoveredLocatorKeepsUnreservedBytes(t *testing.T) {
	got := sqlitefile.RecoveredLocator(locRow("~._-", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB))
	if want := "sqlite:table=~._-;page=1;cell=0;file=db"; got != want {
		t.Errorf("unreserved bytes were encoded: %s", got)
	}
}
