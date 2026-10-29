package artparse_test

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	smsDomain = "HomeDomain"
	smsRel    = "Library/SMS/sms.db"
	attDomain = "MediaDomain"
	attRel    = "Library/SMS/Attachments/aa/x.jpg"

	filesDDL     = "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)"
	filesDDLNoPK = "CREATE TABLE Files (fileID TEXT, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)"

	plistHead = `<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict>`
	plistTail = `</dict></plist>`
	plistNo   = plistHead + `<key>IsEncrypted</key><false/>` + plistTail
	plistYes  = plistHead + `<key>IsEncrypted</key><true/>` + plistTail

	encryptedReason = "encrypted backup (sub-project 10)"
)

// frow is one row of the Files table of a test Manifest.db; id is any so a test can store a BLOB.
type frow struct {
	id          any
	domain, rel string
	flags       int64
}

// bfile is one file of a test backup. asDomain and asRel change what the Files row says; flags 0 means 1.
type bfile struct {
	domain, rel string
	data        string
	remote      string // the remote path ("" = <dir>/<aa>/<sha1(domain-rel)>)
	flags       int64
	unlisted    bool // no Files row
	dup         bool // a second row with the same fileID
	asDomain    string
	asRel       string
}

// bspec is one backup of a test case.
type bspec struct {
	dev, acq, dir string // defaults UDID1, acq1, <dev>
	files         []bfile
	rows          []frow // more Files rows
	noPK          bool
	plist         string // "" = not encrypted
	noPlist       bool
	manifest      []byte // replaces the generated Manifest.db
	noManifest    bool
	partialDB     bool // Manifest.db captured aborted (incomplete)
}

// bres is what a bspec put in the case.
type bres struct {
	key               string // acquisitionKey
	dir               string
	db, plist, status evidence.ManifestRecord
	files             map[string]evidence.ManifestRecord // "domain/rel"
	fileIDOf          map[string]string
}

func (r *bres) file(domain, rel string) evidence.ManifestRecord { return r.files[domain+"/"+rel] }

type bcase struct {
	t       *testing.T
	c       *evidence.Case
	started map[string]bool
}

func newBackupCase(t *testing.T) *bcase {
	t.Helper()
	return &bcase{t: t, c: newCase(t), started: map[string]bool{}}
}

func stdFiles() []bfile {
	return []bfile{
		{domain: smsDomain, rel: smsRel, data: "SMSDB"},
		{domain: smsDomain, rel: smsRel + "-wal", data: "WAL"},
		{domain: attDomain, rel: attRel, data: "JPG"},
	}
}

func stdSpec() bspec { return bspec{files: stdFiles()} }

func (bc *bcase) put(dev, acq, remote, data string) evidence.ManifestRecord {
	bc.t.Helper()
	rec, err := bc.c.Capture(dev, acq, "backup/"+remote, evidence.Source{Kind: "backup", DeviceID: dev, RemotePath: remote}, func(w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	})
	if err != nil {
		bc.t.Fatal(err)
	}
	return rec
}

func manifestBytes(noPK bool, rows []frow) []byte {
	b := sqlitetest.New(sqlitetest.Options{})
	ddl := filesDDL
	if noPK {
		ddl = filesDDLNoPK
	}
	tb := b.CreateTable("Files", ddl)
	for i, r := range rows {
		tb.Insert(int64(i+1), r.id, r.domain, r.rel, r.flags, nil)
	}
	return b.Bytes()
}

