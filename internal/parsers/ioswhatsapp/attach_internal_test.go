package ioswhatsapp

import (
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func TestMediaRel(t *testing.T) {
	long := func(n int) string { // n elements
		return strings.TrimSuffix(strings.Repeat("e/", n), "/")
	}
	for _, c := range []struct {
		name, stored string
		container    int // the container's element count
		rel, state   string
	}{
		{"plain", "Media/x/IMG.jpg", 1, "Media/x/IMG.jpg", ""},
		{"empty", "", 1, "", stateNoLocalPath},
		{"star", "Media/a*b.jpg", 1, "Media/a*b.jpg", stateNotExpressible},
		{"double star element", "Media/**/x", 1, "Media/**/x", stateNotExpressible},
		{"NUL", "Media/a\x00b.jpg", 1, "Media/a\x00b.jpg", stateNotExpressible},
		{"invalid UTF-8", "Media/a\xffb.jpg", 1, "Media/a\xffb.jpg", stateNotExpressible},
		{"absolute", "/var/mobile/x", 1, "/var/mobile/x", stateRefused},
		{"dotdot", "../x", 1, "../x", stateRefused},
		{"dot", "Media/./x", 1, "Media/./x", stateRefused},
		{"empty element", "Media//x", 1, "Media//x", stateRefused},
		{"trailing slash", "Media/x/", 1, "Media/x/", stateRefused},
		{"backslash", `Media\x`, 1, `Media\x`, stateRefused},
		{"64 elements under a one-element container", long(64), 1, long(64), stateRefused},
		{"62 elements under a one-element container", long(62), 1, long(62), ""},
		{"63 elements under a one-element container", long(63), 1, long(63), stateRefused},
		{"62 elements under an app group", long(62), 5, long(62), stateRefused},
		{"58 elements under an app group", long(58), 5, long(58), ""},
		{"the other glob characters are literal", "Media/[a]?{b}.jpg", 1, "Media/[a]?{b}.jpg", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			rel, state := mediaRel([]byte(c.stored), c.container)
			if state != c.state {
				t.Errorf("state %q, want %q", state, c.state)
			}
			if state != stateNotExpressible && rel != c.rel {
				t.Errorf("rel %q, want %q", rel, c.rel)
			}
		})
	}
}

func TestContainerElems(t *testing.T) {
	if n := containerElems("ios:AppDomainGroup-x"); n != 1 {
		t.Errorf("domain container: %d", n)
	}
	if n := containerElems(appGroupRoot + "ID"); n != 5 {
		t.Errorf("app group container: %d", n)
	}
}

// fakeRow is a ZWAMEDIAITEM row of a fake context: the rowid and the ZMESSAGE value.
type fakeRow struct{ id, msg int64 }

func (r fakeRow) Table() string                          { return tableMediaItem }
func (r fakeRow) Rowid() (int64, bool)                   { return r.id, true }
func (r fakeRow) IsNull(int) bool                        { return false }
func (r fakeRow) Int(int) (int64, bool)                  { return r.msg, true }
func (r fakeRow) Float(int) (float64, bool)              { return 0, false }
func (r fakeRow) Text(int) ([]byte, bool)                { return nil, false }
func (r fakeRow) Blob(int) ([]byte, bool)                { return nil, false }
func (r fakeRow) Range() (records.Range, parse.FileRole) { return records.Range{}, "" }

// fakeCtx answers the two joins of joinMedia from a list of rows; matchErr, when set, is what Match returns.
type fakeCtx struct {
	parse.MapContext
	rows     []fakeRow
	matchErr error
}

func (c fakeCtx) Get(_ string, id int64) (parse.Row, bool, error) {
	for _, r := range c.rows {
		if r.id == id {
			return r, true, nil
		}
	}
	return nil, false, nil
}

func (c fakeCtx) Match(_, _ string, key parse.JoinKey) ([]parse.Row, error) {
	if c.matchErr != nil {
		return nil, c.matchErr
	}
	var out []parse.Row
	for _, r := range c.rows {
		if r.msg == key.I {
			out = append(out, r)
		}
	}
	return out, nil
}

