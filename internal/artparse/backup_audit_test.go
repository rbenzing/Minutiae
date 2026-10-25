package artparse_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

func TestBackupManifestIntegrityIsExit4AndOtherBackupsContinue(t *testing.T) {
	bc := newBackupCase(t)
	a := bc.add(bspec{acq: "acqA", files: stdFiles()})
	b := bc.add(bspec{acq: "acqB", files: stdFiles()})
	tamper(t, bc.c, a.db)
	h := bc.host(nil)
	p := bc.plan(h)
	if len(p.Backups) != 2 {
		t.Fatalf("Backups = %+v", p.Backups)
	}
	ba, bb := first(t, p.Backups), second(t, p.Backups)
	if ba.Acquisition != a.key || ba.Status != "refused" || !ba.Integrity || !strings.Contains(ba.Reason, a.db.ID) || ba.Mapped != 0 {
		t.Errorf("A = %+v, want refused with integrity naming %s", ba, a.db.ID)
	}
	if bb.Acquisition != b.key || bb.Status != "mapped" || bb.Integrity {
		t.Errorf("B = %+v, want mapped", bb)
	}
	if len(p.Rows) != 1 || p.Rows[0].Status != artparse.StatusNew || p.Rows[0].Job.Primary.Artifact.ID != b.file(smsDomain, smsRel).ID {
		t.Errorf("rows = %+v, want only B's job, new", p.Rows)
	}
	sum := bc.run(h)
	if sum.Class() != artparse.ClassIntegrity {
		t.Errorf("Class = %v, want ClassIntegrity", sum.Class())
	}
	if len(sum.Jobs) != 1 || sum.Jobs[0].Outcome != artparse.OutcomeComplete {
		t.Errorf("jobs = %+v, want B's job complete", sum.Jobs)
	}
}

func TestManifestPlistTamperedIsRefused(t *testing.T) {
	bc := newBackupCase(t)
	a := bc.add(bspec{files: stdFiles()})
	tamper(t, bc.c, a.plist)
	p := bc.plan(bc.host(nil))
	b := first(t, p.Backups)
	if b.Status != "refused" || !b.Integrity || !strings.Contains(b.Reason, a.plist.ID) || len(p.Rows) != 0 {
		t.Errorf("Backups[0] = %+v, rows %d", b, len(p.Rows))
	}
}

func TestIncompleteManifestDBIsNotUsed(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(bspec{files: stdFiles(), partialDB: true})
	if !r.db.Incomplete {
		t.Fatal("the test Manifest.db is not flagged incomplete")
	}
	named, unnamed := bc.names()
	p := bc.plan(bc.host(nil))
	b := first(t, p.Backups)
	if b.Status != "unparsed" || !strings.Contains(b.Reason, "Manifest.db is incomplete") || b.Mapped != 0 || len(p.Rows) != 0 {
		t.Errorf("Backups[0] = %+v, rows %d", b, len(p.Rows))
	}
	id := r.file(smsDomain, smsRel).ID
	if _, ok := named[id]; ok || !strings.Contains(unnamed[id], "Manifest.db is incomplete") {
		t.Errorf("sms.db named %q reason %q", named[id], unnamed[id])
	}
}

func TestManifestRowCapRefusesTheMapping(t *testing.T) {
	bc := newBackupCase(t)
	bc.add(stdSpec()) // three Files rows
	p := bc.plan(bc.host(func(o *artparse.Options) { artparse.SetMaxFilesRows(o, 2) }))
	b := first(t, p.Backups)
	if b.Status != "unparsed" || b.Mapped != 0 || !strings.Contains(b.Reason, "more Files rows than 2") || len(p.Rows) != 0 {
		t.Errorf("Backups[0] = %+v, rows %d", b, len(p.Rows))
	}
	// exactly at the cap it maps
	p = bc.plan(bc.host(func(o *artparse.Options) { artparse.SetMaxFilesRows(o, 3) }))
	if first(t, p.Backups).Status != "mapped" || first(t, p.Backups).Mapped != 3 {
		t.Errorf("at the cap: Backups[0] = %+v", first(t, p.Backups))
	}
}

