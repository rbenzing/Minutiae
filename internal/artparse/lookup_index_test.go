package artparse_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// overlaySource serves the manifest of a real case plus synthetic records (4E-R39): the synthetic
// artifacts exist only in the manifest, so nothing may open them.
type overlaySource struct {
	c     *evidence.Case
	extra []evidence.ManifestRecord
}

func (o overlaySource) Manifest() ([]evidence.ManifestRecord, error) {
	m, err := o.c.Manifest()
	return append(m, o.extra...), err
}
func (o overlaySource) ReadAudit() ([]evidence.AuditEntry, error) { return o.c.ReadAudit() }

func TestLookupFindLiteralUsesTheIndexAndEqualsTheScan(t *testing.T) {
	const n = 20000
	bc := newBackupCase(t)
	rows := make([]frow, 0, n)
	extra := make([]evidence.ManifestRecord, 0, n)
	for i := range n {
		rel := fmt.Sprintf("Library/SMS/f%05d.db", i)
		fid := parsertest.BackupFileID(smsDomain, rel)
		remote := "UDID1/" + fid[:2] + "/" + fid
		rows = append(rows, frow{id: fid, domain: smsDomain, rel: rel, flags: 1})
		extra = append(extra, evidence.ManifestRecord{
			ID: fmt.Sprintf("syn%05d", i), Path: "artifacts/UDID1/acq1/backup/" + remote, Size: 1, SHA256: strings.Repeat("a", 64),
			Source: evidence.Source{Kind: "backup", DeviceID: "UDID1", RemotePath: remote},
		})
	}
	bc.add(bspec{files: stdFiles(), rows: rows})
	opened := map[string]bool{}
	h := bc.host(func(o *artparse.Options) {
		artparse.WithCaseSource(o, overlaySource{c: bc.c, extra: extra})
		artparse.WithOnOpen(o, func(id string) { opened[id] = true })
	})
	snap := snapshotOf(t, h)
	rs, err := h.ResolveBackups(context.Background(), snap)
	if err != nil || len(rs) != 1 || rs[0].Status != "mapped" || rs[0].Mapped != n+3 {
		t.Fatalf("resolutions %+v err %v, want one mapped backup of %d files", rs, err, n+3)
	}
	for id := range opened {
		if strings.HasPrefix(id, "syn") {
			t.Fatalf("resolution opened the synthetic artifact %s", id)
		}
	}
	jobs, _, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs %v err %v", jobs, err)
	}
	b, err := artparse.OpenBundle(context.Background(), h, snap, jobs[0], "p1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	lk := b.Input(false).Lookup

	var globs []string
	for i := range 40 {
		globs = append(globs, fmt.Sprintf("ios:HomeDomain/Library/SMS/f%05d.db", (i*397)%n))
	}
	for i := range 8 {
		globs = append(globs, fmt.Sprintf("ios:HomeDomain/Library/SMS/nope%d.db", i))
	}
	globs = append(globs, "ios:HomeDomain/Library/SMS/sms.db", "ios:MediaDomain/"+attRel)
	if len(globs) != 50 {
		t.Fatalf("%d globs", len(globs))
	}
	before := artparse.NameCalls(h)
	got := make([][]string, len(globs))
	found := make([]any, len(globs))
	for i, g := range globs {
		as := lk.Find(g)
		found[i] = as
		for _, a := range as {
			got[i] = append(got[i], a.ID)
		}
	}
	total, _ := h.Case().Manifest()
	if calls := artparse.NameCalls(h) - before; calls > int64(len(total)+len(extra)) {
		t.Errorf("50 literal lookups named records %d times, want at most one pass over the %d records (the index is built once)", calls, len(total)+len(extra))
	}
	hits := 0
	for i, g := range globs {
		want := artparse.FindScan(b, g)
		if !reflect.DeepEqual(found[i], any(want)) {
			t.Errorf("Find(%q) = %v, the scan finds %v", g, found[i], want)
		}
		if len(want) > 0 {
			hits++
		}
	}
	if hits != 42 {
		t.Errorf("%d globs found an artifact, want 42 (the misses must stay misses)", hits)
	}
	for _, g := range []string{"ios:HomeDomain/Library/SMS/f0000*.db", "ios:*/Library/SMS/sms.db", "ios:HomeDomain/**", "./relative", "ios:", "android:/x"} {
		if f, s := lk.Find(g), artparse.FindScan(b, g); !reflect.DeepEqual(f, s) {
			t.Errorf("Find(%q) = %d artifacts, the scan finds %d", g, len(f), len(s))
		}
	}
	if n := len(lk.Find("ios:HomeDomain/Library/SMS/f0000*.db")); n != 10 {
		t.Errorf("wildcard lookup found %d artifacts, want 10", n)
	}
}
