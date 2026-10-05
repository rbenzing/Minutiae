package artparse_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
)

func TestLogicalPathNormalization(t *testing.T) {
	long := "/" + strings.Repeat("a", 4096) // 4097 bytes
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"/data/x", "/data/x", true},
		{"//data//x", "/data/x", true},
		{"/data///x/y", "/data/x/y", true},
		{"/data/x/", "", false},
		{"/data/../x", "", false},
		{"/data/./x", "", false},
		{"/..", "", false},
		{"/da\x00ta", "", false},
		{`/data\x`, "", false},
		{"data/x", "", false},
		{"", "", false},
		{"/", "", false},
		{long, "", false},
		{"/" + strings.Repeat("a", 4095), "/" + strings.Repeat("a", 4095), true},
		{"/Data/X", "/Data/X", true}, // case is kept
	} {
		got, ok := artparse.NormalizeAndroidPath(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("NormalizeAndroidPath(%.40q) = %.40q, %v; want %.40q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// nameOf runs the default namers the way discovery does: the first that answers wins.
func nameOf(m evidence.ManifestRecord, env artparse.Env) (artparse.Logical, bool) {
	for _, n := range artparse.DefaultNamers() {
		if l, ok := n.Logical(m, env); ok {
			return l, true
		}
	}
	return artparse.Logical{}, false
}

func extractRec(fsType, fsPath string) evidence.ManifestRecord {
	return evidence.ManifestRecord{ID: "x", Path: "artifacts/d/a/x", Source: evidence.Source{
		Kind: "extract", DeviceID: "d", RemotePath: fsPath,
		Derived: &evidence.Derivation{FSType: fsType, FSPath: fsPath},
	}}
}

func TestDefaultNamersAreDeviceFileThenExtract(t *testing.T) {
	var names []string
	for _, n := range artparse.DefaultNamers() {
		names = append(names, n.Name())
	}
	if got := strings.Join(names, ","); got != "device-file,extract" {
		t.Errorf("DefaultNamers = %s", got)
	}
}

func TestExtractNamerPlatformInference(t *testing.T) {
	for _, tc := range []struct {
		fs, path, want, platform string
	}{
		{"ext2", "/data/a", "android:/data/a", "android"},
		{"ext3", "/data/a", "android:/data/a", "android"},
		{"ext4", "/data/data/com.a/databases/app.db", "android:/data/data/com.a/databases/app.db", "android"},
		{"f2fs", "/data/a", "android:/data/a", "android"},
		{"fat12", "/DCIM/a.jpg", "android:/DCIM/a.jpg", "android"},
		{"fat16", "/a", "android:/a", "android"},
		{"fat32", "/a", "android:/a", "android"},
		{"exfat", "/a", "android:/a", "android"},
		{"ext4", "//data//a", "android:/data/a", "android"},
		{"apfs", "/private/var/mobile/Library/SMS/sms.db", "ios:HomeDomain/Library/SMS/sms.db", "ios"},
		{"hfsplus", "/private/var/mobile/Library/SMS/sms.db", "ios:HomeDomain/Library/SMS/sms.db", "ios"},
		{"hfsx", "/private/var/mobile/Library/x", "ios:HomeDomain/Library/x", "ios"},
		// no logical path
		{"apfs", "/private/var/mobile", "", ""},
		{"apfs", "/private/var/mobile/", "", ""},
		{"apfs", "/private/var/root/x", "", ""},
		{"apfs", "/System/Library/x", "", ""},
		{"hfsplus", "/var/mobile/x", "", ""},
		{"ntfs", "/data/a", "", ""},
		{"", "/data/a", "", ""},
		{"ext4", "/data/../a", "", ""},
		{"ext4", "relative/a", "", ""},
		{"apfs", "/private/var/mobile/../x", "", ""},
	} {
		l, ok := nameOf(extractRec(tc.fs, tc.path), artparse.Env{})
		if tc.want == "" {
			if ok {
				t.Errorf("%s %s: got a logical path %+v, want none", tc.fs, tc.path, l)
			}
			continue
		}
		if !ok || l.Path != tc.want || l.Platform != tc.platform || l.Namer != "extract" {
			t.Errorf("%s %s: got %+v, %v; want %s (%s, extract)", tc.fs, tc.path, l, ok, tc.want, tc.platform)
		}
	}
}

func TestSourceInfoCarriesProvenance(t *testing.T) {
	m := extractRec("ext4", "/data/a")
	m.Source.Partition = "2"
	m.Source.Derived.Encrypted = true
	m.Source.Derived.ParentIncomplete = true
	m.Source.Derived.Snapshot = &evidence.SnapshotRef{Name: "snap1", Xid: 261}
	si := artparse.SourceInfo(m)
	if si.Kind != "extract" || si.DeviceID != "d" || si.RemotePath != "/data/a" || si.Partition != "2" ||
		si.FSType != "ext4" || si.FSPath != "/data/a" || !si.Encrypted || !si.ParentIncomplete ||
		si.Snapshot == nil || si.Snapshot.Name != "snap1" || si.Snapshot.Xid != 261 {
		t.Fatalf("SourceInfo = %+v (snapshot %+v)", si, si.Snapshot)
	}
	// a copy: changing it does not reach the manifest record
	si.Snapshot.Name = "changed"
	if m.Source.Derived.Snapshot.Name != "snap1" {
		t.Error("SourceInfo shares the snapshot reference with the record")
	}
	plain := artparse.SourceInfo(evidence.ManifestRecord{Source: evidence.Source{Kind: "file", DeviceID: "d", RemotePath: "/x", OriginalPath: "o"}})
	if plain.Snapshot != nil || plain.Encrypted || plain.OriginalPath != "o" || plain.FSType != "" {
		t.Errorf("plain SourceInfo = %+v", plain)
	}
}

func TestFilePlatformFromDeviceEvidence(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "droid", "A1", "logical")
	acquireStart(t, c, "phone", "A1", "ios.backup")
	acquireStart(t, c, "both", "A1", "logical")
	acquireStart(t, c, "other", "A1", "serial") // evidence of nothing
	put(t, c, "viaexec", "A1", "info/props", evidence.Source{Kind: "info", RemotePath: "exec:getprop"}, "x")
	put(t, c, "viashell", "A1", "info/props", evidence.Source{Kind: "info", RemotePath: "shell:id"}, "x")
	put(t, c, "vialock", "A1", "info/lockdown", evidence.Source{Kind: "info", RemotePath: "lockdown:GetValue"}, "x")
	put(t, c, "both", "A1", "info/lockdown", evidence.Source{Kind: "info", RemotePath: "lockdown:GetValue"}, "x")

	snap := snapshotOf(t, newHost(t, c))
	want := map[string]string{"droid": "android", "phone": "ios", "viaexec": "android", "viashell": "android", "vialock": "ios"}
	for dev, p := range want {
		if got := snap.Env().DevicePlatform[dev]; got != p {
			t.Errorf("platform of %s = %q, want %q", dev, got, p)
		}
	}
	for _, dev := range []string{"both", "other", "nobody"} {
		if p, ok := snap.Env().DevicePlatform[dev]; ok {
			t.Errorf("device %s must have no platform (conflicting or no evidence), got %q", dev, p)
		}
	}
	file := func(dev, remote string) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: "f", Source: evidence.Source{Kind: "file", DeviceID: dev, RemotePath: remote}}
	}
	env := snap.Env()
	for _, tc := range []struct {
		dev, remote, want, platform string
	}{
		{"droid", "/data/data/a/databases/app.db", "android:/data/data/a/databases/app.db", "android"},
		{"droid", "//data//x", "android:/data/x", "android"},
		{"viaexec", "/sdcard/x", "android:/sdcard/x", "android"},
		{"phone", "/DCIM/100APPLE/a.jpg", "ios:MediaDomain/DCIM/100APPLE/a.jpg", "ios"},
		{"phone", "DCIM/a.jpg", "ios:MediaDomain/DCIM/a.jpg", "ios"},
		// a path that merely looks Android on an iOS device stays iOS
		{"phone", "/data/data/a/databases/app.db", "ios:MediaDomain/data/data/a/databases/app.db", "ios"},
		{"vialock", "/Books/a.epub", "ios:MediaDomain/Books/a.epub", "ios"},
		// no usable platform or path
		{"both", "/data/x", "", ""},
		{"nobody", "/data/data/a/databases/app.db", "", ""},
		{"droid", "/data/../x", "", ""},
		{"phone", "/a/../x", "", ""},
	} {
		l, ok := nameOf(file(tc.dev, tc.remote), env)
		if tc.want == "" {
			if ok {
				t.Errorf("%s %s: got %+v, want none", tc.dev, tc.remote, l)
			}
			continue
		}
		if !ok || l.Path != tc.want || l.Platform != tc.platform || l.Namer != "device-file" {
			t.Errorf("%s %s: got %+v, %v; want %s", tc.dev, tc.remote, l, ok, tc.want)
		}
	}
	if got := artparse.WhyNoLogical(file("nobody", "/x"), env); got != "platform unknown for device nobody" {
		t.Errorf("reason = %q", got)
	}
	if got := artparse.WhyNoLogical(file("both", "/x"), env); got != "platform unknown for device both" {
		t.Errorf("reason for conflicting evidence = %q", got)
	}
}

