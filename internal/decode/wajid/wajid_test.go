package wajid

import (
	"errors"
	"strings"
	"testing"
)

func u8(v uint8) *uint8    { return &v }
func u16(v uint16) *uint16 { return &v }

// wajidCases is the table of TestWAJIDParseNeverInvents and the seeds of FuzzWAJID.
var wajidCases = []struct {
	in      string
	user    string
	raw     string // RawUser; empty means equal to user
	server  string
	agent   *uint8
	device  *uint16
	class   Class
	wantErr error
}{
	{in: "15550000001@s.whatsapp.net", user: "15550000001", server: "s.whatsapp.net", class: ClassUser},
	{in: "120363000000000001@g.us", user: "120363000000000001", server: "g.us", class: ClassGroup},
	{in: "status@broadcast", user: "status", server: "broadcast", class: ClassStatus},
	{in: "1234@broadcast", user: "1234", server: "broadcast", class: ClassBroadcast},
	{in: "100000000000001@lid", user: "100000000000001", server: "lid", class: ClassLID},
	{in: "120363000000000099@newsletter", user: "120363000000000099", server: "newsletter", class: ClassNewsletter},
	{in: "15550000001@c.us", user: "15550000001", server: "c.us", class: ClassOther},
	{in: "x@bot", user: "x", server: "bot", class: ClassOther},
	{in: "x@hosted", user: "x", server: "hosted", class: ClassOther},
	{in: "x@hosted.lid", user: "x", server: "hosted.lid", class: ClassOther},
	{in: "15550000001@S.WHATSAPP.NET", user: "15550000001", server: "S.WHATSAPP.NET", class: ClassOther},
	{in: "15550000001.0:2@s.whatsapp.net", user: "15550000001", raw: "15550000001.0:2", server: "s.whatsapp.net", agent: u8(0), device: u16(2), class: ClassUser},
	{in: "142.1:0@lid", user: "142", raw: "142.1:0", server: "lid", agent: u8(1), device: u16(0), class: ClassLID},
	{in: "15550000001:7@s.whatsapp.net", user: "15550000001", raw: "15550000001:7", server: "s.whatsapp.net", device: u16(7), class: ClassUser},
	{in: "a@b@g.us", user: "a@b", server: "g.us", class: ClassGroup},
	{in: "1.5@s.whatsapp.net", user: "1.5", server: "s.whatsapp.net", class: ClassUser}, // a plain user part keeps its dots
	{in: "-1", wantErr: ErrNoServer},
	{in: "", wantErr: ErrNoServer},
	{in: "@g.us", wantErr: ErrEmptyUser},
	{in: "15550000001@", wantErr: ErrEmptyServer},
	{in: "1:x@lid", wantErr: ErrBadSuffix},
	{in: "1:70000@lid", wantErr: ErrBadSuffix},
	{in: "1.300:1@lid", wantErr: ErrBadSuffix},
	{in: "1\x00@lid", wantErr: ErrNotText},
	{in: "1\xff@lid", wantErr: ErrNotText},
}

// TestWAJIDParseNeverInvents pins 4I spec §6: a JID is split as stored, never completed,
// folded or repaired.
func TestWAJIDParseNeverInvents(t *testing.T) {
	for _, c := range wajidCases {
		j, err := Parse(c.in)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("Parse(%q) error = %v, want %v", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		raw := c.raw
		if raw == "" {
			raw = c.user
		}
		if j.User != c.user || j.RawUser != raw || j.Server != c.server || j.Class != c.class {
			t.Errorf("Parse(%q) = %+v, want user %q raw %q server %q class %q", c.in, j, c.user, raw, c.server, c.class)
		}
		if (j.Agent == nil) != (c.agent == nil) || j.Agent != nil && *j.Agent != *c.agent {
			t.Errorf("Parse(%q) agent = %v, want %v", c.in, j.Agent, c.agent)
		}
		if (j.Device == nil) != (c.device == nil) || j.Device != nil && *j.Device != *c.device {
			t.Errorf("Parse(%q) device = %v, want %v", c.in, j.Device, c.device)
		}
		if !strings.Contains(c.in, j.User) || !strings.Contains(c.in, j.Server) {
			t.Errorf("Parse(%q): User or Server is not a substring", c.in)
		}
	}
}

// TestWAJIDLongInputAllocatesNothingProportional: a hostile 1 MiB user part is not copied.
func TestWAJIDLongInputAllocatesNothingProportional(t *testing.T) {
	s := strings.Repeat("1", 1<<20) + "@s.whatsapp.net"
	if _, err := Parse(s); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(10, func() { _, _ = Parse(s) }); n > 1 {
		t.Fatalf("Parse allocated %v times for a 1 MiB input", n)
	}
}

// TestWAJIDServerClassesCiteTwoSources: every server the parser classifies is established by
// at least two distinct research sources (research §7.11).
func TestWAJIDServerClassesCiteTwoSources(t *testing.T) {
	want := map[string]Class{
		"s.whatsapp.net": ClassUser, "g.us": ClassGroup, "broadcast": ClassBroadcast,
		"lid": ClassLID, "newsletter": ClassNewsletter, "status@broadcast": ClassStatus,
	}
	got := map[string]bool{}
	for _, sc := range ServerClasses() {
		got[sc.Server] = true
		if want[sc.Server] != sc.Class {
			t.Errorf("server %q has class %q", sc.Server, sc.Class)
		}
		distinct := map[string]bool{}
		for _, s := range sc.Sources {
			distinct[s] = true
		}
		if len(distinct) < 2 {
			t.Errorf("server %q cites %d distinct sources, want at least 2", sc.Server, len(distinct))
		}
	}
	for s := range want {
		if !got[s] {
			t.Errorf("server %q missing from ServerClasses", s)
		}
	}
}