func TestFileIDHashMismatchIsCountedNotUnmapped(t *testing.T) {
	bc := newBackupCase(t)
	odd := bfile{domain: smsDomain, rel: "Library/Other/odd.db", data: "x", remote: "UDID1/ab/ab00000000000000000000000000000000000000"}
	r := bc.add(bspec{files: append(stdFiles(), odd)})
	named, _ := bc.names()
	if got := named[r.file(smsDomain, odd.rel).ID]; got != "ios:HomeDomain/Library/Other/odd.db" {
		t.Errorf("the mismatching file was named %q, want it named from the Files row", got)
	}
	b := first(t, bc.plan(bc.host(nil)).Backups)
	if b.FileIDMismatch != 1 || b.Mapped != 4 || b.Unmapped != 0 {
		t.Errorf("Backups[0] = %+v, want FileIDMismatch 1 and Mapped 4", b)
	}
}

func TestParseAuditRecordsManifestDbArtifact(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(stdSpec())
	bc.run(bc.host(nil))
	as := decodeEntry[artparse.AnalysisStart](t, entry0(t, bc.entries(artparse.ActionAnalysisStart)))
	if len(as.Backups) != 1 {
		t.Fatalf("backups = %+v", as.Backups)
	}
	b := first2(t, as.Backups)
	wantDB := &artparse.AuditArtifactRef{ArtifactID: r.db.ID, SHA256: r.db.SHA256}
	wantPlist := &artparse.AuditArtifactRef{ArtifactID: r.plist.ID, SHA256: r.plist.SHA256}
	if !reflect.DeepEqual(b.ManifestDB, wantDB) || !reflect.DeepEqual(b.ManifestPlist, wantPlist) || b.Status != "mapped" || b.Encryption != "not-encrypted" ||
		b.Mapped != 3 || b.Acquisition != r.key || b.Dir != "UDID1" {
		t.Errorf("analysis.start backup = %+v", b)
	}
	js := decodeEntry[artparse.JobStart](t, entry0(t, bc.entries(artparse.ActionParseJobStart)))
	for _, role := range []string{"db", "wal"} {
		m := js.Bundle[role]
		if m.Namer != "ios-backup" || !reflect.DeepEqual(m.NamerSource, wantDB) {
			t.Errorf("bundle.%s = %+v, want namer ios-backup and namer_source %+v", role, m, wantDB)
		}
	}
	// a case without backups still writes the field, as an empty array
	c2 := newCase(t)
	acquireStart(t, c2, "D1", "A1", "logical")
	putFile(t, c2, "D1", "A1", "/data/data/x/databases/app.db", "x")
	h2 := newHost(t, c2, register(t, "p", "1.0.0"))
	if _, err := h2.Run(context.Background(), artparse.RunOptions{}); err != nil {
		t.Fatal(err)
	}
	all, _ := c2.ReadAudit()
	for _, e := range all {
		if e.Action == artparse.ActionAnalysisStart {
			if bs, ok := e.Details["backups"].([]any); !ok || len(bs) != 0 {
				t.Errorf("backups = %#v, want an empty array", e.Details["backups"])
			}
		}
	}
}

func TestClaimedTablesResolveBackupLogicalPaths(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(stdSpec())
	reg := regFake(t, smsMapper{}, "ios-sms-claim")
	h := bc.host(nil, reg)
	got, err := h.ClaimedTables(context.Background(), r.file(smsDomain, smsRel).ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Table != "message" || got[0].Role != "db" {
		t.Fatalf("claims = %+v, want one claim of table message", got)
	}
	tamper(t, bc.c, r.db)
	_, err = h.ClaimedTables(context.Background(), r.file(smsDomain, smsRel).ID)
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Errorf("with a tampered Manifest.db err = %v, want evidence.ErrIntegrity", err)
	}
}

