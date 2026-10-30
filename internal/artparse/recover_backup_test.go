package artparse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

const (
	recBackupDomain = "HomeDomain"
	recBackupRel    = "Library/Safari/History.db"
	recBackupLogic  = "ios:HomeDomain/Library/Safari/History.db"
)

// iosRecParser is recParser claiming the table people of the sqlitefile freelist fixture, over an iOS
// database named like Safari's History.db.
type iosRecParser struct{ *recParser }

func (p iosRecParser) Meta() parse.Meta {
	m := p.recParser.Meta()
	m.Platforms = []string{parse.PlatformIOS}
	m.Inputs = []parse.InputSpec{
		{Role: "db", Globs: []string{"ios:*/Library/Safari/History.db"}, Required: true},
		{Role: "wal", Companions: []string{"-wal"}},
	}
	return m
}

func regIOSRec(t *testing.T) artparse.Registered {
	t.Helper()
	return regFake(t, iosRecParser{newRecParser("rm", "people")}, "rm")
}

func historyFile(t *testing.T) bfile {
	t.Helper()
	return bfile{domain: recBackupDomain, rel: recBackupRel, data: string(fixtureFile(t, "sqlite3-freelist.db"))}
}

// 4E-R38 finally honored: a typed recovery over a backup acquisition names the backup's files.
func TestRecoverOverABackupAcquisitionNamesBackupFiles(t *testing.T) {
	bc := newBackupCase(t)
	a := bc.add(bspec{files: []bfile{historyFile(t)}})
	h := bc.host(nil, regIOSRec(t))
	sum := recoverOK(t, h, artparse.RecoverOptions{})
	if len(sum.Jobs) != 1 {
		t.Fatalf("%d jobs, want 1: %+v", len(sum.Jobs), sum.Jobs)
	}
	j := sum.Jobs[0]
	if j.Logical != recBackupLogic || j.ArtifactID != a.file(recBackupDomain, recBackupRel).ID || j.Outcome != artparse.OutcomeComplete || j.Records == 0 {
		t.Fatalf("job %+v, want a complete job over %s with records", j, recBackupLogic)
	}
	if len(sum.Backups) != 1 || sum.Backups[0].Status != artparse.BackupMapped {
		t.Errorf("summary backups %+v, want one mapped backup", sum.Backups)
	}
	// the same database as an Android file recovers the same number of rows: the backup path adds no row and loses none
	rc := newRecCase(t, "sqlite3-freelist", false)
	want := recoverOK(t, rc.host(nil, regRec(t, newRecParser("rm", "people"))), artparse.RecoverOptions{}).Jobs[0].Records
	if j.Records != want {
		t.Errorf("backup database recovered %d records, the same file as an Android file %d", j.Records, want)
	}
	start := bc.entries(artparse.ActionAnalysisStart)
	if len(start) != 1 {
		t.Fatalf("%d analysis.start entries", len(start))
	}
	as := decodeEntry[artparse.AnalysisStart](t, start[0])
	if as.Op != artparse.OpParseRecover || as.Backups == nil || len(as.Backups) != 1 || !as.BackupsResolved || as.Backups[0].Status != artparse.BackupMapped {
		t.Errorf("analysis.start op %q backups %+v resolved %v, want an array of one mapped backup and backups_resolved true", as.Op, as.Backups, as.BackupsResolved)
	}
	if raw, ok := start[0].Details["backups"].([]any); !ok || len(raw) != 1 || start[0].Details["backups_resolved"] != true {
		t.Errorf("raw analysis.start backups = %#v resolved %#v", start[0].Details["backups"], start[0].Details["backups_resolved"])
	}
}