// add stores one backup: its files, Manifest.db, Manifest.plist and Status.plist.
func (bc *bcase) add(s bspec) *bres {
	bc.t.Helper()
	if s.dev == "" {
		s.dev = "UDID1"
	}
	if s.acq == "" {
		s.acq = "acq1"
	}
	if s.dir == "" {
		s.dir = s.dev
	}
	if k := s.dev + "/" + s.acq; !bc.started[k] {
		bc.started[k] = true
		acquireStart(bc.t, bc.c, s.dev, s.acq, "ios.backup")
	}
	res := &bres{dir: s.dir, files: map[string]evidence.ManifestRecord{}, fileIDOf: map[string]string{}}
	var rows []frow
	for _, f := range s.files {
		fid := parsertest.BackupFileID(f.domain, f.rel)
		remote := f.remote
		if remote == "" {
			remote = s.dir + "/" + fid[:2] + "/" + fid
		} else {
			fid = path.Base(remote)
		}
		rec := bc.put(s.dev, s.acq, remote, f.data)
		res.files[f.domain+"/"+f.rel] = rec
		res.fileIDOf[f.domain+"/"+f.rel] = fid
		if f.unlisted {
			continue
		}
		domain, rel, flags := f.domain, f.rel, f.flags
		if f.asDomain != "" {
			domain = f.asDomain
		}
		if f.asRel != "" {
			rel = f.asRel
		}
		if flags == 0 {
			flags = 1
		}
		rows = append(rows, frow{id: fid, domain: domain, rel: rel, flags: flags})
		if f.dup {
			rows = append(rows, frow{id: fid, domain: domain, rel: rel + ".dup", flags: flags})
		}
	}
	rows = append(rows, s.rows...)
	if !s.noManifest {
		data := s.manifest
		if data == nil {
			data = manifestBytes(s.noPK, rows)
		}
		if s.partialDB {
			w, err := bc.c.NewArtifact(s.dev, s.acq, "backup/"+s.dir+"/Manifest.db", evidence.Source{Kind: "backup", DeviceID: s.dev, RemotePath: s.dir + "/Manifest.db"})
			if err != nil {
				bc.t.Fatal(err)
			}
			if _, err := w.Write(data); err != nil {
				bc.t.Fatal(err)
			}
			if res.db, err = w.Abort(errors.New("cut short")); err != nil {
				bc.t.Fatal(err)
			}
		} else {
			res.db = bc.put(s.dev, s.acq, s.dir+"/Manifest.db", string(data))
		}
	}
	if !s.noPlist {
		pl := s.plist
		if pl == "" {
			pl = plistNo
		}
		res.plist = bc.put(s.dev, s.acq, s.dir+"/Manifest.plist", pl)
	}
	res.status = bc.put(s.dev, s.acq, s.dir+"/Status.plist", "status")
	for _, m := range []evidence.ManifestRecord{res.db, res.plist, res.status} {
		if m.Path != "" {
			res.key = mustKey(bc.t, m.Path)
			break
		}
	}
	return res
}

// smsParser is a fake iOS database parser: primary sms.db (or the address book), a -wal companion and an
// images database beside the address book. It emits one record.
type smsParser struct{}

func smsMeta() parse.Meta {
	return parse.Meta{
		Name: "ios-sms-fake", Version: "1.0.0", Title: "ios sms fake",
		Platforms: []string{parse.PlatformIOS},
		Emits:     metaFor("x", "1.0.0").Emits,
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: []string{"ios:*/Library/SMS/sms.db", "ios:*/Library/AddressBook/AddressBook.sqlitedb"}, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
			{Role: "images", Globs: []string{"./AddressBookImages.sqlitedb"}},
		},
	}
}

func (smsParser) Meta() parse.Meta { return smsMeta() }
func (smsParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: parse.Applicable}, nil
}

func (smsParser) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	return out.Emit(ctx, rxRecord(in, 1))
}

// smsMapper is smsParser with a table claim.
type smsMapper struct{ smsParser }

func (smsMapper) Meta() parse.Meta {
	m := smsMeta()
	m.Name = "ios-sms-claim"
	m.Claims = []parse.TableClaim{{Role: "db", Table: "message"}}
	return m
}
func (smsMapper) Mappings() []parse.TableMapping { return []parse.TableMapping{{Table: "message"}} }

// anyIOS is a fake parser whose primary glob matches every iOS logical path: a plan over it lists every
// named iOS artifact.
type anyIOS struct{ smsParser }

func (anyIOS) Meta() parse.Meta {
	m := smsMeta()
	m.Name = "ios-any-fake"
	m.Inputs = []parse.InputSpec{{Role: "db", Globs: []string{"ios:**"}, Required: true}}
	return m
}

func regSMS(t testing.TB) artparse.Registered {
	t.Helper()
	return regFake(t, smsParser{}, "ios-sms-fake")
}

func (bc *bcase) host(mod func(*artparse.Options), ps ...artparse.Registered) *artparse.Host {
	bc.t.Helper()
	if len(ps) == 0 {
		ps = []artparse.Registered{regSMS(bc.t)}
	}
	return hostWith(bc.t, bc.c, func(o *artparse.Options) {
		artparse.SkipLimitMinimums(o)
		if mod != nil {
			mod(o)
		}
	}, ps...)
}

func (bc *bcase) plan(h *artparse.Host) artparse.Plan {
	bc.t.Helper()
	p, err := h.Plan(context.Background(), artparse.Selection{})
	if err != nil {
		bc.t.Fatalf("Plan: %v", err)
	}
	return p
}

