package androidwacontacts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeOf(t *testing.T, fx parsertest.Fixture, remote string) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidwacontacts.Parser{}
	b := fx.Bundle(h, remote)
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatalf("Probe(%s): %v", remote, err)
	}
	return got
}

func probeVariant(t *testing.T, variant string) parse.Applicability {
	t.Helper()
	return probeOf(t, parsertest.LoadFixture(t, "testdata", variant), remoteFor(variant))
}

func TestMetaIsValid(t *testing.T) {
	m := androidwacontacts.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "android.whatsapp.contacts" || m.Version != androidwacontacts.Version || len(m.Emits) != 1 || m.Emits[0].Type != "contact" {
		t.Errorf("meta %+v", m)
	}
	roles := map[string]bool{}
	for _, in := range m.Inputs {
		roles[in.Role] = true
	}
	if m.Inputs[0].Role != "db" || !m.Inputs[0].Required || !roles["wal"] || !roles["journal"] {
		t.Errorf("inputs %+v", m.Inputs)
	}
	if len(m.Inputs[0].Globs) != 6 {
		t.Errorf("primary globs %v", m.Inputs[0].Globs)
	}
}

// 4I-R21: no primary glob matches a companion, and every backup name is matched.
func TestWAContactsCompanionIsNeverAPrimary(t *testing.T) {
	m := androidwacontacts.Parser{}.Meta()
	var globs []*parse.Glob
	for _, g := range m.Inputs[0].Globs {
		cg, err := parse.CompileGlob(g)
		if err != nil {
			t.Fatalf("glob %q: %v", g, err)
		}
		globs = append(globs, cg)
	}
	matches := func(logical string) bool {
		for _, g := range globs {
			if g.Match(logical) {
				return true
			}
		}
		return false
	}
	dirs := []string{
		"/data/data/com.whatsapp/databases/", "/sdcard/WhatsApp/Databases/", "/sdcard/WhatsApp Business/Databases/",
		"/storage/emulated/0/Android/media/com.whatsapp/WhatsApp/Databases/", "/data/data/com.whatsapp.w4b/databases/",
	}
	for _, d := range dirs {
		for _, f := range []string{"wa.db-wal", "wa.db-journal", "wa.db-shm"} {
			if matches("android:" + d + f) {
				t.Errorf("a primary glob matches the companion %s%s", d, f)
			}
		}
	}
	for _, d := range []string{"/sdcard/WhatsApp/Databases/", "/sdcard/WhatsApp Business/Databases/"} {
		for _, f := range []string{"wa.db", "wa.db.crypt15", "wa.db.crypt14"} {
			if !matches("android:" + d + f) {
				t.Errorf("no primary glob matches the backup %s%s", d, f)
			}
		}
	}
	for _, p := range []string{"/data/data/com.whatsapp/databases/wa.db", "/data/user/10/com.whatsapp.w4b/databases/wa.db"} {
		if !matches("android:" + p) {
			t.Errorf("no primary glob matches the live database %s", p)
		}
	}
	roles := map[string][]string{}
	for _, in := range m.Inputs[1:] {
		roles[in.Role] = in.Companions
	}
	if len(roles["wal"]) != 1 || roles["wal"][0] != "-wal" || len(roles["journal"]) != 1 || roles["journal"][0] != "-journal" {
		t.Errorf("companion roles %v", roles)
	}
}

func TestProbeAcceptsEveryParsedVariant(t *testing.T) {
	for _, v := range []string{"wa-2.11", "wa-current", "wa-business", "wa-deleted"} {
		if got := probeVariant(t, v); got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v", v, got)
		}
	}
}

