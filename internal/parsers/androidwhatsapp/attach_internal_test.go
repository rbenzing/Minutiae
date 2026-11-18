package androidwhatsapp

import (
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// 4I-R7 and spec 6.7: the split of a media path into its root, the part after the app folder and the form.
func TestSplitMediaPath(t *testing.T) {
	cases := []struct {
		path, app string
		user      int
		named     bool
		rest      string
		form      string
		ok        bool
	}{
		{"/storage/emulated/10/WhatsApp/Media/WhatsApp Images/IMG-1.jpg", appConsumer, 10, true, "Media/WhatsApp Images/IMG-1.jpg", formLegacy, true},
		{"android:/storage/emulated/10/WhatsApp/Media/x", appConsumer, 10, true, "Media/x", formLegacy, true},
		{"/storage/emulated/0/Android/media/com.whatsapp/WhatsApp/Media/x", appConsumer, 0, true, "Media/x", formAndroid11, true},
		{"/data/media/3/WhatsApp/Media/x", appConsumer, 3, true, "Media/x", formLegacy, true},
		{"/media/0/WhatsApp/Media/x", appConsumer, 0, true, "Media/x", formLegacy, true},
		{"/sdcard/WhatsApp/Media/x", appConsumer, 0, false, "Media/x", formLegacy, true},
		{"/storage/self/primary/WhatsApp/Media/x", appConsumer, 0, false, "Media/x", formLegacy, true},
		{"/mnt/other/WhatsApp/Media/x", appConsumer, 0, false, "Media/x", formLegacy, true}, // any other prefix names no user
		{"/storage/emulated/0/WhatsApp Business/Media/x", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp Business/Media/x", appBusiness, 0, true, "Media/x", formLegacy, true},
		{"/storage/emulated/0/Android/media/com.whatsapp.w4b/WhatsApp Business/Media/x", appBusiness, 0, true, "Media/x", formAndroid11, true},
		{"/storage/emulated/0/Android/media/com.whatsapp.w4b/WhatsApp Business/Media/x", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/1234567/WhatsApp/Media/x", appConsumer, 0, false, "Media/x", formLegacy, true}, // 7 digits: not a user
		{"/storage/emulated/0/WhatsApp/../x", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp/Media/*", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp/Media\\x", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp/Media/x\x00", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp", appConsumer, 0, false, "", "", false},
		{"/storage/emulated/0/WhatsApp/Media/x", appUnknown, 0, false, "", "", false},
		{"storage/emulated/0/WhatsApp/Media/x", appConsumer, 0, false, "", "", false},
	}
	for _, c := range cases {
		root, rest, form, ok := splitMediaPath(c.path, c.app)
		if ok != c.ok || (ok && (root.Named != c.named || (c.named && root.User != c.user) || rest != c.rest || form != c.form)) {
			t.Errorf("splitMediaPath(%q, %s) = %+v %q %q %v; want user %d named %v %q %q %v", c.path, c.app, root, rest, form, ok, c.user, c.named, c.rest, c.form, c.ok)
		}
	}
}

func TestIsKnownRoot(t *testing.T) {
	for p, want := range map[string]bool{
		"/storage/emulated/10/WhatsApp/x": true, "/data/media/3/WhatsApp/x": true, "/media/0/WhatsApp/x": true,
		"/sdcard/WhatsApp/x": true, "/storage/self/primary/WhatsApp/x": true,
		"/storage/ABCD-1234/WhatsApp/x": false, "/mnt/sdcard/WhatsApp/x": false, "/storage/emulated/x/WhatsApp/x": false,
		"/media/x": false, "/data/data/com.whatsapp/files/x": false, "storage/emulated/0/x": false, "/sdcard/../x": false,
	} {
		if got := isKnownRoot(p); got != want {
			t.Errorf("isKnownRoot(%q) = %v, want %v", p, got, want)
		}
	}
}

// fakeLookup returns every artifact it holds whatever the glob: the callers filter by path, device and recovery.
type fakeLookup []parse.Artifact

func (f fakeLookup) Find(string) []parse.Artifact { return append([]parse.Artifact(nil), f...) }
func (f fakeLookup) Open(parse.Artifact) (io.ReaderAt, error) {
	return nil, io.ErrUnexpectedEOF
}

func art(id, logical, device, acq string) parse.Artifact {
	return parse.Artifact{ID: id, Logical: logical, Acquisition: acq, Source: parse.SourceInfo{DeviceID: device}}
}

// R5a, 4I-R12: the database is the only live one of its app when the artifacts at a LIVE path of its package, of the
// device, not recovered, are exactly the primary: another user, another acquisition and a snapshot copy count; a recovered
// copy, another device, another package and a path outside the live directory do not.
func TestSoleLiveCountsLiveDatabasesOfTheAppOnTheDevice(t *testing.T) {
	prim := art("P", "android:/data/data/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	db := classifyDB(prim.Logical)
	snap := parse.SnapshotInfo{}
	recovered := art("R", "android:/data/user/10/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	recovered.Recovery = &parse.RecoveryInfo{}
	snapshot := art("S", "android:/data/user/11/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	snapshot.Source.Snapshot = &snap
	for name, c := range map[string]struct {
		others []parse.Artifact
		want   bool
	}{
		"alone":                         {nil, true},
		"another user":                  {[]parse.Artifact{art("U", "android:/data/user/10/com.whatsapp/databases/msgstore.db", "SER1", "acq1")}, false},
		"another acquisition":           {[]parse.Artifact{art("A", prim.Logical, "SER1", "acq2")}, false},
		"a snapshot copy":               {[]parse.Artifact{snapshot}, false},
		"a recovered copy":              {[]parse.Artifact{recovered}, true},
		"another device":                {[]parse.Artifact{art("D", "android:/data/user/10/com.whatsapp/databases/msgstore.db", "SER2", "acq-SER2")}, true},
		"another package":               {[]parse.Artifact{art("B", "android:/data/data/com.whatsapp.w4b/databases/msgstore.db", "SER1", "acq1")}, true},
		"outside the live directory":    {[]parse.Artifact{art("O", "android:/storage/emulated/0/Android/data/com.whatsapp/databases/msgstore.db", "SER1", "acq1")}, true},
		"a backup folder":               {[]parse.Artifact{art("K", "android:/sdcard/WhatsApp/Databases/msgstore.db", "SER1", "acq1")}, true},
		"a partition extraction (user)": {[]parse.Artifact{art("X", "android:/user/10/com.whatsapp/databases/msgstore.db", "SER1", "acq1")}, false},
	} {
		in := &parse.Input{Primary: prim, Lookup: fakeLookup(append([]parse.Artifact{prim}, c.others...))}
		if got := soleLive(in, db); got != c.want {
			t.Errorf("%s: soleLive = %v, want %v", name, got, c.want)
		}
	}
	// no device on the primary: unknown, never alone
	noDev := prim
	noDev.Source.DeviceID = ""
	if soleLive(&parse.Input{Primary: noDev, Lookup: fakeLookup{noDev}}, db) {
		t.Errorf("a primary with no device was taken as alone")
	}
	if soleLive(&parse.Input{Primary: prim}, db) {
		t.Errorf("no Lookup was taken as alone")
	}
}

// R5, R5a: the pairing of a root with the database, and the outcome of a lookup over several candidates.
func TestResolveMediaPairsUserAndSource(t *testing.T) {
	prim := art("P", "android:/data/user/10/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	db := classifyDB(prim.Logical)
	const rel = "Media/WhatsApp Images/IMG-1.jpg"
	at := func(id, root, device string) parse.Artifact {
		acq := "acq1"
		if device != "SER1" {
			acq = "acq-" + device
		}
		return art(id, "android:"+root+"/WhatsApp/"+rel, device, acq)
	}
	resolve := func(ref string, files ...parse.Artifact) (string, lookup) {
		in := &parse.Input{Primary: prim, Lookup: fakeLookup(append([]parse.Artifact{prim}, files...))}
		a, st, _ := resolveMedia(in, db, ref, nil)
		return a.ID, st
	}
	for name, c := range map[string]struct {
		ref   string
		files []parse.Artifact
		id    string
		want  lookup
	}{
		"same user":                   {rel, []parse.Artifact{at("a", "/storage/emulated/10", "SER1")}, "a", lookupFound},
		"other user only":             {rel, []parse.Artifact{at("a", "/storage/emulated/0", "SER1")}, "", lookupOtherUser},
		"same and other user":         {rel, []parse.Artifact{at("a", "/storage/emulated/10", "SER1"), at("b", "/storage/emulated/0", "SER1")}, "a", lookupFound},
		"two of the same user":        {rel, []parse.Artifact{at("a", "/storage/emulated/10", "SER1"), at("b", "/data/media/10", "SER1")}, "", lookupAmbiguous},
		"another device only":         {rel, []parse.Artifact{at("a", "/storage/emulated/10", "SER2")}, "", lookupOtherSource},
		"no user, the only database":  {rel, []parse.Artifact{at("a", "/sdcard", "SER1")}, "a", lookupFound},
		"no user and a same-user one": {rel, []parse.Artifact{at("a", "/sdcard", "SER1"), at("b", "/storage/emulated/10", "SER1")}, "", lookupAmbiguous},
		"nothing":                     {rel, nil, "", lookupMissing},
		"not under Media":             {"Databases/x", nil, "", lookupNone},
		"parent element":              {"Media/../x", nil, "", lookupNone},
		"empty":                       {"", nil, "", lookupNone},
	} {
		if id, st := resolve(c.ref, c.files...); id != c.id || st != c.want {
			t.Errorf("%s: %q %v, want %q %v", name, id, st, c.id, c.want)
		}
	}
	// a second live database of the app makes a path with no user undecided, and leaves a path that names one alone
	other := art("O", "android:/data/user/0/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	in := &parse.Input{Primary: prim, Lookup: fakeLookup{prim, other, at("a", "/sdcard", "SER1")}}
	if _, st, p := resolveMedia(in, db, rel, nil); st != lookupUserUndecided || p != pairUndecided {
		t.Errorf("two live databases, a path with no user: %v %v", st, p)
	}
	in = &parse.Input{Primary: prim, Lookup: fakeLookup{prim, other, at("a", "/storage/emulated/10", "SER1")}}
	if a, st, p := resolveMedia(in, db, rel, nil); st != lookupFound || a.ID != "a" || p != pairSameUser {
		t.Errorf("two live databases, a path of the database's user: %q %v %v", a.ID, st, p)
	}
	// an absolute path: only a known root of the app's own folder is looked up
	abs := "/storage/emulated/10/WhatsApp/" + rel
	in = &parse.Input{Primary: prim, Lookup: fakeLookup{prim, art("a", "android:"+abs, "SER1", "acq1")}}
	if a, st, _ := resolveMedia(in, db, abs, nil); st != lookupFound || a.ID != "a" {
		t.Errorf("absolute path: %q %v", a.ID, st)
	}
	for _, ref := range []string{"/storage/emulated/10/DCIM/x.jpg", "/data/data/com.whatsapp/files/x", "/storage/emulated/10/WhatsApp Business/Media/x"} {
		if _, st, _ := resolveMedia(in, db, ref, nil); st != lookupNone {
			t.Errorf("%q: %v, want no lookup", ref, st)
		}
	}
}

// M1: a path holding '*', the one character the case glob (parse.CompileGlob) reads as a pattern, is not expressible:
// no lookup, so never "missing". Every other character is literal to that glob, so a path holding one is looked up.
func TestResolveMediaGlobMetacharactersAreUndecided(t *testing.T) {
	prim := art("P", "android:/data/user/10/com.whatsapp/databases/msgstore.db", "SER1", "acq1")
	db := classifyDB(prim.Logical)
	in := &parse.Input{Primary: prim, Lookup: fakeLookup{prim}}
	refs := func(c string) []string {
		return []string{"Media/WhatsApp Images/IMG" + c + ".jpg", "/storage/emulated/10/WhatsApp/Media/WhatsApp Images/IMG" + c + ".jpg"}
	}
	for _, ref := range refs("*") {
		if _, st, _ := resolveMedia(in, db, ref, nil); st != lookupUndecidedPath {
			t.Errorf("%q: %v, want lookupUndecidedPath", ref, st)
		}
	}
	for _, c := range []string{"[", "]", "?", "{", "}"} {
		for _, ref := range refs(c) {
			if _, st, _ := resolveMedia(in, db, ref, nil); st == lookupUndecidedPath {
				t.Errorf("%q: lookupUndecidedPath, but %q is literal to the case glob, so the path must be looked up", ref, c)
			}
		}
	}
}
