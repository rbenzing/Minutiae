package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// fatChainImage is a FAT16 image holding the chain database and its WAL, both deleted the way the vfat
// driver deletes them (0xE5, chain freed, long names kept).
func fatChainImage(t *testing.T) []byte {
	t.Helper()
	db, wal := chainFiles()
	return fattest.Build(fattest.Options{Type: 16}, []fattest.File{
		{Path: "DATA", Dir: true},
		{Path: "DATA/app.db", Data: db, Deleted: true, LongName: true},
		{Path: "DATA/app.db-wal", Data: wal, Deleted: true, LongName: true},
	})
}

// The end-to-end chain of spec 15.3 on a FAT16 image (3K-R4): a deleted database and its deleted WAL are
// recovered by the real driver (3B), the rows are recovered from them (3J), then case verify.
func TestSQLiteRecoverChainFromFATImage(t *testing.T) {
	db, wal := chainFiles()
	c := newCLICase(t)
	ref := importRaw(t, c, fatChainImage(t))
	code, out := run(t, Deps{}, "image", "recover", "--case", c, ref, "--all", "--json")
	if code != 0 {
		t.Fatalf("image recover --all: %d\n%s", code, out)
	}
	// (1) exactly the two deleted files, byte for byte
	recs := recoveredRecords(t, c)
	if len(recs) != 2 {
		t.Fatalf("%d recover artifacts, want 2", len(recs))
	}
	hexOf := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	var dbRec, walRec evidence.ManifestRecord
	for _, r := range recs {
		d := r.Source.Derived
		if d.Recovery == nil || d.Recovery.Class != "deleted-file" || d.Recovery.Method != "fat-contiguous" {
			t.Fatalf("artifact %s: recovery %+v, want deleted-file by fat-contiguous", r.ID, d.Recovery)
		}
		switch d.FSPath {
		case "/DATA/app.db":
			dbRec = r
			if r.SHA256 != hexOf(db) {
				t.Errorf("database bytes differ: %s", r.SHA256)
			}
		case "/DATA/app.db-wal":
			walRec = r
			if r.SHA256 != hexOf(wal) {
				t.Errorf("WAL bytes differ: %s", r.SHA256)
			}
		default:
			t.Errorf("unexpected recovered path %q", d.FSPath)
		}
	}
	if dbRec.ID == "" || walRec.ID == "" {
		t.Fatalf("the database and the WAL are not both recovered: %v", recs)
	}
	conf := func(r evidence.ManifestRecord) int {
		if r.Source.Derived.Recovery.Confidence == nil {
			t.Fatalf("artifact %s has no recovery confidence", r.ID)
		}
		return *r.Source.Derived.Recovery.Confidence
	}
	floor := min(conf(dbRec), conf(walRec))
	// (2) sqlite recover finds the recovered WAL by parent, partition and path
	code, out = run(t, Deps{}, "sqlite", "recover", "--case", c, "--json", dbRec.ID)
	if code != 0 {
		t.Fatalf("sqlite recover: %d\n%s", code, out)
	}
	var rep sqliteReportJSON
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	roles := map[string]string{}
	for _, in := range rep.Inputs {
		roles[in.Role] = in.ArtifactID
		want := conf(dbRec)
		if in.Role == "wal" {
			want = conf(walRec)
		}
		if !in.Recovered || in.ConfidenceCap == nil || *in.ConfidenceCap > want {
			t.Errorf("input %s: recovered %v cap %v, want recovered and a cap <= %d", in.Role, in.Recovered, in.ConfidenceCap, want)
		}
	}
	if roles["db"] != dbRec.ID || roles["wal"] != walRec.ID {
		t.Fatalf("inputs %v, want the recovered database %s and WAL %s", roles, dbRec.ID, walRec.ID)
	}
	// (3) every row is recovered history under the floor; some come from the WAL
	rows := chainRows(t, c)
	if len(rows) == 0 {
		t.Fatal("no db_row records stored")
	}
	fromWAL := 0
	for _, r := range rows {
		if r.ArtifactID == walRec.ID {
			fromWAL++
		}
		if !r.Recovered || !r.Deleted || r.Confidence == nil || *r.Confidence > floor {
			t.Errorf("row %d: recovered %v deleted %v confidence %v, want recovered, deleted and <= %d", r.ID, r.Recovered, r.Deleted, r.Confidence, floor)
		}
	}
	if fromWAL == 0 {
		t.Error("no row came from the recovered WAL")
	}
	// (4) a recovered marker is found and never shown as live
	code, out = run(t, Deps{}, "records", "search", "--case", c, "--json", "--type", "db_row", chainMarker)
	if code != 0 {
		t.Fatalf("records search: %d\n%s", code, out)
	}
	var hits struct {
		Records []hitJSON `json:"records"`
	}
	if err := json.Unmarshal(jsonPart(out), &hits); err != nil || len(hits.Records) == 0 {
		t.Fatalf("search found nothing for %q: %v\n%s", chainMarker, err, out)
	}
	for _, h := range hits.Records {
		if !h.Recovered || !h.Deleted {
			t.Errorf("hit %d is shown as live", h.ID)
		}
	}
	// (5) case verify is clean and counts both
	code, out = run(t, Deps{}, "case", "verify", "--case", c)
	if code != 0 || strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "2 recovered (2 reproduced)") {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
	if ok, _ := lastVerifyRun(t, c)["ok"].(bool); !ok {
		t.Errorf("verify.run ok %v, want true", lastVerifyRun(t, c)["ok"])
	}
}