// 4I-R3: an encrypted backup is unparsed, and a plain copy at a backup path is "backup copy, not parsed".
func TestWAContactsBackupsAreUnparsed(t *testing.T) {
	got := probeVariant(t, "wa-crypt15")
	if got.Status != parse.Encrypted || got.Reason != "WhatsApp encrypted backup (crypt12/14/15): sub-project 10" {
		t.Errorf("wa-crypt15: Probe = %+v", got)
	}
	got = probeVariant(t, "wa-backup-plain")
	if got.Status != parse.UnsupportedSchema || got.Reason != "backup-copy: plain SQLite at a WhatsApp backup path, not parsed (sub-project 10)" {
		t.Errorf("wa-backup-plain: Probe = %+v", got)
	}
	res := runVariant(t, "wa-backup-plain")
	if !errors.Is(res.Err, androidwacontacts.ErrNotLiveDatabase) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records; want ErrNotLiveDatabase and none", res.Err, len(res.Records))
	}
	// text at a backup path is not SQLite
	fx := parsertest.Fixture{Name: "text", Files: map[string][]byte{"db": []byte(strings.Repeat("plain text, not a database. ", 4))}}
	if got := probeOf(t, fx, "/sdcard/WhatsApp Business/Databases/wa.db"); got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at a WhatsApp backup path" {
		t.Errorf("text at a backup path: Probe = %+v", got)
	}
}

// Review focus 1, 4I-R3: a database under external Android/data is not the app's live directory.
func TestWAContactsNonLiveDatabaseDirIsNotParsedAsLive(t *testing.T) {
	got := probeVariant(t, "wa-other-location")
	if got.Status != parse.UnsupportedSchema || got.Reason != "backup-copy: not the app's live databases directory" {
		t.Fatalf("Probe = %+v", got)
	}
	for _, remote := range []string{"/sdcard/Android/media/com.whatsapp/databases/wa.db", "/data/user/1234567/com.whatsapp/databases/wa.db"} {
		fx := parsertest.LoadFixture(t, "testdata", "wa-current")
		if got := probeOf(t, fx, remote); got.Status != parse.UnsupportedSchema || !strings.HasPrefix(got.Reason, "backup-copy:") {
			t.Errorf("%s: Probe = %+v", remote, got)
		}
	}
	// a LIVE database parsed from an OTHER path emits nothing
	fx := parsertest.LoadFixture(t, "testdata", "wa-current")
	h := parsertest.NewHarness(t)
	p := androidwacontacts.Parser{}
	b := fx.Bundle(h, remoteOtherPlace)
	res := h.RunLenient(p, b.Input(h, p, false))
	if !errors.Is(res.Err, androidwacontacts.ErrNotLiveDatabase) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records", res.Err, len(res.Records))
	}
}

// Review focus 2, 4I-R3: a zero-length or text file at the live path is unparsed, never a benign skip.
func TestWAContactsNonSQLiteAtLivePathIsUnparsed(t *testing.T) {
	for name, data := range map[string][]byte{"zero": {}, "text": []byte(strings.Repeat("not a database, only text. ", 4))} {
		fx := parsertest.Fixture{Name: name, Files: map[string][]byte{"db": data}}
		got := probeOf(t, fx, remoteLive)
		if got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the WhatsApp databases path" {
			t.Errorf("%s: Probe = %+v", name, got)
		}
	}
	fx := parsertest.LoadFixture(t, "testdata", "wa-crypt15")
	if got := probeOf(t, fx, remoteLive); got.Status != parse.Encrypted || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted at the live path: Probe = %+v", got)
	}
	fx = parsertest.Fixture{Name: "text", Files: map[string][]byte{"db": []byte("text"), "wal": {}}}
	if got := probeOf(t, fx, remoteLive); got.Status != parse.UnsupportedSchema || got.Reason != "the main file is not a database but a -wal or -journal companion exists" {
		t.Errorf("text with a companion: Probe = %+v", got)
	}
}

// A live database with no wa_contacts table is a store we cannot read, not another kind of file.
func TestWAContactsMissingTableIsUnsupported(t *testing.T) {
	got := probeVariant(t, "wa-no-table")
	if got.Status != parse.UnsupportedSchema || got.Reason != "no wa_contacts table at the WhatsApp databases path" {
		t.Fatalf("Probe = %+v", got)
	}
	// a wa_contacts table without its jid column is unsupported too, loudly
	bld := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	bld.CreateTable("wa_contacts", "create table wa_contacts(_id integer primary key, number text)")
	fx := parsertest.Fixture{Name: "nojid", Files: map[string][]byte{"db": bld.Bytes()}}
	if got := probeOf(t, fx, remoteLive); got.Status != parse.UnsupportedSchema {
		t.Errorf("wa_contacts without jid: Probe = %+v", got)
	}
}