func (bc *bcase) run(h *artparse.Host) artparse.RunSummary {
	bc.t.Helper()
	sum, err := h.Run(context.Background(), artparse.RunOptions{})
	if err != nil {
		bc.t.Fatalf("Run: %v", err)
	}
	return sum
}

func (bc *bcase) entries(actions ...string) []evidence.AuditEntry {
	bc.t.Helper()
	all, err := bc.c.ReadAudit()
	if err != nil {
		bc.t.Fatal(err)
	}
	var out []evidence.AuditEntry
	for _, e := range all {
		if slices.Contains(actions, e.Action) {
			out = append(out, e)
		}
	}
	return out
}

func (bc *bcase) actions() []string {
	bc.t.Helper()
	all, err := bc.c.ReadAudit()
	if err != nil {
		bc.t.Fatal(err)
	}
	var out []string
	for _, e := range all {
		out = append(out, e.Action)
	}
	return out
}

// names plans every named iOS artifact and returns the logical path of each named artifact id and the
// reason of each unnamed one.
func (bc *bcase) names() (named, unnamed map[string]string) {
	bc.t.Helper()
	h := bc.host(nil, regFake(bc.t, anyIOS{}, "ios-any-fake"))
	p := bc.plan(h)
	named, unnamed = map[string]string{}, map[string]string{}
	for _, r := range p.Rows {
		named[r.Job.Primary.Artifact.ID] = r.Job.Primary.Logical
	}
	for _, u := range p.Counts.UnnamedList {
		unnamed[u.ArtifactID] = u.Reason
	}
	return named, unnamed
}

func randomBytes(n int) []byte {
	r := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // deterministic test bytes
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

func ref(m evidence.ManifestRecord) *artparse.ArtifactRef {
	return &artparse.ArtifactRef{ArtifactID: m.ID, SHA256: m.SHA256}
}

func TestIOSBackupResolverMapsFilesViaManifestDB(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(stdSpec())
	p := bc.plan(bc.host(nil))
	if len(p.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (the sms.db job)", len(p.Rows))
	}
	j := p.Rows[0].Job
	if j.Primary.Logical != "ios:HomeDomain/Library/SMS/sms.db" || j.Primary.Namer != "ios-backup" || j.Primary.Platform != parse.PlatformIOS {
		t.Errorf("primary = %q namer %q platform %q", j.Primary.Logical, j.Primary.Namer, j.Primary.Platform)
	}
	if got := j.Primary.NamerSource; got == nil || *got != *ref(r.db) {
		t.Errorf("NamerSource = %+v, want %+v", got, ref(r.db))
	}
	if w, ok := j.Others["wal"]; !ok || w.Artifact.ID != r.file(smsDomain, smsRel+"-wal").ID || w.NamerSource == nil {
		t.Errorf("wal member = %+v (ok %v)", w, ok)
	}
	if len(p.Backups) != 1 {
		t.Fatalf("Backups = %+v", p.Backups)
	}
	b := first(t, p.Backups)
	if b.Status != "mapped" || b.Encryption != "not-encrypted" || b.Mapped != 3 || b.Unmapped != 0 || b.FileIDMismatch != 0 || b.Integrity ||
		b.Acquisition != r.key || b.Dir != "UDID1" || b.ManifestDB == nil || *b.ManifestDB != *ref(r.db) || b.ManifestPlist == nil || *b.ManifestPlist != *ref(r.plist) {
		t.Errorf("Backups[0] = %+v", b)
	}
	meta := map[string]bool{r.db.ID: true, r.plist.ID: true, r.status.ID: true}
	if len(p.Counts.UnnamedList) != 3 {
		t.Errorf("UnnamedList = %+v, want the three metadata files", p.Counts.UnnamedList)
	}
	for _, u := range p.Counts.UnnamedList {
		if !meta[u.ArtifactID] || u.Reason != "backup metadata file" {
			t.Errorf("unnamed %+v, want a metadata file with reason %q", u, "backup metadata file")
		}
	}
}

func TestBackupArtifactsWithoutAUsableRowAreUnnamedWithTheirReason(t *testing.T) {
	blobRow := frow{id: []byte{1, 2, 3}, domain: "x", rel: "y", flags: 1}
	cases := []struct {
		name string
		spec bspec
		want string
	}{
		{"not listed", bspec{files: []bfile{{domain: "D", rel: "a.db", data: "x", unlisted: true}}}, "file ID not listed in Manifest.db"},
		{"listed twice", bspec{noPK: true, files: []bfile{{domain: "D", rel: "a.db", data: "x", dup: true}}}, "listed more than once"},
		{"directory flags", bspec{files: []bfile{{domain: "D", rel: "a.db", data: "x", flags: 2}}}, "not a regular file (flags 2)"},
		{"Snapshot element", bspec{files: []bfile{{domain: "D", rel: "a.db", data: "x", remote: "UDID1/Snapshot/ab/ab11111111111111111111111111111111111111"}}}, "not a backup file path"},
		{"wrong aa", bspec{files: []bfile{{domain: "D", rel: "a.db", data: "x", remote: "UDID1/00/ab11111111111111111111111111111111111111"}}}, "not a backup file path"},
		{"unreadable row", bspec{rows: []frow{blobRow}, files: []bfile{{domain: "D", rel: "a.db", data: "x", unlisted: true}}}, "undecided"},
		{"bad domain", bspec{files: []bfile{{domain: "Home/Domain", rel: "a.db", data: "x"}}}, "unusable domain"},
		{"double slash", bspec{files: []bfile{{domain: "D", rel: "Library//SMS/sms.db", data: "x"}}}, "relativePath is not canonical"},
		{"leading slash", bspec{files: []bfile{{domain: "D", rel: "/a", data: "x"}}}, "relativePath is not canonical"},
		{"trailing slash", bspec{files: []bfile{{domain: "D", rel: "a/", data: "x"}}}, "relativePath is not canonical"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := newBackupCase(t)
			tc.spec.files = append(tc.spec.files, bfile{domain: smsDomain, rel: smsRel, data: "ok"}) // one mapped file keeps the backup mapped
			r := bc.add(tc.spec)
			named, unnamed := bc.names()
			target := r.files[tc.spec.files[0].domain+"/"+tc.spec.files[0].rel]
			if _, ok := named[target.ID]; ok {
				t.Fatalf("the file was named %q, want unnamed", named[target.ID])
			}
			if got := unnamed[target.ID]; !strings.Contains(got, tc.want) {
				t.Errorf("reason = %q, want it to contain %q", got, tc.want)
			}
			if named[r.file(smsDomain, smsRel).ID] != "ios:HomeDomain/Library/SMS/sms.db" {
				t.Errorf("the listed sms.db was not named: %v", named)
			}
		})
	}
}