func TestEligibleAllowlist(t *testing.T) {
	snap := &evidence.SnapshotRef{Name: "s", Xid: 1}
	rec := func(kind string, d *evidence.Derivation) evidence.ManifestRecord {
		return evidence.ManifestRecord{Source: evidence.Source{Kind: kind, Derived: d}}
	}
	live := &evidence.Derivation{FSType: "ext4", FSPath: "/a"}
	snapD := &evidence.Derivation{FSType: "apfs", FSPath: "/a", Snapshot: snap}
	for _, tc := range []struct {
		name string
		m    evidence.ManifestRecord
		incl bool
		want bool
	}{
		{"file", rec("file", nil), false, true},
		{"live extract", rec("extract", live), false, true},
		{"backup", rec("backup", nil), false, true},
		{"snapshot extract excluded", rec("extract", snapD), false, false},
		{"snapshot extract included", rec("extract", snapD), true, true},
		{"extract without derivation", rec("extract", nil), true, false},
		{"recover with derivation", rec("recover", live), true, false},
		{"carve", rec("carve", live), true, false},
		{"empty kind", rec("", nil), true, false},
		{"unknown kind", rec("future-kind", nil), true, false},
	} {
		ok, why := artparse.Eligible(tc.m, tc.incl)
		if ok != tc.want || (!ok && why == "") {
			t.Errorf("%s: Eligible = %v %q, want %v (with a reason when refused)", tc.name, ok, why, tc.want)
		}
	}
	if !errors.Is(artparse.ErrNotParserInput, artparse.ErrNotParserInput) {
		t.Fatal("sentinel")
	}
}
