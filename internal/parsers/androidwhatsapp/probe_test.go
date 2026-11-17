package androidwhatsapp_test

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func probeOf(t *testing.T, fx parsertest.Fixture, remote string) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
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
	m := androidwhatsapp.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "android.whatsapp.msgstore" || m.Version != androidwhatsapp.Version || len(m.Emits) != 2 {
		t.Errorf("meta %+v", m)
	}
	if len(m.Claims) != 0 {
		t.Errorf("claims %v: the typed recovery claims come with plan 4I task 14", m.Claims)
	}
	roles := map[string]bool{}
	for _, in := range m.Inputs {
		roles[in.Role] = true
	}
	if m.Inputs[0].Role != "db" || !m.Inputs[0].Required || !roles["wal"] || !roles["journal"] {
		t.Errorf("inputs %+v", m.Inputs)
	}
}

// 4I-R21: no primary glob matches a companion, and every backup name is matched.
func TestWhatsAppCompanionIsNeverAPrimary(t *testing.T) {
	m := androidwhatsapp.Parser{}.Meta()
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
		for _, f := range []string{"msgstore.db-wal", "msgstore.db-journal", "msgstore.db-shm"} {
			if matches("android:" + d + f) {
				t.Errorf("a primary glob matches the companion %s%s", d, f)
			}
		}
	}
	for _, d := range []string{"/sdcard/WhatsApp/Databases/", "/sdcard/WhatsApp Business/Databases/"} {
		for _, f := range []string{"msgstore.db", "msgstore-2026-11-01.1.db", "msgstore.db.crypt14", "msgstore-2026-11-01.1.db.crypt15"} {
			if !matches("android:" + d + f) {
				t.Errorf("no primary glob matches the backup %s%s", d, f)
			}
		}
	}
	for _, p := range []string{"/data/data/com.whatsapp/databases/msgstore.db", "/data/user/10/com.whatsapp.w4b/databases/msgstore.db"} {
		if !matches("android:" + p) {
			t.Errorf("no primary glob matches the live database %s", p)
		}
	}
	// the companions reach the job only through their roles
	roles := map[string][]string{}
	for _, in := range m.Inputs[1:] {
		roles[in.Role] = in.Companions
	}
	if len(roles["wal"]) != 1 || roles["wal"][0] != "-wal" || len(roles["journal"]) != 1 || roles["journal"][0] != "-journal" {
		t.Errorf("companion roles %v", roles)
	}
}

func TestProbeAcceptsEveryParsedStructuralVariant(t *testing.T) {
	for _, v := range []string{
		"android-legacy-2.11", "android-transition-legacydata", "android-transition-moderndata", "android-transition-both",
		"android-transition-walonly", "android-transition-empty", "android-modern-2021", "android-modern-2026", "android-modern-business",
	} {
		if got := probeVariant(t, v); got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v", v, got)
		}
	}
}

func TestWhatsAppNeitherTableIsUnsupportedSchema(t *testing.T) {
	got := probeVariant(t, "android-neither")
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "neither messages nor message table") {
		t.Fatalf("Probe = %+v", got)
	}
	// with a companion the main file proves nothing: Probe defers, Parse fails loudly
	fx := parsertest.LoadFixture(t, "testdata", "android-neither")
	fx.Files["wal"] = []byte{}
	if got := probeOf(t, fx, remoteLive); got.Status != parse.Applicable {
		t.Fatalf("Probe with a companion = %+v, want the deferral", got)
	}
}

// 4I-R3: encrypted backups and non-SQLite files at a backup path are unparsed, never empty.
func TestWhatsAppEncryptedBackupIsUnparsedNotEmpty(t *testing.T) {
	for _, v := range []string{"android-crypt14", "android-crypt15"} {
		got := probeVariant(t, v)
		if got.Status != parse.Encrypted || got.Reason != "WhatsApp encrypted backup (crypt12/14/15): sub-project 10" {
			t.Errorf("%s: Probe = %+v", v, got)
		}
	}
	got := probeVariant(t, "android-backup-notsqlite")
	if got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at a WhatsApp backup path" {
		t.Errorf("android-backup-notsqlite: Probe = %+v", got)
	}
	// an encrypted file outside the live directory is encrypted too
	fx := parsertest.LoadFixture(t, "testdata", "android-crypt14")
	if got := probeOf(t, fx, remoteOther); got.Status != parse.Encrypted {
		t.Errorf("encrypted file at an other path: Probe = %+v", got)
	}
}

// R7, 4I-R3: a plain copy at a backup path is "backup copy, not parsed", never applicable.
func TestWhatsAppPlainBackupCopyIsNotParsed(t *testing.T) {
	got := probeVariant(t, "android-backup-plain")
	if got.Status != parse.UnsupportedSchema || got.Reason != "backup-copy: plain SQLite at a WhatsApp backup path, not parsed (sub-project 10)" {
		t.Fatalf("Probe = %+v", got)
	}
	res, _ := runVariant(t, "android-backup-plain")
	if !errorsIs(res.Err, androidwhatsapp.ErrNotLiveDatabase) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records; want ErrNotLiveDatabase and none", res.Err, len(res.Records))
	}
}

// Review focus 1, 4I-R3: a database under external Android/data is not the app's live directory.
func TestWhatsAppNonLiveDatabaseDirIsNotParsedAsLive(t *testing.T) {
	got := probeVariant(t, "android-other-location")
	if got.Status != parse.UnsupportedSchema || got.Reason != "backup-copy: not the app's live databases directory" {
		t.Fatalf("Probe = %+v", got)
	}
	for _, remote := range []string{"/sdcard/Android/media/com.whatsapp/databases/msgstore.db", "/data/user/1234567/com.whatsapp/databases/msgstore.db"} {
		fx := parsertest.LoadFixture(t, "testdata", "android-modern-2026")
		if got := probeOf(t, fx, remote); got.Status != parse.UnsupportedSchema || !strings.HasPrefix(got.Reason, "backup-copy:") {
			t.Errorf("%s: Probe = %+v", remote, got)
		}
	}
	res, _ := runVariant(t, "android-other-location")
	if !errorsIs(res.Err, androidwhatsapp.ErrNotLiveDatabase) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records", res.Err, len(res.Records))
	}
}

// Review focus 2, 4I-R3: a zero-length or text file at the live path is unparsed, never a benign skip.
func TestWhatsAppNonSQLiteAtLivePathIsUnparsed(t *testing.T) {
	for name, data := range map[string][]byte{"zero": {}, "text": []byte(strings.Repeat("not a database, only text. ", 4))} {
		fx := parsertest.Fixture{Name: name, Files: map[string][]byte{"db": data}}
		got := probeOf(t, fx, remoteLive)
		if got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the WhatsApp databases path" {
			t.Errorf("%s: Probe = %+v", name, got)
		}
	}
	fx := parsertest.LoadFixture(t, "testdata", "android-crypt14")
	if got := probeOf(t, fx, remoteLive); got.Status != parse.Encrypted || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted at the live path: Probe = %+v", got)
	}
	fx = parsertest.Fixture{Name: "text", Files: map[string][]byte{"db": []byte("text"), "wal": {}}}
	if got := probeOf(t, fx, remoteLive); got.Status != parse.UnsupportedSchema || got.Reason != "the main file is not a database but a -wal or -journal companion exists" {
		t.Errorf("text with a companion: Probe = %+v", got)
	}
}
