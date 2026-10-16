package androidmms_test

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// Spec 10.6: every committed DDL variant of this family appears in at least one fixture.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "android-telephony", "mmssms-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		used[parsertest.LoadFixture(t, "testdata", v).Expect.DDL] = true
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "mmssms-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
	}
}

func run(t *testing.T, variant string) (parsertest.Result, parsertest.Bundle) {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", variant).Bundle(h, remotePath)
	p := androidmms.Parser{}
	return h.RunLenient(p, b.Input(h, p, false)), b
}

// Review Focus 3: rows that are deleted but still in the file are not live.
func TestFreelistResidueIsNotEmitted(t *testing.T) {
	res, _ := run(t, "av-freelist-512")
	if len(res.Records) != 149 {
		t.Fatalf("%d records, want 149 (300 inserted, rows 100 to 250 deleted)", len(res.Records))
	}
	for _, r := range res.Records {
		if strings.Contains(r.Body, "message 100 ") || strings.Contains(r.Body, "message 200 ") {
			t.Errorf("a deleted row came back as a live record: %s", r.Locator)
		}
		if r.Deleted || r.Recovery != "" {
			t.Errorf("%s: a live parse set Deleted/Recovery", r.Locator)
		}
	}
}

func TestWALRowsAreAttributedToTheWALArtifact(t *testing.T) {
	res, b := run(t, "wal-uncheckpointed")
	wal := b.ByRole["wal"]
	for _, id := range []string{"2", "7", "8", "9"} {
		var found bool
		for _, r := range res.Records {
			if strings.HasPrefix(r.Locator, "sqlite:table=sms;rowid="+id+";wal_frame=") {
				found = true
				if r.ArtifactID != wal.ID || r.Range == nil || r.Range.Offset+r.Range.Length > wal.Size {
					t.Errorf("row %s: artifact %s range %+v, want the WAL artifact %s", id, r.ArtifactID, r.Range, wal.ID)
				}
			}
		}
		if !found {
			t.Errorf("row %s, written only in the WAL, has no wal_frame locator", id)
		}
	}
	if len(res.Records) != 9 { // rows 1 to 6 from the database, 7 to 9 only in the WAL (row 2 is edited in place)
		t.Errorf("%d records, want 9", len(res.Records))
	}
}

// Ruling T10: if the reader does not put journal-hot into DB.Warnings(), forward Info().Journal.Hot from
// rowrun.Run as one warning (locator sqlite:file=journal;page=0;offset=0, reason "journal-hot: ..."); never weaken this test.
func TestHotJournalRowsAreLiveAndAttributedToTheFileThatHoldsTheCell(t *testing.T) {
	res, b := run(t, "hot-journal")
	var inJournal int
	for _, r := range res.Records {
		if strings.Contains(r.Locator, ";journal_rec=") {
			inJournal++
			if r.ArtifactID != b.ByRole["journal"].ID {
				t.Fatalf("%s: artifact %s, want the journal", r.Locator, r.ArtifactID)
			}
		}
		if strings.HasPrefix(r.Body, "interrupted-") {
			t.Fatalf("%s: the uncommitted transaction was emitted as live", r.Locator)
		}
	}
	if inJournal == 0 {
		t.Error("no record lies in the journal: the fixture does not exercise the rolled-back view")
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w.Reason, "journal-hot")
	}
	if !found {
		t.Errorf("no journal-hot warning: %+v", res.Warnings)
	}
}

func TestParserIsDeterministic(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "aosp-baseline").Bundle(h, remotePath)
	p := androidmms.Parser{}
	parsertest.AssertDeterministic(t, func() parsertest.Result { return h.RunLenient(p, b.Input(h, p, false)) })
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.LoadFixture(t, "testdata", "aosp-baseline").Bundle(h, remotePath)
	p := androidmms.Parser{}
	h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
}