// The decision table of the media join (plan 4I2 vocabulary, "Media"): F the rows naming the message, B the forward key.
func TestJoinMedia(t *testing.T) {
	const msg = int64(1)
	row := func(id, m int64) fakeRow { return fakeRow{id, m} }
	for _, c := range []struct {
		name    string
		rows    []fakeRow
		err     error
		forward mediaKey
		want    mediaJoin
		wantRow int64
	}{
		{"F=1, B unset", []fakeRow{row(5, msg)}, nil, mediaKey{}, mediaRow, 5},
		{"F=1, B the same row", []fakeRow{row(5, msg)}, nil, mediaKey{PK: 5, Set: true}, mediaRow, 5},
		{"F=1, B another row", []fakeRow{row(5, msg), row(6, 99)}, nil, mediaKey{PK: 6, Set: true}, mediaUndecided, 0},
		{"F=1, B dangling", []fakeRow{row(5, msg)}, nil, mediaKey{PK: 77, Set: true}, mediaUndecided, 0},
		{"F=0, B unset", []fakeRow{row(6, 99)}, nil, mediaKey{}, mediaNone, 0},
		{"F=0, B names a row of another message", []fakeRow{row(6, 99)}, nil, mediaKey{PK: 6, Set: true}, mediaUndecided, 0},
		{"F=0, B dangling", []fakeRow{row(6, 99)}, nil, mediaKey{PK: 77, Set: true}, mediaNone, 0},
		{"|F|=2, B unset", []fakeRow{row(5, msg), row(6, msg)}, nil, mediaKey{}, mediaUndecided, 0},
		{"|F|=2, B one of them", []fakeRow{row(5, msg), row(6, msg)}, nil, mediaKey{PK: 5, Set: true}, mediaUndecided, 0},
		{"B unreadable", []fakeRow{row(5, msg)}, nil, mediaKey{Bad: true}, mediaUndecided, 0},
		{"lossy index", []fakeRow{row(5, msg)}, sqlitedb.ErrKeyUndecidable, mediaKey{}, mediaUndecided, 0},
		{"lossy index, B set", nil, sqlitedb.ErrKeyUndecidable, mediaKey{PK: 5, Set: true}, mediaUndecided, 0},
		{"table absent", nil, sqlitedb.ErrNoSuchTable, mediaKey{}, mediaUndecided, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, j, err := joinMedia(fakeCtx{rows: c.rows, matchErr: c.err}, msg, c.forward)
			if err != nil {
				t.Fatal(err)
			}
			if j != c.want {
				t.Fatalf("join %d, want %d", j, c.want)
			}
			if j != mediaRow {
				if got != nil {
					t.Errorf("a row was returned for outcome %d", j)
				}
				return
			}
			if id, _ := got.Rowid(); id != c.wantRow {
				t.Errorf("row %d, want %d", id, c.wantRow)
			}
		})
	}
}

// lookupFake is a Lookuper over a fixed list that counts its Finds.
type lookupFake struct {
	arts  []parse.Artifact
	finds []string
}

func (l *lookupFake) Find(g string) []parse.Artifact {
	l.finds = append(l.finds, g)
	return l.arts
}

func (l *lookupFake) Open(parse.Artifact) (io.ReaderAt, error) {
	return nil, nil
}

func art(id, logical, device, acq, remote string) parse.Artifact {
	return parse.Artifact{
		ID: id, SHA256: "sha-" + id, Logical: logical, Acquisition: acq,
		Source: parse.SourceInfo{Kind: "backup", DeviceID: device, RemotePath: remote},
	}
}

const (
	testContainer = "ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared"
	testRel       = "Media/15550000001@s.whatsapp.net/a/b/IMG-0001.jpg"
	testLogical   = testContainer + "/Message/" + testRel
)

func inputWith(l *lookupFake) *parse.Input {
	return &parse.Input{
		Primary: art("db", testContainer+"/ChatStorage.sqlite", "UDID1", "acq1", "UDID1/aa/db"),
		Lookup:  l,
	}
}

// Two same-source artifacts at one logical path (two captures of one logical file) are counted and never chosen (4I2-R6).
func TestResolveMediaCountsAmbiguousNeverChooses(t *testing.T) {
	l := &lookupFake{arts: []parse.Artifact{
		art("a", testLogical, "UDID1", "acq1", "UDID1/aa/a"),
		art("b", testLogical, "UDID1", "acq1", "UDID1/aa/b"),
	}}
	got, st := resolveMedia(inputWith(l), testContainer, testRel)
	if st != lookupAmbiguous || got.ID != "" {
		t.Errorf("got %q, %d; want no artifact and lookupAmbiguous", got.ID, st)
	}
	if len(l.finds) != 1 || l.finds[0] != testLogical {
		t.Errorf("finds %q, want exactly one, at the container's Message path", l.finds)
	}
}

func TestResolveMediaOutcomes(t *testing.T) {
	same := art("a", testLogical, "UDID1", "acq1", "UDID1/aa/a")
	otherDev := art("b", testLogical, "IOS2", "acq-IOS2", "IOS2/aa/b")
	otherDir := art("c", testLogical, "UDID1", "acq1", "OTHERDIR/aa/c")
	otherPath := art("d", testContainer+"/Message/Media/elsewhere.jpg", "UDID1", "acq1", "UDID1/aa/d")
	otherCont := art("e", "ios:AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared/Message/"+testRel, "UDID1", "acq1", "UDID1/aa/e")
	for _, c := range []struct {
		name string
		arts []parse.Artifact
		want lookup
		id   string
	}{
		{"one same-source file", []parse.Artifact{same}, lookupFound, "a"},
		{"same-source and another device", []parse.Artifact{same, otherDev}, lookupFound, "a"},
		{"no file", nil, lookupMissing, ""},
		{"another device only", []parse.Artifact{otherDev}, lookupOtherSource, ""},
		{"another backup directory only", []parse.Artifact{otherDir}, lookupOtherSource, ""},
		{"a glob match at another path is no match", []parse.Artifact{otherPath}, lookupMissing, ""},
		{"another container's file is no match", []parse.Artifact{otherCont}, lookupMissing, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, st := resolveMedia(inputWith(&lookupFake{arts: c.arts}), testContainer, testRel)
			if st != c.want || got.ID != c.id {
				t.Errorf("got %q, %d; want %q, %d", got.ID, st, c.id, c.want)
			}
		})
	}
}