func TestEncryptedBackupIsUnparsedNotEmpty(t *testing.T) {
	t.Run("high entropy Manifest.db", func(t *testing.T) {
		bc := newBackupCase(t)
		r := bc.add(bspec{files: stdFiles(), plist: plistYes, manifest: randomBytes(4096)})
		h := bc.host(nil)
		p := bc.plan(h)
		if len(p.Rows) != 0 {
			t.Errorf("rows = %d, want none", len(p.Rows))
		}
		if len(p.Backups) != 1 || first(t, p.Backups).Status != "unparsed" || first(t, p.Backups).Encryption != "encrypted" || !strings.Contains(first(t, p.Backups).Reason, encryptedReason) {
			t.Fatalf("Backups = %+v", p.Backups)
		}
		sum := bc.run(h)
		if sum.Class() != artparse.ClassPartial {
			t.Errorf("Class = %v, want ClassPartial", sum.Class())
		}
		acts := bc.actions()
		start := slices.Index(acts, artparse.ActionAnalysisStart)
		warn := slices.Index(acts, evidence.ActionAnalysisWarning)
		if start < 0 || warn < start || slices.Contains(acts, artparse.ActionParseJobStart) {
			t.Errorf("audit actions %v: want analysis.start, then the warning, no job", acts)
		}
		as := decodeEntry[artparse.AnalysisStart](t, entry0(t, bc.entries(artparse.ActionAnalysisStart)))
		if len(as.Backups) != 1 || as.Backups[0].Encryption != "encrypted" || as.Backups[0].Status != "unparsed" || as.Backups[0].Dir != "UDID1" {
			t.Errorf("analysis.start backups = %+v", as.Backups)
		}
		ws := bc.entries(evidence.ActionAnalysisWarning)
		if len(ws) != 1 {
			t.Fatalf("%d warnings, want 1", len(ws))
		}
		w := decodeEntry[artparse.HostWarning](t, ws[0])
		if w.Path != "backup:"+r.key+"/UDID1" || !strings.Contains(w.Reason, encryptedReason) || w.Source != "host" {
			t.Errorf("warning = %+v", w)
		}
	})
	t.Run("readable Manifest.db", func(t *testing.T) {
		bc := newBackupCase(t)
		bc.add(bspec{files: stdFiles(), plist: plistYes})
		h := bc.host(nil)
		p := bc.plan(h)
		if len(p.Rows) != 1 || p.Rows[0].Status != artparse.StatusUnparsed || !strings.Contains(p.Rows[0].Reason, encryptedReason) {
			t.Fatalf("rows = %+v", p.Rows)
		}
		if first(t, p.Backups).Status != "mapped" || first(t, p.Backups).Encryption != "encrypted" {
			t.Errorf("Backups = %+v", p.Backups)
		}
		sum := bc.run(h)
		if sum.Class() != artparse.ClassPartial {
			t.Errorf("Class = %v, want ClassPartial", sum.Class())
		}
		if n := len(bc.entries(evidence.ActionIngestStart)); n != 0 {
			t.Errorf("%d records.ingest.start entries, want none", n)
		}
	})
}

