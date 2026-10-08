package evidence_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

func ip(v int) *int { return &v }

func fullRecovery() evidence.Recovery {
	return evidence.Recovery{
		Class: evidence.ClassJournalBlock, Method: "ext4-journal-inode", Confidence: ip(0),
		Basis: []string{"journal:seq=412:fsblk=1061"}, Assumptions: []string{"contiguous-allocation"},
		Alloc:    evidence.AllocSummary{Free: 8192, Allocated: 1, Unknown: 2, ExcludedRuns: 3, ExcludedBytes: 4},
		Excluded: []evidence.Run{{Offset: 1048576, Length: 4096}}, Scope: "unallocated", Content: "ok",
		Algorithm: evidence.AlgorithmRecover, Params: map[string]string{"min_confidence": "0"},
		Journal: &evidence.JournalRef{Seq: 412, Committed: true, Region: "stale", Revoked: true},
	}
}

// Characterization: the model is plain data, so this passes as soon as the types exist.
func TestRecoveryJSONRoundTrip(t *testing.T) {
	full := fullRecovery()
	b, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"class":"journal-block","method":"ext4-journal-inode","confidence":0,` +
		`"basis":["journal:seq=412:fsblk=1061"],"assumptions":["contiguous-allocation"],` +
		`"alloc":{"free":8192,"allocated":1,"unknown":2,"excluded_runs":3,"excluded_bytes":4},` +
		`"excluded":[{"offset":1048576,"length":4096}],"scope":"unallocated","content":"ok",` +
		`"algorithm":"minutiae-recover/1","params":{"min_confidence":"0"},` +
		`"journal":{"seq":412,"committed":true,"region":"stale","revoked":true}}`
	if string(b) != want {
		t.Fatalf("json:\n got %s\nwant %s", b, want)
	}
	var back evidence.Recovery
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, full) {
		t.Fatalf("round trip: got %+v want %+v", back, full)
	}

	// nil confidence and *0 are different; alloc is always emitted.
	nilConf, _ := json.Marshal(evidence.Recovery{Class: "x", Method: "y"})
	if want := `{"class":"x","method":"y","alloc":{"free":0,"allocated":0,"unknown":0},"algorithm":""}`; string(nilConf) != want {
		t.Fatalf("zero Recovery json %s want %s", nilConf, want)
	}
	zero, _ := json.Marshal(evidence.Recovery{Class: "x", Method: "y", Confidence: ip(0)})
	if !strings.Contains(string(zero), `"confidence":0`) {
		t.Fatalf("*0 confidence not emitted: %s", zero)
	}
	var a, b2 evidence.Recovery
	if err := json.Unmarshal(nilConf, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(zero, &b2); err != nil {
		t.Fatal(err)
	}
	if a.Confidence != nil || b2.Confidence == nil || *b2.Confidence != 0 {
		t.Fatalf("confidence round trip: nil=%v zero=%v", a.Confidence, b2.Confidence)
	}
}

// Characterization: pins today's bytes so adding the field cannot change them.
func TestDerivationJSONUnchangedWithoutRecovery(t *testing.T) {
	extract := evidence.Source{Kind: "extract", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: "p1", ParentSHA256: "abc", Partition: 1, PartitionOffset: 1048576, FSType: "ext4",
		FSPath: "/a.txt", FSID: "nid:5", Mode: 420, Runs: []evidence.Run{{Offset: 4096, Length: 512}},
	}}
	unalloc := evidence.Source{Kind: "unallocated", DeviceID: "dev1", Derived: &evidence.Derivation{
		ParentID: "p1", ParentSHA256: "abc", RunsArtifact: "r1",
	}}
	for _, c := range []struct{ name, want string }{
		{"extract", `{"kind":"extract","device_id":"dev1","derived":{"parent_id":"p1","parent_sha256":"abc","partition":1,"partition_offset":1048576,"fs_type":"ext4","fs_path":"/a.txt","fs_id":"nid:5","mode":420,"runs":[{"offset":4096,"length":512}]}}`},
		{"unallocated", `{"kind":"unallocated","device_id":"dev1","derived":{"parent_id":"p1","parent_sha256":"abc","partition":0,"partition_offset":0,"runs_artifact":"r1"}}`},
	} {
		src := extract
		if c.name == "unallocated" {
			src = unalloc
		}
		b, err := json.Marshal(src)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, b, c.want)
		}
		if strings.Contains(string(b), "recovery") {
			t.Errorf("%s: has a recovery key", c.name)
		}
	}
}

// Passes on arrival: the existing provenance-vs-audit check already compares the
// whole Source, so the new field is covered the moment it exists.
func TestVerifyDetectsRecoveryDifferingFromAudit(t *testing.T) {
	image := bytes.Repeat([]byte("0123456789abcdef"), 512)
	runs := []evidence.Run{{Offset: 1024, Length: 512}}
	rv := func(f func(*evidence.Recovery)) func(*evidence.ManifestRecord) {
		return func(r *evidence.ManifestRecord) { f(r.Source.Derived.Recovery) }
	}
	for _, tc := range []struct {
		name string
		mut  func(*evidence.ManifestRecord)
	}{
		{"method", rv(func(r *evidence.Recovery) { r.Method = "fat-fragmented" })},
		{"confidence", rv(func(r *evidence.Recovery) { r.Confidence = ip(99) })},
		{"confidence removed", rv(func(r *evidence.Recovery) { r.Confidence = nil })},
		{"alloc free", rv(func(r *evidence.Recovery) { r.Alloc.Free++ })},
		{"basis", rv(func(r *evidence.Recovery) { r.Basis = []string{"dirent:9:9"} })},
		{"algorithm", rv(func(r *evidence.Recovery) { r.Algorithm = "minutiae-recover/2" })},
		{"class", rv(func(r *evidence.Recovery) { r.Class = evidence.ClassPostCheckpointFile })},
		{"scope", rv(func(r *evidence.Recovery) { r.Scope = "raw" })},
		{"content", rv(func(r *evidence.Recovery) { r.Content = "uniform" })},
		{"excluded", rv(func(r *evidence.Recovery) { r.Excluded = []evidence.Run{{Offset: 0, Length: 1}} })},
		{"journal", rv(func(r *evidence.Recovery) { r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"} })},
		{"params", rv(func(r *evidence.Recovery) { r.Params = map[string]string{"min_confidence": "0"} })},
		{"recovery removed entirely", func(r *evidence.ManifestRecord) { r.Source.Derived.Recovery = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := evidencetest.NewCase(t)
			parent := evidencetest.AddImage(t, c, image)
			rec := evidencetest.AddRecovered(t, c, parent, image, evidencetest.RecoveredSpec{Runs: runs})
			before, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			evidencetest.RequireNoProblem(t, before, "source differs from audit")
			evidencetest.RewriteManifest(t, c.Dir, rec.ID, tc.mut)
			rep, err := c.Verify()
			if err != nil {
				t.Fatal(err)
			}
			evidencetest.RequireProblem(t, rep, "source differs from audit")
			evidencetest.RequireNoProblem(t, rep, "hash mismatch")
		})
	}
	t.Run("a live artifact gains a recovery description", func(t *testing.T) {
		c := evidencetest.NewCase(t)
		parent := evidencetest.AddImage(t, c, image)
		live, err := c.Capture("dev1", "x1", "p1-mtfs/a.bin", evidence.Source{
			Kind: "extract", DeviceID: "dev1",
			Derived: &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256, Partition: 1, FSType: "mtfs", Runs: runs},
		},
			func(w io.Writer) error { _, err := w.Write(image[1024:1536]); return err })
		if err != nil {
			t.Fatal(err)
		}
		evidencetest.RewriteManifest(t, c.Dir, live.ID, func(r *evidence.ManifestRecord) {
			r.Source.Derived.Recovery = &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "fat-contiguous", Confidence: ip(50), Algorithm: evidence.AlgorithmRecover}
		})
		rep, err := c.Verify()
		if err != nil {
			t.Fatal(err)
		}
		evidencetest.RequireProblem(t, rep, "source differs from audit")
	})
}

func TestRecoveryCheckTable(t *testing.T) {
	good := func(class, method string) evidence.Recovery {
		r := evidence.Recovery{Class: class, Method: method, Confidence: ip(40), Algorithm: evidence.AlgorithmRecover}
		if ci, ok := evidence.LookupClass(class); ok {
			r.Scope = ci.Scope
		}
		return r
	}
	longStr := strings.Repeat("x", 257)
	many := func(n int) []string { return slices.Repeat([]string{"a"}, n) }
	type row struct {
		name string
		kind string
		r    evidence.Recovery
		want []string
	}
	mk := func(name, kind string, r evidence.Recovery, want ...string) row { return row{name, kind, r, want} }
	rows := []row{
		// valid rows
		mk("valid deleted-file", "recover", good("deleted-file", "fat-contiguous")),
		mk("valid deleted-file f2fs-node", "recover", good("deleted-file", "f2fs-node-scan")),
		mk("valid deleted-file ext4-journal-inode", "recover", good("deleted-file", "ext4-journal-inode")),
		mk("valid post-checkpoint", "recover", good("post-checkpoint-file", "f2fs-rollforward")),
		mk("valid carved", "carve", func() evidence.Recovery { r := good("carved", "carve-sig"); r.Scope = "raw"; return r }()),
		mk("valid slack at the ceiling", "slack", func() evidence.Recovery { r := good("slack", "slack-file"); r.Confidence = ip(10); return r }()),
		mk("journal-block without confidence", "journal", func() evidence.Recovery {
			r := good("journal-block", "ext4-journal")
			r.Confidence = nil
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			return r
		}()),
		mk("journal-report without confidence", "report", func() evidence.Recovery {
			r := good("journal-report", "f2fs-rollforward")
			r.Confidence = nil
			return r
		}()),
		mk("deleted-file confidence 0 and 80", "recover", func() evidence.Recovery {
			r := good("deleted-file", "ext-inode-intact")
			r.Confidence = ip(80)
			return r
		}()),
		mk("deleted-file with a journal reference", "recover", func() evidence.Recovery {
			r := good("deleted-file", "ext4-journal-inode")
			r.Journal = &evidence.JournalRef{Seq: 2, Region: "stale"}
			return r
		}()),

		// C25: the scope of slack and journal classes comes from the class table
		mk("slack without scope", "slack", func() evidence.Recovery {
			r := good("slack", "slack-file")
			r.Confidence = ip(10)
			r.Scope = ""
			return r
		}(), `recovery: scope "" does not match "unallocated", which class "slack" requires`),
		mk("slack in scope raw", "slack", func() evidence.Recovery {
			r := good("slack", "slack-file")
			r.Confidence = ip(10)
			r.Scope = "raw"
			return r
		}(), `recovery: scope "raw" does not match "unallocated", which class "slack" requires`),
		mk("journal-block in scope volume", "journal", func() evidence.Recovery {
			r := good("journal-block", "ext4-journal")
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			r.Scope = "volume"
			return r
		}(), `recovery: scope "volume" does not match "unallocated", which class "journal-block" requires`),
		mk("journal-report without scope", "report", func() evidence.Recovery { r := good("journal-report", "ext4-journal"); r.Scope = ""; return r }(), `recovery: scope "" does not match "unallocated", which class "journal-report" requires`),
		mk("post-checkpoint-file refuses a journal reference", "recover", func() evidence.Recovery {
			r := good("post-checkpoint-file", "f2fs-rollforward")
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			return r
		}(), `recovery: journal reference is not allowed for class "post-checkpoint-file"`),
		mk("slack refuses a journal reference", "slack", func() evidence.Recovery {
			r := good("slack", "slack-file")
			r.Confidence = ip(10)
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			return r
		}(), `recovery: journal reference is not allowed for class "slack"`),
		mk("zero-length excluded run", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = []evidence.Run{{Offset: 5, Length: 0}}
			return r
		}(), `recovery: excluded run 0 is not valid (negative, empty, a hole or overflowing)`),
		mk("65 params", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{}
			for i := range 65 {
				r.Params[fmt.Sprintf("key%02d", i)] = "v"
			}
			return r
		}(), `recovery: params has 65 entries (at most 64)`),
		mk("64 params are fine", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{}
			for i := range 64 {
				r.Params[fmt.Sprintf("key%02d", i)] = "v"
			}
			return r
		}()),
		mk("negative excluded_runs", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Alloc.ExcludedRuns = -1
			return r
		}(), `recovery: alloc values must not be negative`),
		mk("negative unknown", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Alloc.Unknown = -1; return r }(), `recovery: alloc values must not be negative`),
		mk("negative allocated", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Alloc.Allocated = -1
			return r
		}(), `recovery: alloc values must not be negative`),

		// R1
		mk("unknown class", "recover", good("bogus", "fat-contiguous"), `recovery: class "bogus" is not known`),
		mk("class and kind mismatch", "carve", good("deleted-file", "fat-contiguous"), `recovery: class "deleted-file" belongs to kind "recover", not "carve"`),
		mk("non-recovered kind", "extract", good("deleted-file", "fat-contiguous"), `recovery: class "deleted-file" belongs to kind "recover", not "extract"`),

		// R2 method
		mk("method not a token", "recover", good("deleted-file", "Fat Contiguous"), `recovery: method "Fat Contiguous" is not a token`),
		mk("empty method", "recover", good("deleted-file", ""), `recovery: method "" is not a token`),
		mk("method prefix not allowed", "recover", good("deleted-file", "zfs-undelete"), `recovery: method "zfs-undelete" is not allowed for class "deleted-file"`),
		mk("rollforward is not a deleted-file method", "recover", good("deleted-file", "f2fs-rollforward"), `recovery: method "f2fs-rollforward" is not allowed for class "deleted-file"`),
		mk("carve method on a slack class", "slack", func() evidence.Recovery { r := good("slack", "carve-sig"); r.Confidence = ip(10); return r }(), `recovery: method "carve-sig" is not allowed for class "slack"`),

		// R2 confidence
		mk("confidence -1", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Confidence = ip(-1); return r }(), `recovery: confidence -1 is outside 0..100`),
		mk("confidence 101", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Confidence = ip(101)
			return r
		}(), `recovery: confidence 101 is outside 0..100`),
		mk("deleted-file above the ceiling", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Confidence = ip(81); return r }(), `recovery: confidence 81 is above the ceiling 80 of class "deleted-file"`),
		mk("carved above the ceiling", "carve", func() evidence.Recovery {
			r := good("carved", "carve-sig")
			r.Scope = "raw"
			r.Confidence = ip(71)
			return r
		}(), `recovery: confidence 71 is above the ceiling 70 of class "carved"`),
		mk("slack above the ceiling", "slack", func() evidence.Recovery { r := good("slack", "slack-file"); r.Confidence = ip(11); return r }(), `recovery: confidence 11 is above the ceiling 10 of class "slack"`),
		mk("deleted-file needs confidence", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Confidence = nil; return r }(), `recovery: confidence is required for class "deleted-file"`),
		mk("post-checkpoint needs confidence", "recover", func() evidence.Recovery {
			r := good("post-checkpoint-file", "f2fs-rollforward")
			r.Confidence = nil
			return r
		}(), `recovery: confidence is required for class "post-checkpoint-file"`),
		mk("carved needs confidence", "carve", func() evidence.Recovery {
			r := good("carved", "carve-sig")
			r.Scope = "raw"
			r.Confidence = nil
			return r
		}(), `recovery: confidence is required for class "carved"`),
		mk("slack needs confidence", "slack", func() evidence.Recovery { r := good("slack", "slack-file"); r.Confidence = nil; return r }(), `recovery: confidence is required for class "slack"`),

		// R2 algorithm, scope, content
		mk("empty algorithm", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Algorithm = ""; return r }(), `recovery: algorithm is empty`),
		mk("bad scope", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Scope = "weird"; return r }(), `recovery: scope "weird" is not one of unallocated, volume, raw, artifact`),
		mk("carved without scope", "carve", good("carved", "carve-sig"), `recovery: scope is required for class "carved"`),
		mk("bad content", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Content = "bad"; return r }(), `recovery: content "bad" is not one of ok, uniform, type-match, type-mismatch`),

		mk("deleted-file takes no scope", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Scope = "raw"; return r }(), `recovery: scope "raw" is not allowed for class "deleted-file" (only a carved artifact takes a chosen scope)`),
		mk("post-checkpoint-file takes no scope", "recover", func() evidence.Recovery {
			r := good("post-checkpoint-file", "f2fs-rollforward")
			r.Scope = "volume"
			return r
		}(), `recovery: scope "volume" is not allowed for class "post-checkpoint-file" (only a carved artifact takes a chosen scope)`),
		mk("carved takes each of the four scopes", "carve", func() evidence.Recovery { r := good("carved", "carve-sig"); r.Scope = "artifact"; return r }()),
		// R2 journal reference
		mk("journal-block needs a reference", "journal", func() evidence.Recovery { r := good("journal-block", "ext4-journal"); r.Confidence = nil; return r }(), `recovery: journal reference is required for class "journal-block"`),
		mk("carved refuses a journal reference", "carve", func() evidence.Recovery {
			r := good("carved", "carve-sig")
			r.Scope = "raw"
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "live"}
			return r
		}(), `recovery: journal reference is not allowed for class "carved"`),
		mk("bad region", "journal", func() evidence.Recovery {
			r := good("journal-block", "ext4-journal")
			r.Journal = &evidence.JournalRef{Seq: 1, Region: "x"}
			return r
		}(), `recovery: journal reference region "x" is not live or stale`),

		// R2 alloc and excluded
		mk("negative alloc", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Alloc.Free = -1; return r }(), `recovery: alloc values must not be negative`),
		mk("negative excluded count", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Alloc.ExcludedBytes = -5
			return r
		}(), `recovery: alloc values must not be negative`),
		mk("257 excluded runs", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = slices.Repeat([]evidence.Run{{Offset: 0, Length: 1}}, 257)
			return r
		}(), `recovery: excluded has 257 runs (at most 256)`),
		mk("256 excluded runs are fine", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = slices.Repeat([]evidence.Run{{Offset: 0, Length: 1}}, 256)
			return r
		}()),
		mk("negative excluded run", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = []evidence.Run{{Offset: 4096, Length: 4096}, {Offset: 5, Length: -1}}
			return r
		}(), `recovery: excluded run 1 is not valid (negative, empty, a hole or overflowing)`),
		mk("hole excluded run", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = []evidence.Run{{Offset: -1, Length: 4096}}
			return r
		}(), `recovery: excluded run 0 is not valid (negative, empty, a hole or overflowing)`),
		mk("overflowing excluded run", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Excluded = []evidence.Run{{Offset: math.MaxInt64, Length: 2}}
			return r
		}(), `recovery: excluded run 0 is not valid (negative, empty, a hole or overflowing)`),

		// R2 strings
		mk("65 basis entries", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Basis = many(65); return r }(), `recovery: basis has 65 entries (at most 64)`),
		mk("64 basis entries are fine", "recover", func() evidence.Recovery { r := good("deleted-file", "fat-contiguous"); r.Basis = many(64); return r }()),
		mk("65 assumptions", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Assumptions = many(65)
			return r
		}(), `recovery: assumptions has 65 entries (at most 64)`),
		mk("invalid UTF-8 in basis", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Basis = []string{"ok", "a\xffb"}
			return r
		}(), `recovery: basis entry 1 is not valid UTF-8`),
		mk("NUL in assumptions", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Assumptions = []string{"a\x00b"}
			return r
		}(), `recovery: assumptions entry 0 holds a NUL`),
		mk("long basis entry", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Basis = []string{longStr}
			return r
		}(), `recovery: basis entry 0 is 257 bytes (at most 256)`),
		mk("params bad key", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{"Bad Key": "v"}
			return r
		}(), `recovery: params key "Bad Key" is not a token`),
		mk("params long value", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{"key": longStr}
			return r
		}(), `recovery: params value of "key" is 257 bytes (at most 256)`),
		mk("params NUL value", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{"key": "a\x00"}
			return r
		}(), `recovery: params value of "key" holds a NUL`),
		mk("params good", "recover", func() evidence.Recovery {
			r := good("deleted-file", "fat-contiguous")
			r.Params = map[string]string{"min_confidence": "0", "keep_uniform": "false"}
			return r
		}()),
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.r.Check(tc.kind)
			if len(tc.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("want no problems, got %q", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestClassTableIsConsistent(t *testing.T) {
	known := map[string]bool{
		evidence.KindRecover: true, evidence.KindCarve: true, evidence.KindSlack: true,
		evidence.KindJournal: true, evidence.KindReport: true,
	}
	infos := evidence.ClassInfos()
	if len(infos) != 6 {
		t.Fatalf("%d classes, want 6", len(infos))
	}
	nsOfKind := map[string]string{}
	kindOfNS := map[string]string{}
	recoverClasses := 0
	for _, ci := range infos {
		if !known[ci.Kind] {
			t.Errorf("class %q names unknown kind %q", ci.Class, ci.Kind)
		}
		if !evidence.IsRecoveredKind(ci.Kind) {
			t.Errorf("IsRecoveredKind(%q) is false", ci.Kind)
		}
		if ns, ok := nsOfKind[ci.Kind]; ok && ns != ci.Namespace {
			t.Errorf("kind %q maps to namespaces %q and %q", ci.Kind, ns, ci.Namespace)
		}
		nsOfKind[ci.Kind] = ci.Namespace
		if k, ok := kindOfNS[ci.Namespace]; ok && k != ci.Kind {
			t.Errorf("namespace %q shared by kinds %q and %q", ci.Namespace, k, ci.Kind)
		}
		kindOfNS[ci.Namespace] = ci.Kind
		if ci.Kind == evidence.KindRecover {
			recoverClasses++
		}
		if len(ci.MethodPrefixes) == 0 {
			t.Errorf("class %q has no method prefixes", ci.Class)
		}
		for _, p := range ci.MethodPrefixes {
			if !strings.HasSuffix(p, "-") {
				p += "x" // a prefix without a trailing dash is a whole token stem
			}
			if r := (&evidence.Recovery{Class: ci.Class, Method: p, Algorithm: "a", Confidence: ip(0)}); slices.ContainsFunc(r.Check(ci.Kind), func(s string) bool { return strings.Contains(s, "is not a token") }) {
				t.Errorf("class %q prefix %q is not token syntax", ci.Class, p)
			}
		}
		if ci.MaxConfidence < 0 || ci.MaxConfidence > 100 {
			t.Errorf("class %q ceiling %d", ci.Class, ci.MaxConfidence)
		}
		back, ok := evidence.LookupClass(ci.Class)
		if !ok || !reflect.DeepEqual(back, ci) {
			t.Errorf("LookupClass(%q) = %+v, %v", ci.Class, back, ok)
		}
	}
	if recoverClasses != 2 {
		t.Errorf("kind recover has %d classes, want 2", recoverClasses)
	}
	if evidence.IsRecoveredKind("extract") || evidence.IsRecoveredKind("") {
		t.Error("extract or empty counted as a recovered kind")
	}
	if _, ok := evidence.LookupClass("nope"); ok {
		t.Error("LookupClass found a class that does not exist")
	}
	// the returned slice is a copy
	infos[0].Class = "mutated"
	infos[0].MethodPrefixes[0] = "mutated"
	again := evidence.ClassInfos()
	if again[0].Class == "mutated" || again[0].MethodPrefixes[0] == "mutated" {
		t.Error("ClassInfos exposes its table")
	}
	// pinned table values
	wantScope := map[string]string{"slack": "unallocated", "journal-block": "unallocated", "journal-report": "unallocated"}
	for _, ci := range again {
		if ci.Scope != wantScope[ci.Class] {
			t.Errorf("class %q scope %q want %q", ci.Class, ci.Scope, wantScope[ci.Class])
		}
	}
	wantCeil := map[string]int{"deleted-file": 80, "post-checkpoint-file": 80, "carved": 70, "slack": 10, "journal-block": 100, "journal-report": 100}
	for _, ci := range again {
		if ci.MaxConfidence != wantCeil[ci.Class] {
			t.Errorf("class %q ceiling %d want %d", ci.Class, ci.MaxConfidence, wantCeil[ci.Class])
		}
	}
}

func TestConfidenceBand(t *testing.T) {
	for _, tc := range []struct {
		c    int
		want string
	}{{0, "low"}, {39, "low"}, {40, "medium"}, {69, "medium"}, {70, "high"}, {100, "high"}, {-1, ""}, {101, ""}} {
		if got := evidence.ConfidenceBand(tc.c); got != tc.want {
			t.Errorf("ConfidenceBand(%d) = %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestRecoveredNamespace(t *testing.T) {
	for _, tc := range []struct {
		path string
		ns   string
		ok   bool
	}{
		{"artifacts/d/a/recovered/p1-fat/000001-a.bin", "recovered", true},
		{"artifacts/d/a/carved/raw/0000000000001000-jpeg.jpg", "carved", true},
		{"artifacts/d/a/slack/p1-fat/slack.bin", "slack", true},
		{"artifacts/d/a/journal/p1-ext4/seq1-fsblk2.bin", "journal", true},
		{"artifacts/d/a/reports/p1/x.jsonl", "reports", true},
		{"artifacts/d/a/recovered/x", "recovered", true},
		{"artifacts/d/a/recovered", "", false},
		{"artifacts/d/a/extract/p1-fat/a.txt", "", false},
		{"artifacts/d/a/Recovered/p1/x", "", false},
		{"files/recovered/x", "", false},
		{"files/d/a/recovered/x", "", false},
		{"artifacts/d/a/x/recovered", "", false},
		{"artifacts/d/recovered/p1/x", "", false},
		{"", "", false},
	} {
		ns, ok := evidence.RecoveredNamespace(tc.path)
		if ns != tc.ns || ok != tc.ok {
			t.Errorf("RecoveredNamespace(%q) = %q, %v; want %q, %v", tc.path, ns, ok, tc.ns, tc.ok)
		}
	}
}

func TestRecoveryTrailOf(t *testing.T) {
	rec := func(c *int) *evidence.Recovery {
		return &evidence.Recovery{Class: evidence.ClassCarved, Method: "carve-x", Confidence: c}
	}
	m := func(id, parent string, rs ...*evidence.Recovery) evidence.ManifestRecord {
		var r *evidence.Recovery
		if len(rs) > 0 {
			r = rs[0]
		}
		d := &evidence.Derivation{ParentID: parent, Recovery: r}
		if parent == "" {
			d = nil
		}
		return evidence.ManifestRecord{ID: id, Source: evidence.Source{Kind: "x", Derived: d}}
	}
	set := func(rs ...evidence.ManifestRecord) map[string]evidence.ManifestRecord {
		out := map[string]evidence.ManifestRecord{}
		for _, r := range rs {
			out[r.ID] = r
		}
		return out
	}
	t.Run("self", func(t *testing.T) {
		r := rec(ip(50))
		tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", r)), "a")
		if !ok || tr.ArtifactID != "a" || tr.Hops != 0 || tr.Recovery != r || tr.MinConfidence == nil || *tr.MinConfidence != 50 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	t.Run("parent", func(t *testing.T) {
		r := rec(ip(50))
		tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", r), m("b", "a", nil)), "b")
		if !ok || tr.ArtifactID != "a" || tr.Hops != 1 || tr.Recovery != r || *tr.MinConfidence != 50 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	t.Run("grandparent", func(t *testing.T) {
		r := rec(ip(60))
		tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", r), m("b", "a", nil), m("c", "b", nil)), "c")
		if !ok || tr.ArtifactID != "a" || tr.Hops != 2 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	t.Run("lowest confidence over the chain", func(t *testing.T) {
		// the nearest carrier says 70, a farther one says 30, the nearest wins ArtifactID but the minimum is 30
		tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", rec(ip(30))), m("b", "a", rec(ip(70))), m("c", "b", nil)), "c")
		if !ok || tr.ArtifactID != "b" || tr.Hops != 1 || tr.MinConfidence == nil || *tr.MinConfidence != 30 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	t.Run("none", func(t *testing.T) {
		if tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", nil)), "a"); ok {
			t.Fatalf("got %+v", tr)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		if _, ok := evidence.RecoveryTrailOf(set(m("img", "")), "zzz"); ok {
			t.Fatal("found")
		}
	})
	t.Run("cycle", func(t *testing.T) {
		if tr, ok := evidence.RecoveryTrailOf(set(m("a", "b", nil), m("b", "a", nil)), "a"); ok {
			t.Fatalf("got %+v", tr)
		}
		// a carrier before the cycle is still found
		tr, ok := evidence.RecoveryTrailOf(set(m("a", "b", rec(ip(5))), m("b", "a", nil)), "b")
		if !ok || tr.ArtifactID != "a" || tr.Hops != 1 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	chain := func(carrierAtHops int) map[string]evidence.ManifestRecord {
		// a0 <- a1 <- ... ; the carrier is carrierAtHops above the start a0
		var rs []evidence.ManifestRecord
		for i := 0; i <= carrierAtHops; i++ {
			var r *evidence.Recovery
			if i == carrierAtHops {
				r = rec(ip(44))
			}
			rs = append(rs, m(fmt.Sprintf("a%d", i), fmt.Sprintf("a%d", i+1), r))
		}
		rs = append(rs, m(fmt.Sprintf("a%d", carrierAtHops+1), ""))
		return set(rs...)
	}
	t.Run("sixteen hops are followed", func(t *testing.T) {
		tr, ok := evidence.RecoveryTrailOf(chain(16), "a0")
		if !ok || tr.Hops != 16 {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
	t.Run("seventeen hops are not", func(t *testing.T) {
		if tr, ok := evidence.RecoveryTrailOf(chain(17), "a0"); ok {
			t.Fatalf("got %+v", tr)
		}
	})
	t.Run("missing parent ends the walk", func(t *testing.T) {
		if tr, ok := evidence.RecoveryTrailOf(set(m("a", "gone", nil)), "a"); ok {
			t.Fatalf("got %+v", tr)
		}
	})
	t.Run("nil confidence", func(t *testing.T) {
		r := rec(nil)
		tr, ok := evidence.RecoveryTrailOf(set(m("img", ""), m("a", "img", r)), "a")
		if !ok || tr.MinConfidence != nil || tr.Recovery != r {
			t.Fatalf("%+v %v", tr, ok)
		}
	})
}

func TestCheckRecoveredRuns(t *testing.T) {
	const path = "artifacts/d/a/recovered/p1-x/000001-a"
	pre := `artifact "a1" ("` + path + `"): `
	mkrec := func(size int64, class, scope string, alloc evidence.AllocSummary) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: "a1", Path: path, Size: size, Source: evidence.Source{Kind: "recover", Derived: &evidence.Derivation{
			Recovery: &evidence.Recovery{Class: class, Scope: scope, Alloc: alloc},
		}}}
	}
	free := func(n int64) evidence.AllocSummary { return evidence.AllocSummary{Free: n} }
	del := evidence.ClassDeletedFile
	r100 := []evidence.Run{{Offset: 4096, Length: 100}}

	big := make([]evidence.Run, evidence.MaxRecoveredRuns+1)
	for i := range big {
		big[i] = evidence.Run{Offset: int64(i), Length: 1}
	}
	inline := mkrec(4097, del, "", free(4097))
	inline.Source.Derived.Runs = make([]evidence.Run, evidence.MaxInlineRuns+1)

	rows := []struct {
		name string
		rec  evidence.ManifestRecord
		runs []evidence.Run
		want []string
	}{
		{"valid", mkrec(100, del, "", free(100)), r100, nil},
		{"valid two runs", mkrec(150, del, "", free(150)), []evidence.Run{{Offset: 0, Length: 100}, {Offset: 500, Length: 50}}, nil},
		{"hole", mkrec(100, del, "", free(100)), []evidence.Run{{Offset: -1, Length: 100}}, []string{pre + `run 0 is a hole (recovered artifacts have no holes)`}},
		{"negative offset", mkrec(100, del, "", free(100)), []evidence.Run{{Offset: -5, Length: 100}}, []string{pre + `run 0 is not valid (negative or overflowing)`}},
		{"negative length", mkrec(100, del, "", free(100)), []evidence.Run{{Offset: 5, Length: -100}}, []string{pre + `run 0 is not valid (negative or overflowing)`}},
		{"overflow", mkrec(100, del, "", free(100)), []evidence.Run{{Offset: math.MaxInt64 - 1, Length: 10}}, []string{pre + `run 0 is not valid (negative or overflowing)`}},
		{"zero length", mkrec(0, del, "", free(0)), []evidence.Run{{Offset: 5, Length: 0}}, []string{pre + `run 0 is empty (zero length)`}},
		{"sum below size", mkrec(200, del, "", free(100)), r100, []string{pre + `runs cover 100 bytes but the artifact holds 200`}},
		{"sum above size", mkrec(50, del, "", free(100)), r100, []string{pre + `runs cover 100 bytes but the artifact holds 50`}},
		{"incomplete with sum above size", func() evidence.ManifestRecord {
			r := mkrec(50, del, "", free(100))
			r.Incomplete, r.Error = true, "read failed"
			return r
		}(), r100, []string{pre + `runs cover 100 bytes but the artifact holds 50`}},
		{"incomplete without error", func() evidence.ManifestRecord {
			r := mkrec(100, del, "", free(100))
			r.Incomplete = true
			return r
		}(), r100, []string{pre + `is flagged incomplete but records no error`}},
		{"incomplete with error and prefix runs", func() evidence.ManifestRecord {
			r := mkrec(100, del, "", evidence.AllocSummary{Free: 100, ExcludedRuns: 1, ExcludedBytes: 20})
			r.Incomplete, r.Error = true, "1 of 2 runs not captured: allocated since deletion"
			return r
		}(), r100, nil},
		{"no runs", mkrec(10, del, "", free(0)), nil, []string{pre + `records no runs`}},
		{"post-checkpoint-file with bytes and no runs", mkrec(10, evidence.ClassPostCheckpointFile, "", free(0)), nil, []string{pre + `records no runs`}},
		{"carved with bytes and no runs", mkrec(10, evidence.ClassCarved, "", free(0)), nil, []string{pre + `records no runs`}},
		{"slack with bytes and no runs", mkrec(10, evidence.ClassSlack, "", free(0)), nil, []string{pre + `records no runs`}},
		{"journal-block with bytes and no runs", mkrec(10, evidence.ClassJournalBlock, "", free(0)), nil, []string{pre + `records no runs`}},
		{"deleted-file of size 0 may have no runs", mkrec(0, del, "", free(0)), nil, nil},
		{"carved of size 0 may have no runs", mkrec(0, evidence.ClassCarved, "", free(0)), nil, nil},
		{"slack of size 0 may have no runs", mkrec(0, evidence.ClassSlack, "", free(0)), nil, nil},
		{"journal-report is derived and needs no runs", mkrec(10, evidence.ClassJournalReport, "", free(0)), nil, nil},
		{"alloc total differs", mkrec(100, del, "", free(50)), r100, []string{pre + `alloc: free+allocated+unknown (50 bytes) does not equal the 100 bytes of the recorded runs`}},
		{"deleted-file with allocated bytes", mkrec(100, del, "", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `40 allocated bytes in a deleted-file artifact`}},
		{"deleted-file with unknown bytes", mkrec(100, del, "", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, []string{pre + `10 unknown bytes in a deleted-file artifact`}},
		{"carved unallocated with allocated bytes", mkrec(100, evidence.ClassCarved, "unallocated", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `scope unallocated but 40 allocated and 0 unknown bytes (class "carved")`}},
		{"carved raw with allocated bytes", mkrec(100, evidence.ClassCarved, "raw", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, nil},
		{"slack with allocated bytes and no scope", mkrec(100, evidence.ClassSlack, "", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `scope unallocated but 40 allocated and 0 unknown bytes (class "slack")`}},
		{"slack with allocated bytes and a stored scope that lies", mkrec(100, evidence.ClassSlack, "raw", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `scope unallocated but 40 allocated and 0 unknown bytes (class "slack")`}},
		{"slack with unknown bytes", mkrec(100, evidence.ClassSlack, "", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, []string{pre + `scope unallocated but 0 allocated and 10 unknown bytes (class "slack")`}},
		{"journal-block with allocated bytes", mkrec(100, evidence.ClassJournalBlock, "", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `scope unallocated but 40 allocated and 0 unknown bytes (class "journal-block")`}},
		{"journal-block with unknown bytes", mkrec(100, evidence.ClassJournalBlock, "", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, []string{pre + `scope unallocated but 0 allocated and 10 unknown bytes (class "journal-block")`}},
		{"journal-report with allocated bytes", mkrec(100, evidence.ClassJournalReport, "", evidence.AllocSummary{Free: 60, Allocated: 40}), r100, []string{pre + `scope unallocated but 40 allocated and 0 unknown bytes (class "journal-report")`}},
		{"journal-report with unknown bytes", mkrec(100, evidence.ClassJournalReport, "", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, []string{pre + `scope unallocated but 0 allocated and 10 unknown bytes (class "journal-report")`}},
		{"carved unallocated with unknown bytes", mkrec(100, evidence.ClassCarved, "unallocated", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, []string{pre + `scope unallocated but 0 allocated and 10 unknown bytes (class "carved")`}},
		{"carved volume with unknown bytes", mkrec(100, evidence.ClassCarved, "volume", evidence.AllocSummary{Free: 90, Unknown: 10}), r100, nil},
		{"slack all free is fine", mkrec(100, evidence.ClassSlack, "", free(100)), r100, nil},
		{"negative length cancelled by the sum", mkrec(10, del, "", free(10)), []evidence.Run{{Offset: 0, Length: -5}, {Offset: 0, Length: 15}}, []string{pre + `run 0 is not valid (negative or overflowing)`}},
		{"lengths overflow the sum", mkrec(1, del, "", free(1)), []evidence.Run{{Offset: 0, Length: math.MaxInt64}, {Offset: 0, Length: 1}}, []string{pre + `run lengths add up to more than 9223372036854775807 bytes`}},
		{"too many runs", mkrec(int64(len(big)), del, "", free(int64(len(big)))), big, []string{pre + `too many runs (1048577, at most 1048576)`}},
		{"too many inline runs", inline, make([]evidence.Run, 0), nil},
	}
	rows[len(rows)-1].runs = []evidence.Run{{Offset: 0, Length: 4097}}
	rows[len(rows)-1].want = []string{pre + `records 4097 inline runs (more than 4096; they belong in a runs sidecar)`}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := evidence.CheckRecoveredRuns(tc.rec, tc.runs)
			if len(tc.want) == 0 && len(got) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("problems:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestDerivedRunsSidecar(t *testing.T) {
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, []byte("img"))
	idx := func() map[string]evidence.ManifestRecord {
		recs, err := c.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]evidence.ManifestRecord{}
		for _, r := range recs {
			m[r.ID] = r
		}
		return m
	}
	sidecar := func(name string, content []byte) evidence.ManifestRecord {
		rec, err := c.Capture("dev1", "s1", name, evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256}}, func(w io.Writer) error {
			_, err := w.Write(content)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	recFor := func(sc string) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: "r", Source: evidence.Source{Kind: "recover", Derived: &evidence.Derivation{ParentID: parent.ID, RunsArtifact: sc}}}
	}
	lines := func(n int) []byte {
		var b bytes.Buffer
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "{\"offset\":%d,\"length\":4096}\n", i*4096)
		}
		return b.Bytes()
	}

	t.Run("inline wins", func(t *testing.T) {
		rec := evidence.ManifestRecord{Source: evidence.Source{Derived: &evidence.Derivation{Runs: []evidence.Run{{Offset: 1, Length: 2}}, RunsArtifact: "ignored"}}}
		got, err := c.DerivedRuns(rec, nil)
		if err != nil || !reflect.DeepEqual(got, []evidence.Run{{Offset: 1, Length: 2}}) {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("5000 runs", func(t *testing.T) {
		sc := sidecar("five.runs.jsonl", lines(5000))
		got, err := c.DerivedRuns(recFor(sc.ID), idx())
		if err != nil || len(got) != 5000 || got[4999] != (evidence.Run{Offset: 4999 * 4096, Length: 4096}) {
			t.Fatalf("%d runs, err %v", len(got), err)
		}
	})
	t.Run("last line without newline", func(t *testing.T) {
		sc := sidecar("nonl.runs.jsonl", []byte("{\"offset\":0,\"length\":1}\n{\"offset\":5,\"length\":1}"))
		got, err := c.DerivedRuns(recFor(sc.ID), idx())
		if err != nil || len(got) != 2 {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("no runs at all", func(t *testing.T) {
		got, err := c.DerivedRuns(evidence.ManifestRecord{Source: evidence.Source{Derived: &evidence.Derivation{}}}, nil)
		if err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
	})
	for _, tc := range []struct {
		name    string
		content []byte
		want    string
	}{
		{"unknown field", []byte("{\"offset\":0,\"length\":1}\n{\"offset\":0,\"length\":1,\"x\":2}\n"), `line 2`},
		{"non-JSON line", []byte("not json\n"), `line 1`},
		{"blank line", []byte("{\"offset\":0,\"length\":1}\n\n"), `line 2`},
		{"two values on a line", []byte("{\"offset\":0,\"length\":1}{\"offset\":0,\"length\":1}\n"), `line 1`},
		{"300-byte line", []byte("{\"offset\":0,\"length\":1,\"pad\":\"" + strings.Repeat("a", 300) + "\"}\n"), `line 1 is longer than 256 bytes`},
		{"too many lines", bytes.Repeat([]byte("{\"offset\":0,\"length\":1}\n"), evidence.MaxRecoveredRuns+1), `more than 1048576 runs`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := sidecar(strings.ReplaceAll(tc.name, " ", "-")+".runs.jsonl", tc.content)
			got, err := c.DerivedRuns(recFor(sc.ID), idx())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %d runs, err %v; want an error containing %q", len(got), err, tc.want)
			}
		})
	}
	t.Run("unknown field names the field", func(t *testing.T) {
		sc := sidecar("uf.runs.jsonl", []byte("{\"offset\":0,\"length\":1,\"zzz\":2}\n"))
		_, err := c.DerivedRuns(recFor(sc.ID), idx())
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("missing sidecar id", func(t *testing.T) {
		_, err := c.DerivedRuns(recFor("nope"), idx())
		if err == nil || !strings.Contains(err.Error(), `runs artifact "nope" is not in the manifest`) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestDerivedRunsSidecarMustBelongToTheArtifact(t *testing.T) {
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, []byte("img"))
	body := []byte("{\"offset\":0,\"length\":1}\n")
	capture := func(name string, src evidence.Source) evidence.ManifestRecord {
		src.DeviceID = "dev1"
		rec, err := c.Capture("dev1", "s1", name, src, func(w io.Writer) error { _, err := w.Write(body); return err })
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	index := func() map[string]evidence.ManifestRecord {
		recs, err := c.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]evidence.ManifestRecord{}
		for _, r := range recs {
			m[r.ID] = r
		}
		return m
	}
	recFor := func(sc string) evidence.ManifestRecord {
		return evidence.ManifestRecord{ID: "r", Source: evidence.Source{Kind: "recover", Derived: &evidence.Derivation{ParentID: parent.ID, RunsArtifact: sc}}}
	}
	own := &evidence.Derivation{ParentID: parent.ID, ParentSHA256: parent.SHA256}
	t.Run("a good sidecar", func(t *testing.T) {
		sc := capture("good.runs.jsonl", evidence.Source{Kind: "runs", Derived: own})
		if got, err := c.DerivedRuns(recFor(sc.ID), index()); err != nil || len(got) != 1 {
			t.Fatalf("%v %v", got, err)
		}
	})
	t.Run("kind is not runs", func(t *testing.T) {
		sc := capture("notruns.bin", evidence.Source{Kind: "import", Derived: own})
		_, err := c.DerivedRuns(recFor(sc.ID), index())
		if err == nil || !strings.Contains(err.Error(), "is not a runs sidecar of parent") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("another parent", func(t *testing.T) {
		sc := capture("other.runs.jsonl", evidence.Source{Kind: "runs", Derived: &evidence.Derivation{ParentID: "someone-else"}})
		_, err := c.DerivedRuns(recFor(sc.ID), index())
		if err == nil || !strings.Contains(err.Error(), "is not a runs sidecar of parent") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("no derivation", func(t *testing.T) {
		sc := capture("nod.runs.jsonl", evidence.Source{Kind: "runs"})
		_, err := c.DerivedRuns(recFor(sc.ID), index())
		if err == nil || !strings.Contains(err.Error(), "is not a runs sidecar of parent") {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("not a regular file", func(t *testing.T) {
		sc := capture("dir.runs.jsonl", evidence.Source{Kind: "runs", Derived: own})
		p := filepath.Join(c.Dir, filepath.FromSlash(sc.Path))
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := c.DerivedRuns(recFor(sc.ID), index())
		if err == nil || !strings.Contains(err.Error(), "is not a regular file") {
			t.Fatalf("err %v", err)
		}
	})
	for _, bad := range []string{"../x.runs.jsonl", "a/../../x.runs.jsonl", "/x.runs.jsonl"} {
		t.Run("path outside the case "+bad, func(t *testing.T) {
			sc := capture("out"+strings.NewReplacer("/", "_", ".", "_").Replace(bad)+".runs.jsonl", evidence.Source{Kind: "runs", Derived: own})
			idx := index()
			r := idx[sc.ID]
			r.Path = bad
			idx[sc.ID] = r
			_, err := c.DerivedRuns(recFor(sc.ID), idx)
			if err == nil || !strings.Contains(err.Error(), "lies outside the case") {
				t.Fatalf("err %v", err)
			}
		})
	}
	t.Run("the index replaces the manifest read", func(t *testing.T) {
		sc := capture("idx.runs.jsonl", evidence.Source{Kind: "runs", Derived: own})
		idx := index()
		if err := os.Remove(filepath.Join(c.Dir, "manifest.jsonl")); err != nil {
			t.Fatal(err)
		}
		if got, err := c.DerivedRuns(recFor(sc.ID), idx); err != nil || len(got) != 1 {
			t.Fatalf("%v %v", got, err)
		}
	})
}

func TestDerivedRunsLineLengthBoundary(t *testing.T) {
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, []byte("img"))
	padded := func(n int) []byte { // a valid run line of exactly n bytes (JSON whitespace pads it)
		line := `{"offset":0,"length":1}`
		return []byte(line + strings.Repeat(" ", n-len(line)) + "\n")
	}
	for _, tc := range []struct {
		n    int
		fail bool
	}{{256, false}, {257, true}} {
		t.Run(fmt.Sprint(tc.n), func(t *testing.T) {
			rec, err := c.Capture("dev1", "s1", fmt.Sprintf("l%d.runs.jsonl", tc.n),
				evidence.Source{Kind: "runs", DeviceID: "dev1", Derived: &evidence.Derivation{ParentID: parent.ID}},
				func(w io.Writer) error { _, err := w.Write(padded(tc.n)); return err })
			if err != nil {
				t.Fatal(err)
			}
			d := &evidence.Derivation{ParentID: parent.ID, RunsArtifact: rec.ID}
			got, err := c.DerivedRuns(evidence.ManifestRecord{Source: evidence.Source{Derived: d}}, nil)
			if tc.fail != (err != nil) {
				t.Fatalf("%d runs, err %v; fail=%v", len(got), err, tc.fail)
			}
			if tc.fail && !strings.Contains(err.Error(), "longer than 256 bytes") {
				t.Fatalf("err %v", err)
			}
		})
	}
}