func TestResolveMediaMakesNoLookupItCannot(t *testing.T) {
	l := &lookupFake{}
	if _, st := resolveMedia(nil, testContainer, testRel); st != lookupNone {
		t.Errorf("nil input: %d", st)
	}
	if _, st := resolveMedia(inputWith(l), "", testRel); st != lookupNone {
		t.Errorf("no container: %d", st)
	}
	if _, st := resolveMedia(&parse.Input{}, testContainer, testRel); st != lookupNone {
		t.Errorf("no Lookuper: %d", st)
	}
	// a glob the case refuses (a backslash in the container) is refused, never read as "no file"
	if _, st := resolveMedia(inputWith(l), `ios:Dom\ain`, testRel); st != lookupRefused {
		t.Errorf("uncompilable glob: %d, want lookupRefused", st)
	}
	if len(l.finds) != 0 {
		t.Errorf("a Find was made: %q", l.finds)
	}
	// a container holding '*' is not a pattern: only the file at exactly that path counts
	star := "ios:HomeDomain/Containers/Shared/AppGroup/*"
	other := art("o", "ios:HomeDomain/Containers/Shared/AppGroup/OTHER/Message/"+testRel, "UDID1", "acq1", "UDID1/aa/o")
	if _, st := resolveMedia(inputWith(&lookupFake{arts: []parse.Artifact{other}}), star, testRel); st != lookupMissing {
		t.Errorf("a container with '*' matched another container: %d", st)
	}
}

func TestSameSource(t *testing.T) {
	snap1, snap2 := &parse.SnapshotInfo{Name: "s", Xid: 1}, &parse.SnapshotInfo{Name: "s", Xid: 2}
	db := art("db", "x", "UDID1", "acq1", "UDID1/aa/db")
	file := func(f func(a *parse.Artifact)) parse.Artifact {
		a := art("f", "y", "UDID1", "acq1", "UDID1/bb/f")
		f(&a)
		return a
	}
	for _, c := range []struct {
		name string
		a    parse.Artifact
		db   parse.Artifact
		want bool
	}{
		{"same", file(func(*parse.Artifact) {}), db, true},
		{"another device", file(func(a *parse.Artifact) { a.Source.DeviceID = "IOS2" }), db, false},
		{"no device", file(func(a *parse.Artifact) { a.Source.DeviceID = "" }), func() parse.Artifact { d := db; d.Source.DeviceID = ""; return d }(), false},
		{"another acquisition", file(func(a *parse.Artifact) { a.Acquisition = "acq2" }), db, false},
		{"no acquisition", file(func(a *parse.Artifact) { a.Acquisition = "" }), func() parse.Artifact { d := db; d.Acquisition = ""; return d }(), false},
		{"another partition", file(func(a *parse.Artifact) { a.Source.Partition = "p2" }), db, false},
		{"snapshot against live", file(func(a *parse.Artifact) { a.Source.Snapshot = snap1 }), db, false},
		{"another snapshot", file(func(a *parse.Artifact) { a.Source.Snapshot = snap2 }), func() parse.Artifact { d := db; d.Source.Snapshot = snap1; return d }(), false},
		{"the same snapshot", file(func(a *parse.Artifact) { a.Source.Snapshot = snap1 }), func() parse.Artifact { d := db; d.Source.Snapshot = snap1; return d }(), true},
		{"another backup directory", file(func(a *parse.Artifact) { a.Source.RemotePath = "OTHER/bb/f" }), db, false},
		{"a backup file against a live file", file(func(a *parse.Artifact) { a.Source.Kind = "file" }), db, false},
		{"a live file against a backup database", file(func(*parse.Artifact) {}), func() parse.Artifact { d := db; d.Source.Kind = "file"; return d }(), false},
		{"two live files", file(func(a *parse.Artifact) { a.Source.Kind = "file" }), func() parse.Artifact { d := db; d.Source.Kind = "file"; return d }(), true},
		{"no backup directory", file(func(a *parse.Artifact) { a.Source.RemotePath = "" }), func() parse.Artifact { d := db; d.Source.RemotePath = ""; return d }(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := sameSource(c.a, c.db); got != c.want {
				t.Errorf("sameSource = %v, want %v", got, c.want)
			}
		})
	}
}