func TestBackupEncryptionDecisionTable(t *testing.T) {
	text := []byte("this is plain text, not a database at all, and not high entropy either")
	cases := []struct {
		name     string
		plist    string
		noPlist  bool
		manifest []byte // nil = a SQLite Manifest.db
		status   string
		enc      string
		reason   string
	}{
		{"yes, sqlite", plistYes, false, nil, "mapped", "encrypted", ""},
		{"yes, high entropy", plistYes, false, randomBytes(4096), "unparsed", "encrypted", encryptedReason},
		{"yes, text", plistYes, false, text, "unparsed", "encrypted", encryptedReason},
		{"no, sqlite", plistNo, false, nil, "mapped", "not-encrypted", ""},
		{"no, high entropy", plistNo, false, randomBytes(4096), "unparsed", "undecided", "Manifest.plist says not encrypted but Manifest.db looks encrypted"},
		{"no, text", plistNo, false, text, "unparsed", "not-encrypted", "Manifest.db is not a database"},
		{"absent, sqlite", "", true, nil, "mapped", "unknown", "no Manifest.plist"},
		{"absent, high entropy", "", true, randomBytes(4096), "unparsed", "encrypted", encryptedReason},
		{"absent, text", "", true, text, "unparsed", "unknown", "Manifest.db is not a database"},
		{"garbage, sqlite", "garbage", false, nil, "mapped", "unknown", "unreadable"},
		{"not a bool, sqlite", plistHead + `<key>IsEncrypted</key><string>true</string>` + plistTail, false, nil, "mapped", "unknown", "not a boolean"},
		{"no key, sqlite", plistHead + `<key>Other</key><true/>` + plistTail, false, nil, "mapped", "unknown", "no IsEncrypted key"},
		{"duplicate key, sqlite", plistHead + `<key>IsEncrypted</key><false/><key>IsEncrypted</key><true/>` + plistTail, false, nil, "mapped", "unknown", "unreadable"},
		{"duplicate key, high entropy", plistHead + `<key>IsEncrypted</key><false/><key>IsEncrypted</key><true/>` + plistTail, false, randomBytes(4096), "unparsed", "encrypted", encryptedReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bc := newBackupCase(t)
			bc.add(bspec{files: stdFiles(), plist: tc.plist, noPlist: tc.noPlist, manifest: tc.manifest})
			p := bc.plan(bc.host(nil))
			if len(p.Backups) != 1 {
				t.Fatalf("Backups = %+v", p.Backups)
			}
			b := first(t, p.Backups)
			if b.Status != tc.status || b.Encryption != tc.enc || !strings.Contains(b.Reason, tc.reason) {
				t.Errorf("got status %q encryption %q reason %q; want %q %q containing %q", b.Status, b.Encryption, b.Reason, tc.status, tc.enc, tc.reason)
			}
			if tc.status == "unparsed" && b.Reason == "" {
				t.Error("an unparsed backup has no reason")
			}
		})
	}
}

