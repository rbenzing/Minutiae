package androidmms

import (
	"bytes"
	"testing"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func TestJoinTextBoundsMemoryAndNeverInventsInvalidUTF8AtTheCut(t *testing.T) {
	chunk := bytes.Repeat([]byte("é"), 300<<10/2) // 300 KiB of two-byte characters
	parts := [][]byte{chunk, chunk, chunk, chunk}
	joined, total := joinText(parts)
	if len(joined) > 2*maxBody {
		t.Errorf("joined %d bytes, want at most %d", len(joined), 2*maxBody)
	}
	wantTotal := int64(4*len(chunk) + 3)
	if total != wantTotal {
		t.Errorf("total %d, want %d", total, wantTotal)
	}
	if !utf8.Valid(joined) {
		t.Error("the cut left invalid UTF-8")
	}
	small, tot := joinText([][]byte{[]byte("a"), []byte("b")})
	if string(small) != "a\nb" || tot != 3 {
		t.Errorf("small join %q %d", small, tot)
	}
}

func TestAsciiLowerAndTrim(t *testing.T) {
	if asciiLower([]byte("Text/PLAIN")) != "text/plain" {
		t.Error("asciiLower")
	}
	if got := trimPartialRune([]byte{'a', 0xE2, 0x82}); string(got) != "a" {
		t.Errorf("trim %q", got)
	}
}

// Ruling T42 (the public PduHeaders source): 137 from, 151 to, 129 bcc, 130 cc; any other code, and a code that
// could not be read, is the unknown role "member", never a guess.
func TestAddrRoleFollowsPduHeaders(t *testing.T) {
	for _, c := range []struct {
		code  int64
		known bool
		want  string
	}{
		{137, true, message.RoleFrom}, {151, true, message.RoleTo}, {129, true, message.RoleBCC}, {130, true, message.RoleCC},
		{99, true, message.RoleMember}, {0, true, message.RoleMember}, {-1, true, message.RoleMember},
		{137, false, message.RoleMember}, // the cell is NULL or unreadable: the stored number is not a code
	} {
		if got := addrRole(c.code, c.known); got != c.want {
			t.Errorf("addrRole(%d, %v) = %q, want %q", c.code, c.known, got, c.want)
		}
	}
}

// Rulings T25(b) and T46: an attachment belongs to the message only when device, acquisition, partition and
// snapshot are all present and equal. An empty acquisition is unknown, never "the same".
func TestSameSourceNeedsEveryIdentityPresentAndEqual(t *testing.T) {
	snap := func(name string, xid uint64) *parse.SnapshotInfo { return &parse.SnapshotInfo{Name: name, Xid: xid} }
	db := parse.Artifact{Acquisition: "artifacts/D1/A1", Source: parse.SourceInfo{DeviceID: "D1", Partition: "userdata"}}
	same := func(mod func(*parse.Artifact)) bool {
		a := db
		mod(&a)
		return sameSource(a, db)
	}
	for name, c := range map[string]struct {
		mod  func(*parse.Artifact)
		want bool
	}{
		"identical":                {func(*parse.Artifact) {}, true},
		"another acquisition":      {func(a *parse.Artifact) { a.Acquisition = "artifacts/D1/A2" }, false},
		"empty acquisition":        {func(a *parse.Artifact) { a.Acquisition = "" }, false},
		"another device":           {func(a *parse.Artifact) { a.Source.DeviceID = "D2" }, false},
		"empty device":             {func(a *parse.Artifact) { a.Source.DeviceID = "" }, false},
		"another partition":        {func(a *parse.Artifact) { a.Source.Partition = "system" }, false},
		"a snapshot only on a":     {func(a *parse.Artifact) { a.Source.Snapshot = snap("s", 1) }, false},
		"a different empty device": {func(a *parse.Artifact) { a.Source.DeviceID, a.Acquisition = "", "" }, false},
	} {
		if got := same(c.mod); got != c.want {
			t.Errorf("%s: sameSource = %v, want %v", name, got, c.want)
		}
	}
	// both sides without an acquisition: still unknown, never the same
	if sameSource(parse.Artifact{Source: db.Source}, parse.Artifact{Source: db.Source}) {
		t.Error("two artifacts with an empty acquisition were called the same source")
	}
	// the same snapshot on both sides is the same; different ones are not
	a, b := db, db
	a.Source.Snapshot, b.Source.Snapshot = snap("s", 1), snap("s", 1)
	if !sameSource(a, b) {
		t.Error("the same snapshot on both sides is the same source")
	}
	b.Source.Snapshot = snap("s", 2)
	if sameSource(a, b) {
		t.Error("different snapshots are the same source")
	}
}

func TestExpressibleRefusesWhatAGlobCannotSayExactly(t *testing.T) {
	ok := []string{"/a", "/data/user/0/p/app_parts/PART_1.jpg"}
	bad := []string{"", "a/b", "/a*b", "/a" + string(rune(92)) + "b", "/a" + string(rune(0)) + "b", "/a//b", "/a/./b", "/a/../b", "/a/", "/"}
	for _, p := range ok {
		if !expressible(p) {
			t.Errorf("%q should be expressible", p)
		}
	}
	for _, p := range bad {
		if expressible(p) {
			t.Errorf("%q should not be expressible", p)
		}
	}
	long := ""
	for i := 0; i < 65; i++ {
		long += "/d"
	}
	if expressible(long) || !expressible(long[:128]) {
		t.Error("the 64 element cap is not enforced exactly")
	}
}