func TestRecoverTypedResolvesEveryBackupOfTheCase(t *testing.T) {
	bc := newBackupCase(t)
	a := bc.add(bspec{files: []bfile{historyFile(t)}})
	b := bc.add(bspec{dev: "UDID2", acq: "acq2", files: []bfile{{domain: recBackupDomain, rel: "Library/Other.db", data: "OTHER"}}, plist: plistYes})
	h := bc.host(nil, regIOSRec(t))
	sum := recoverOK(t, h, artparse.RecoverOptions{OnlyPrimary: a.file(recBackupDomain, recBackupRel).ID})
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Fatalf("jobs %+v, want one complete job over the first backup's file", sum.Jobs)
	}
	if len(sum.Backups) != 2 {
		t.Fatalf("summary lists %d backups, want both: %+v", len(sum.Backups), sum.Backups)
	}
	var enc *artparse.BackupResolution
	for i := range sum.Backups {
		if sum.Backups[i].ManifestDB != nil && sum.Backups[i].ManifestDB.ArtifactID == b.db.ID {
			enc = &sum.Backups[i]
		}
	}
	if enc == nil || enc.Encryption != "encrypted" {
		t.Fatalf("the second backup is not resolved as encrypted: %+v", sum.Backups)
	}
	as := decodeEntry[artparse.AnalysisStart](t, bc.entries(artparse.ActionAnalysisStart)[0])
	if len(as.Backups) != 2 || !as.BackupsResolved {
		t.Errorf("analysis.start backups %+v resolved %v, want both backups and backups_resolved true", as.Backups, as.BackupsResolved)
	}
	warned := false
	for _, e := range bc.entries(evidence.ActionAnalysisWarning) {
		if p, _ := e.Details["path"].(string); strings.HasPrefix(p, "backup:") && strings.Contains(p, "UDID2") {
			if r, _ := e.Details["reason"].(string); strings.Contains(r, "encrypted backup") {
				warned = true
			}
		}
	}
	if !warned {
		t.Error("no analysis.warning names the second backup as encrypted: its resolution warning was lost")
	}
}

func TestRecoverTypedPropagatesAnUnrelatedBackupIntegrityFailure(t *testing.T) {
	setup := func(t *testing.T) (*bcase, *bres, *bres) {
		bc := newBackupCase(t)
		a := bc.add(bspec{acq: "acqA", files: []bfile{{domain: recBackupDomain, rel: "Library/Other.db", data: "OTHER"}}})
		b := bc.add(bspec{dev: "UDID2", acq: "acqB", files: []bfile{historyFile(t)}})
		tamper(t, bc.c, a.db)
		return bc, a, b
	}
	check := func(t *testing.T, sum artparse.RunSummary, a *bres) {
		t.Helper()
		if sum.Class() != artparse.ClassIntegrity {
			t.Errorf("Class = %v, want ClassIntegrity", sum.Class())
		}
		found := false
		for _, r := range sum.Backups {
			found = found || (r.Integrity && r.ManifestDB != nil && r.ManifestDB.ArtifactID == a.db.ID)
		}
		if !found {
			t.Errorf("the tampered Manifest.db %s is in no resolution: %+v", a.db.ID, sum.Backups)
		}
	}
	t.Run("plain", func(t *testing.T) {
		bc, a, b := setup(t)
		sum, err := bc.host(nil, regIOSRec(t)).RecoverTyped(context.Background(), artparse.RecoverOptions{OnlyPrimary: b.file(recBackupDomain, recBackupRel).ID})
		if err != nil {
			t.Fatalf("RecoverTyped: %v", err)
		}
		check(t, sum, a)
	})
	t.Run("cancel after the finding", func(t *testing.T) {
		bc, a, b := setup(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		h := bc.host(func(o *artparse.Options) {
			artparse.WithOnOpen(o, func(id string) {
				if id == b.db.ID {
					cancel()
				}
			})
		}, regIOSRec(t))
		sum, err := h.RecoverTyped(ctx, artparse.RecoverOptions{OnlyPrimary: b.file(recBackupDomain, recBackupRel).ID})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		check(t, sum, a)
	})
}