func TestManifestPlistAbsentMapsWithEncryptionUnknown(t *testing.T) {
	bc := newBackupCase(t)
	bc.add(bspec{files: stdFiles(), noPlist: true})
	h := bc.host(nil)
	p := bc.plan(h)
	b := first(t, p.Backups)
	if b.Status != "mapped" || b.Encryption != "unknown" || b.ManifestPlist != nil || b.ManifestDB == nil || len(p.Rows) != 1 {
		t.Fatalf("Backups[0] = %+v, rows %d", b, len(p.Rows))
	}
	bc.run(h)
	as := decodeEntry[artparse.AnalysisStart](t, entry0(t, bc.entries(artparse.ActionAnalysisStart)))
	if len(as.Backups) != 1 || as.Backups[0].ManifestPlist != nil || as.Backups[0].ManifestDB == nil {
		t.Errorf("analysis.start backups = %+v", as.Backups)
	}
	if raw := entry0(t, bc.entries(artparse.ActionAnalysisStart)).Details["backups"].([]any)[0].(map[string]any); raw["manifest_plist"] != nil {
		t.Errorf("manifest_plist = %v, want JSON null", raw["manifest_plist"])
	}
}

func TestOversizeManifestPlistIsEncryptionUnknown(t *testing.T) {
	big := plistNo + strings.Repeat(" ", iosbackup.MaxManifestPlistBytes+1-len(plistNo))
	for _, tc := range []struct {
		name     string
		manifest []byte
		status   string
		enc      string
	}{
		{"sqlite", nil, "mapped", "unknown"},
		{"high entropy", randomBytes(4096), "unparsed", "encrypted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc := newBackupCase(t)
			r := bc.add(bspec{files: stdFiles(), plist: big, manifest: tc.manifest})
			opened := false
			h := bc.host(func(o *artparse.Options) {
				artparse.WithOnOpen(o, func(id string) { opened = opened || id == r.plist.ID })
			})
			p := bc.plan(h)
			b := first(t, p.Backups)
			if b.Status != tc.status || b.Encryption != tc.enc {
				t.Errorf("status %q encryption %q, want %q %q", b.Status, b.Encryption, tc.status, tc.enc)
			}
			if tc.status == "mapped" && !strings.Contains(b.Reason, "too large") {
				t.Errorf("reason = %q, want it to say too large", b.Reason)
			}
			if opened {
				t.Error("the oversize Manifest.plist was opened")
			}
		})
	}
}

func TestUnknownEncryptionIsWarnedNotSilent(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(bspec{files: stdFiles(), noPlist: true})
	h := bc.host(nil)
	sum := bc.run(h)
	if sum.Class() != artparse.ClassOK {
		t.Errorf("Class = %v, want ClassOK (the exit class is unchanged)", sum.Class())
	}
	acts := bc.actions()
	start, warn, job := slices.Index(acts, artparse.ActionAnalysisStart), slices.Index(acts, evidence.ActionAnalysisWarning), slices.Index(acts, artparse.ActionParseJobStart)
	if start < 0 || start >= warn || warn >= job {
		t.Fatalf("audit actions %v: want analysis.start, analysis.warning, parse.job.start", acts)
	}
	ws := bc.entries(evidence.ActionAnalysisWarning)
	if len(ws) != 1 {
		t.Fatalf("%d warnings, want exactly 1", len(ws))
	}
	w := decodeEntry[artparse.HostWarning](t, ws[0])
	if !strings.Contains(w.Reason, "encryption unknown") || w.Path != "backup:"+r.key+"/UDID1" {
		t.Errorf("warning = %+v", w)
	}
}

func TestTwoBackupDirsInOneAcquisitionNeverShareCompanions(t *testing.T) {
	bc := newBackupCase(t)
	const ab, abImg = "Library/AddressBook/AddressBook.sqlitedb", "Library/AddressBook/AddressBookImages.sqlitedb"
	mk := func(dir string) *bres {
		return bc.add(bspec{dev: "UDID1", dir: dir, files: []bfile{
			{domain: smsDomain, rel: smsRel, data: "db-" + dir},
			{domain: smsDomain, rel: smsRel + "-wal", data: "wal-" + dir},
			{domain: smsDomain, rel: ab, data: "ab-" + dir},
			{domain: smsDomain, rel: abImg, data: "img-" + dir},
		}})
	}
	r1, r2 := mk("UDID1"), mk("UDID2")
	if r1.key != r2.key {
		t.Fatalf("the two backups are in different acquisitions %q %q", r1.key, r2.key)
	}
	p := bc.plan(bc.host(nil))
	if len(p.Rows) != 4 {
		t.Fatalf("rows = %d, want 4 (two sms.db, two address books)", len(p.Rows))
	}
	for _, row := range p.Rows {
		j := row.Job
		if row.Status != artparse.StatusNew {
			t.Errorf("job %s: status %s reason %s", j.Primary.Logical, row.Status, row.Reason)
		}
		dir, _, _ := strings.Cut(strings.TrimPrefix(j.Primary.Artifact.Source.RemotePath, ""), "/")
		own := r1
		if dir == "UDID2" {
			own = r2
		}
		var want string
		var role string
		switch {
		case strings.HasSuffix(j.Primary.Logical, "sms.db"):
			role, want = "wal", own.file(smsDomain, smsRel+"-wal").ID
		default:
			role, want = "images", own.file(smsDomain, abImg).ID
		}
		if got := j.Others[role].Artifact.ID; got != want {
			t.Errorf("job of %s in %s: %s = %q, want %q (its own backup's file)", j.Primary.Logical, dir, role, got, want)
		}
	}
}

