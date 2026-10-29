package iossms_test

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// bundleOf stores a fixture as the backup of UDID1 holding HomeDomain/Library/SMS/sms.db.
func bundleOf(h *parsertest.Harness, fx parsertest.Fixture) parsertest.Bundle {
	return fx.BundleIOSBackup(h, domain, rel)
}

// Every DDL snapshot of the messages family is the schema of at least one fixture, and every variant parses to
// exactly the records its oracle states with no contract violation and no refused record.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "tools", "fixtures", "schemas", "ios", "sms-*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		h := parsertest.NewHarness(t)
		b := bundleOf(h, fx)
		p := iossms.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) != len(fx.Expect.Records) || len(res.Records) == 0 {
			t.Errorf("%s: err %v, violations %v, %d records, oracle %d", v, res.Err, res.Violations, len(res.Records), len(fx.Expect.Records))
		}
		if len(res.Warnings) != len(fx.Expect.Warnings) {
			t.Errorf("%s: %d warnings, oracle %d: %+v", v, len(res.Warnings), len(fx.Expect.Warnings), res.Warnings)
		}
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "sms-") {
			t.Errorf("fixture names DDL %q", ddl)
		}
		if _, err := os.Stat(filepath.Join(ddlDir, ddl)); err != nil {
			t.Error(err)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v))
		p := iossms.Parser{}
		parsertest.AssertDeterministic(t, func() parsertest.Result {
			res := h.RunLenient(p, b.Input(h, p, false))
			if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
				t.Errorf("%s: err %v, violations %v, %d records", v, res.Err, res.Violations, len(res.Records))
			}
			return res
		})
	}
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "ios17-nanos")
	b := bundleOf(h, fx)
	p := iossms.Parser{}
	res := h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	// a parser that ignored its context would still emit every row of a small database
	if want := len(fx.Expect.Records); len(res.Records) >= want {
		t.Errorf("%d records on an already cancelled context, the database holds %d", len(res.Records), want)
	}
}

// Rows deleted from a table whose pages went to the freelist still sit in the file; the live parse never emits
// them and never flags a record deleted or recovered (recovery is plan 4G). The header must count freelist pages, so
// the test cannot pass for lack of a freelist (whether the builder leaves the old bytes in them is its own
// business; the Android fixture av-freelist-512 holds real residue). Rows that exist only in the -wal of ios17-wal are attributed to
// the -wal artifact, with its own remote path.
func TestFreelistResidueAndWALAttribution(t *testing.T) {
	d := newSMS(t, "message", "handle")
	d.add("handle", 1, map[string]any{"id": "+15550001"})
	long := strings.Repeat("padding ", 40)
	for i := int64(1); i <= 300; i++ {
		d.msg(i, map[string]any{"text": fmt.Sprintf("message %d %s", i, long)})
	}
	before := d.tbl["message"].Leaves()
	for i := int64(100); i <= 250; i++ {
		d.tbl["message"].Delete(i)
	}
	freed := map[uint32]bool{}
	for _, p := range before {
		freed[p] = true
	}
	for _, p := range d.tbl["message"].Leaves() {
		delete(freed, p)
	}
	var pages []uint32
	for p := range freed {
		pages = append(pages, p)
	}
	if len(pages) == 0 {
		t.Fatal("the deletes freed no page: the test would be vacuous")
	}
	d.b.Free(pages...)
	db := d.b.Bytes()
	if binary.BigEndian.Uint32(db[36:40]) == 0 {
		t.Fatal("the database header counts no freelist page: the test would be vacuous")
	}
	res := runBytes(t, db)
	if res.Err != nil {
		t.Fatalf("err %v", res.Err)
	}
	if len(res.Records) != 149 {
		t.Fatalf("%d records, want 149 (300 inserted, rows 100 to 250 deleted)", len(res.Records))
	}
	for _, r := range res.Records {
		if strings.Contains(r.Body, "message 100 ") || strings.Contains(r.Body, "message 200 ") || r.Deleted || r.Recovery != "" {
			t.Errorf("%s: a deleted row came back (body %.20q, deleted %v, recovery %q)", r.Locator, r.Body, r.Deleted, r.Recovery)
		}
	}

	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "ios17-wal")
	b := bundleOf(h, fx)
	wal, ok := b.ByRole["wal"]
	if !ok || wal.ID == "" || wal.Source.RemotePath == b.Primary.Source.RemotePath {
		t.Fatalf("the bundle has no distinct -wal artifact: %+v", wal)
	}
	p := iossms.Parser{}
	wres := h.RunLenient(p, b.Input(h, p, false))
	if wres.Err != nil || len(wres.Violations) != 0 {
		t.Fatalf("err %v, violations %v", wres.Err, wres.Violations)
	}
	onlyInWAL := map[int64]bool{}
	for _, e := range fx.Expect.Records {
		if e.File == "wal" {
			onlyInWAL[e.Rowid] = true
		}
	}
	if len(onlyInWAL) < 3 {
		t.Fatalf("the oracle names %d rows only the WAL holds, want at least 3", len(onlyInWAL))
	}
	seen := 0
	for _, r := range wres.Records {
		var id int64
		if _, err := fmt.Sscanf(r.Locator, "sqlite:table=message;rowid=%d", &id); err != nil {
			t.Fatalf("locator %q: %v", r.Locator, err)
		}
		if !onlyInWAL[id] {
			continue
		}
		seen++
		if r.ArtifactID != wal.ID || r.SourcePath != wal.Source.RemotePath || r.Range == nil || r.Range.Offset+r.Range.Length > wal.Size {
			t.Errorf("%s: artifact %s path %s range %+v, want the -wal artifact %s at %s", r.Locator, r.ArtifactID, r.SourcePath, r.Range, wal.ID, wal.Source.RemotePath)
		}
	}
	if seen != len(onlyInWAL) {
		t.Errorf("%d of the %d WAL-only rows were found", seen, len(onlyInWAL))
	}
}