func TestParsePlanAppendsNothingWithBackups(t *testing.T) {
	bc := newBackupCase(t)
	bc.add(stdSpec())
	bc.add(bspec{acq: "acq2", plist: plistYes, manifest: randomBytes(4096), files: stdFiles()})
	h := bc.host(nil)
	auditBefore, _ := bc.c.ReadAudit()
	manBefore, _ := bc.c.Manifest()
	bc.plan(h)
	auditAfter, _ := bc.c.ReadAudit()
	manAfter, _ := bc.c.Manifest()
	if !reflect.DeepEqual(auditBefore, auditAfter) || !reflect.DeepEqual(manBefore, manAfter) {
		t.Error("Plan changed the audit log or the manifest")
	}
}

func TestStreamedManifestDBIsRehashedAfterTheRead(t *testing.T) {
	stream := func(o *artparse.Options) { o.Limits.MemInputMax = 0 } // every input is streamed
	t.Run("untouched maps", func(t *testing.T) {
		bc := newBackupCase(t)
		bc.add(stdSpec())
		p := bc.plan(bc.host(stream))
		if first(t, p.Backups).Status != "mapped" || first(t, p.Backups).Mapped != 3 {
			t.Errorf("Backups[0] = %+v", first(t, p.Backups))
		}
	})
	t.Run("changed during the read is refused", func(t *testing.T) {
		bc := newBackupCase(t)
		r := bc.add(stdSpec())
		done := false
		p := bc.plan(bc.host(func(o *artparse.Options) {
			stream(o)
			artparse.WithAfterManifestRead(o, func() {
				if !done {
					done = true
					tamper(t, bc.c, r.db)
				}
			})
		}))
		b := first(t, p.Backups)
		if b.Status != "refused" || !b.Integrity || b.Mapped != 0 || !strings.Contains(b.Reason, r.db.ID) || len(p.Rows) != 0 {
			t.Errorf("Backups[0] = %+v, rows %d, want a refused finding naming %s and nothing mapped", b, len(p.Rows), r.db.ID)
		}
	})
}

func TestBackupResolutionReleasesItsBudgetOnEveryPath(t *testing.T) {
	var bud *parse.Budget
	spy := func(o *artparse.Options) {
		artparse.WithResolveBudget(o, func(limit int64) *parse.Budget { bud = parse.NewBudget(limit); return bud })
	}
	assertFree := func(t *testing.T, path string) {
		t.Helper()
		if bud == nil {
			t.Fatalf("%s: the resolution made no budget", path)
		}
		if used := bud.Used(); used != 0 {
			t.Errorf("%s: %d bytes of the budget are still held after the resolution", path, used)
		}
	}
	t.Run("mapped", func(t *testing.T) {
		bc := newBackupCase(t)
		bc.add(stdSpec())
		bc.plan(bc.host(spy))
		assertFree(t, "mapped")
	})
	t.Run("refused after a successful read", func(t *testing.T) {
		bc := newBackupCase(t)
		r := bc.add(stdSpec())
		bc.plan(bc.host(func(o *artparse.Options) {
			spy(o)
			o.Limits.MemInputMax = 0
			artparse.WithAfterManifestRead(o, func() { tamper(t, bc.c, r.db) })
		}))
		assertFree(t, "refused")
	})
	t.Run("cancelled after a successful read", func(t *testing.T) {
		bc := newBackupCase(t)
		bc.add(stdSpec())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h := bc.host(func(o *artparse.Options) {
			spy(o)
			artparse.WithAfterManifestRead(o, cancel)
		})
		snap := snapshotOf(t, h)
		if _, err := h.ResolveBackups(ctx, snap); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		assertFree(t, "cancelled")
	})
}

func TestResolveBackupsIsIdempotentPerSnapshot(t *testing.T) {
	bc := newBackupCase(t)
	bc.add(stdSpec())
	opens := 0
	h := bc.host(func(o *artparse.Options) { artparse.WithOnOpen(o, func(string) { opens++ }) })
	snap := snapshotOf(t, h)
	first, err := h.ResolveBackups(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	n := opens
	second, err := h.ResolveBackups(context.Background(), snap)
	if err != nil || !reflect.DeepEqual(first, second) || opens != n {
		t.Errorf("second resolution %+v err %v opens %d->%d, want the first answer without opening anything", second, err, n, opens)
	}
}