func TestResolveBackupsCancelAfterIntegrityKeepsTheFinding(t *testing.T) {
	bc := newBackupCase(t)
	a := bc.add(bspec{acq: "acqA", files: stdFiles()})
	b := bc.add(bspec{acq: "acqB", files: stdFiles()})
	tamper(t, bc.c, a.db)
	var cancel context.CancelFunc
	h := bc.host(func(o *artparse.Options) {
		artparse.WithOnOpen(o, func(id string) {
			if id == b.db.ID && cancel != nil {
				cancel()
			}
		})
	})
	fresh := func() context.Context {
		var ctx context.Context
		ctx, cancel = context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return ctx
	}
	checkA := func(t *testing.T, rs []artparse.BackupResolution) {
		t.Helper()
		if len(rs) != 1 || rs[0].Status != "refused" || !rs[0].Integrity || rs[0].Acquisition != a.key || !strings.Contains(rs[0].Reason, a.db.ID) {
			t.Fatalf("resolutions = %+v, want only A's refused finding naming %s", rs, a.db.ID)
		}
	}
	t.Run("ResolveBackups", func(t *testing.T) {
		snap, err := h.TakeSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		rs, err := h.ResolveBackups(fresh(), snap)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		checkA(t, rs)
	})
	t.Run("Plan", func(t *testing.T) {
		p, err := h.Plan(fresh(), artparse.Selection{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		checkA(t, p.Backups)
	})
	t.Run("Run", func(t *testing.T) {
		sum, err := h.Run(fresh(), artparse.RunOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		checkA(t, sum.Backups)
		if sum.Class() != artparse.ClassIntegrity {
			t.Errorf("Class = %v, want ClassIntegrity", sum.Class())
		}
	})
}

func TestWhyNoLogicalOfAnUnresolvedSnapshotSaysUnresolved(t *testing.T) {
	bc := newBackupCase(t)
	bc.add(stdSpec())
	h := bc.host(nil)
	snap := snapshotOf(t, h) // never passed to ResolveBackups
	_, counts, err := h.Discover(context.Background(), snap, artparse.Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if len(counts.UnnamedList) != 6 {
		t.Fatalf("UnnamedList = %+v, want all six backup artifacts", counts.UnnamedList)
	}
	unresolved := 0
	for _, u := range counts.UnnamedList {
		if strings.Contains(u.Reason, "Manifest.db") {
			t.Errorf("reason %q names Manifest.db although nothing was resolved", u.Reason)
		}
		if u.Reason == "backup not resolved in this snapshot" {
			unresolved++
		}
	}
	if unresolved != 3 {
		t.Errorf("%d artifacts say %q, want the 3 backup data files", unresolved, "backup not resolved in this snapshot")
	}
}

func TestBackupMappingNeverCrossesAcquisitionsOrDirectories(t *testing.T) {
	t.Run("two acquisitions of one device", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{acq: "acqA", files: []bfile{{domain: smsDomain, rel: smsRel, data: "a"}}})
		b := bc.add(bspec{acq: "acqB", files: []bfile{{domain: smsDomain, rel: smsRel, data: "b", asRel: "Library/Other/x.db"}}})
		named, _ := bc.names()
		if got := named[a.file(smsDomain, smsRel).ID]; got != "ios:HomeDomain/Library/SMS/sms.db" {
			t.Errorf("A's file = %q", got)
		}
		if got := named[b.file(smsDomain, smsRel).ID]; got != "ios:HomeDomain/Library/Other/x.db" {
			t.Errorf("B's file = %q, want its own acquisition's path", got)
		}
	})
	t.Run("an acquisition without Manifest.db", func(t *testing.T) {
		bc := newBackupCase(t)
		a := bc.add(bspec{acq: "acqA", files: stdFiles(), noManifest: true})
		b := bc.add(bspec{acq: "acqB", files: stdFiles()})
		named, unnamed := bc.names()
		if got := unnamed[a.file(smsDomain, smsRel).ID]; !strings.Contains(got, "no Manifest.db in this backup") {
			t.Errorf("A's file: reason %q", got)
		}
		if named[b.file(smsDomain, smsRel).ID] == "" {
			t.Error("B's file was not named")
		}
		p := bc.plan(bc.host(nil))
		if len(p.Backups) != 2 || first(t, p.Backups).Status != "unparsed" || second(t, p.Backups).Status != "mapped" {
			t.Errorf("Backups = %+v", p.Backups)
		}
	})
	t.Run("two directories of one acquisition", func(t *testing.T) {
		bc := newBackupCase(t)
		other := bfile{domain: smsDomain, rel: "Library/SMS/other.db", data: "two"}
		r1 := bc.add(bspec{dev: "UDID1", dir: "UDID1", files: []bfile{{domain: smsDomain, rel: smsRel, data: "one"}, other}})
		unlisted := other
		unlisted.unlisted = true // UDID1's Manifest.db lists this file ID, UDID2's does not
		r2 := bc.add(bspec{dev: "UDID1", dir: "UDID2", files: []bfile{unlisted}})
		named, unnamed := bc.names()
		if got := named[r1.file(smsDomain, other.rel).ID]; got != "ios:HomeDomain/Library/SMS/other.db" {
			t.Errorf("UDID1's file = %q", got)
		}
		if _, ok := named[r2.file(smsDomain, other.rel).ID]; ok || !strings.Contains(unnamed[r2.file(smsDomain, other.rel).ID], "file ID not listed in Manifest.db") {
			t.Errorf("UDID2's file: named %q, reason %q; want unnamed (listed only in the other directory's Manifest.db)", named[r2.file(smsDomain, other.rel).ID], unnamed[r2.file(smsDomain, other.rel).ID])
		}
		p := bc.plan(bc.host(nil))
		if len(p.Backups) != 2 || first(t, p.Backups).Dir != "UDID1" || second(t, p.Backups).Dir != "UDID2" || first(t, p.Backups).Acquisition != second(t, p.Backups).Acquisition {
			t.Errorf("Backups = %+v, want one resolution per directory", p.Backups)
		}
	})
}

func first(t testing.TB, rs []artparse.BackupResolution) artparse.BackupResolution {
	t.Helper()
	if len(rs) < 1 {
		t.Fatal("no backup resolution")
	}
	return rs[0]
}

func second(t testing.TB, rs []artparse.BackupResolution) artparse.BackupResolution {
	t.Helper()
	if len(rs) < 2 {
		t.Fatalf("%d backup resolutions, want 2", len(rs))
	}
	return rs[1]
}

func first2(t testing.TB, bs []artparse.AuditBackup) artparse.AuditBackup {
	t.Helper()
	if len(bs) != 1 {
		t.Fatalf("analysis.start backups = %+v, want one", bs)
	}
	return bs[0]
}

func entry0(t testing.TB, es []evidence.AuditEntry) evidence.AuditEntry {
	t.Helper()
	if len(es) == 0 {
		t.Fatal("no such audit entry")
	}
	return es[0]
}

// A backup Manifest.plist calls encrypted whose Manifest.db reads, with no file a parser matches, still
// produces a host warning and a non-clean run: its encrypted content was never parsed.
func TestEncryptedMappedBackupWithNoMatchingParserIsNotClean(t *testing.T) {
	bc := newBackupCase(t)
	r := bc.add(bspec{files: []bfile{{domain: "HomeDomain", rel: "Library/Other/unclaimed.bin", data: "XX"}}, plist: plistYes})
	h := bc.host(nil)
	p := bc.plan(h)
	if len(p.Rows) != 0 || len(p.Backups) != 1 || first(t, p.Backups).Status != "mapped" || first(t, p.Backups).Encryption != "encrypted" {
		t.Fatalf("rows %d backups %+v", len(p.Rows), p.Backups)
	}
	sum := bc.run(h)
	if sum.Class() != artparse.ClassPartial {
		t.Errorf("Class = %v, want ClassPartial", sum.Class())
	}
	ws := bc.entries(evidence.ActionAnalysisWarning)
	if len(ws) != 1 {
		t.Fatalf("%d warnings, want 1", len(ws))
	}
	w := decodeEntry[artparse.HostWarning](t, ws[0])
	if w.Path != "backup:"+r.key+"/UDID1" || !strings.Contains(w.Reason, "encrypted") || w.Source != "host" {
		t.Errorf("warning = %+v", w)
	}
}
