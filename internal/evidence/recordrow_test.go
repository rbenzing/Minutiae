package evidence

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

const testArtifactSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// fullRow is a row with every field set and two times given out of order. Each
// call returns fresh pointers so a test may mutate its copy.
func fullRow() RecordRow {
	return RecordRow{
		ID: 42, Type: "message", PayloadV: 2, ArtifactID: "art-1",
		SourcePath: ptr("/data/x.db"), Locator: ptr("table:msg#42"),
		SrcOffset: ptr(int64(4096)), SrcLength: ptr(int64(128)),
		TS: ptr(int64(1700000000)), TSEnd: ptr(int64(1700000060)),
		TSBasis: ptr("local-offset"), TZOffsetMin: ptr(int64(-330)),
		Deleted: true, Recovered: true, RecoveryMethod: ptr("carve"), Confidence: ptr(int64(87)),
		ParserName: "sms", ParserVersion: "1.2.0", ParserHash: ptr("abc123"),
		Summary: strings.Repeat("s", 200), Body: ptr("hi"), Payload: `{"a":1}`,
		Times: []RecordTime{
			{Kind: "mtime", TS: 1700000100, Basis: "utc"},
			{Kind: "atime", TS: -5, Basis: "local-offset", TZOffsetMin: ptr(int64(60))},
		},
	}
}

// TestRowDigestGoldenVector freezes the row encoding. The expected bytes are
// written out by hand from the format reference ("Canonical row digest"): a
// field is 0x00 (NULL) or 0x01 || uvarint(len) || bytes, integers are decimal
// ASCII, booleans "0"/"1". If code and this derivation disagree the code is
// wrong, unless the rule is. Changing either needs a new prefix.
func TestRowDigestGoldenVector(t *testing.T) {
	sha := "\x01\x40" + testArtifactSHA

	t.Run("all nullable fields NULL, no times", func(t *testing.T) {
		r := RecordRow{
			ID: 1, Type: "t", PayloadV: 1, ArtifactID: "a",
			ParserName: "p", ParserVersion: "1", Payload: "{}",
		}
		want := "\x01\x011" + // 1 id
			"\x01\x01t" + // 2 type
			"\x01\x011" + // 3 payload_v
			"\x01\x01a" + // 4 artifact_id
			sha + // 5 artifact_sha256
			"\x00\x00\x00\x00\x00\x00\x00\x00" + // 6-13 source_path locator src_offset src_length ts ts_end ts_basis tz_offset_min
			"\x01\x010" + // 14 deleted
			"\x01\x010" + // 15 recovered
			"\x00" + // 16 recovery_method
			"\x00" + // 17 confidence
			"\x01\x01p" + // 18 parser name
			"\x01\x011" + // 19 parser version
			"\x00" + // 20 parser hash
			"\x01\x00" + // 21 summary: empty but present
			"\x00" + // 22 body
			"\x01\x02{}" + // 23 payload
			"\x01\x010" // 24 count of times
		checkGolden(t, r, want, "10a008a707b3b7715b1c726435396e67e2296e8d417617c457500e4b31aa4b29")
	})

	t.Run("every field set, times out of order", func(t *testing.T) {
		want := "\x01\x0242" + // 1 id
			"\x01\x07message" + // 2 type
			"\x01\x012" + // 3 payload_v
			"\x01\x05art-1" + // 4 artifact_id
			sha + // 5 artifact_sha256
			"\x01\x0a/data/x.db" + // 6 source_path
			"\x01\x0ctable:msg#42" + // 7 locator
			"\x01\x044096" + // 8 src_offset
			"\x01\x03128" + // 9 src_length
			"\x01\x0a1700000000" + // 10 ts
			"\x01\x0a1700000060" + // 11 ts_end
			"\x01\x0clocal-offset" + // 12 ts_basis
			"\x01\x04-330" + // 13 tz_offset_min
			"\x01\x011" + // 14 deleted
			"\x01\x011" + // 15 recovered
			"\x01\x05carve" + // 16 recovery_method
			"\x01\x0287" + // 17 confidence
			"\x01\x03sms" + // 18 parser name
			"\x01\x051.2.0" + // 19 parser version
			"\x01\x06abc123" + // 20 parser hash
			"\x01\xc8\x01" + strings.Repeat("s", 200) + // 21 summary: 200 bytes, two-byte uvarint length
			"\x01\x02hi" + // 22 body
			"\x01\x07{\"a\":1}" + // 23 payload
			"\x01\x012" + // 24 count of times
			// times sorted by kind: atime, then mtime
			"\x01\x05atime" + "\x01\x02-5" + "\x01\x0clocal-offset" + "\x01\x0260" +
			"\x01\x05mtime" + "\x01\x0a1700000100" + "\x01\x03utc" + "\x00"
		checkGolden(t, fullRow(), want, "f0f8850ba3fbaaa4d62ec0f8534ed829d1df297c7f1f5e175a9a487abde7dc08")
	})
}

func checkGolden(t *testing.T, r RecordRow, wantEncoding, pinnedHex string) {
	t.Helper()
	sum := sha256.Sum256([]byte(wantEncoding))
	if got := hex.EncodeToString(sum[:]); got != pinnedHex {
		t.Errorf("SHA-256 of the hand-built encoding = %s, pinned %s", got, pinnedHex)
	}
	if got := RowDigest(r, testArtifactSHA); got != sum {
		t.Errorf("RowDigest = %x, want %x (SHA-256 of the hand-built encoding)", got, sum)
	}
	if got := string(encodeRow(r, testArtifactSHA)); got != wantEncoding {
		t.Errorf("encoding differs:\n got %q\nwant %q", got, wantEncoding)
	}
}

func TestRowDigestEveryFieldChangesDigest(t *testing.T) {
	base := RowDigest(fullRow(), testArtifactSHA)
	mutations := map[string]func(r *RecordRow){
		"1 id":              func(r *RecordRow) { r.ID++ },
		"2 type":            func(r *RecordRow) { r.Type += "x" },
		"3 payload_v":       func(r *RecordRow) { r.PayloadV++ },
		"4 artifact_id":     func(r *RecordRow) { r.ArtifactID += "x" },
		"6 source_path":     func(r *RecordRow) { r.SourcePath = ptr("/data/y.db") },
		"7 locator":         func(r *RecordRow) { r.Locator = ptr("table:msg#43") },
		"8 src_offset":      func(r *RecordRow) { r.SrcOffset = ptr(int64(4097)) },
		"9 src_length":      func(r *RecordRow) { r.SrcLength = ptr(int64(129)) },
		"10 ts":             func(r *RecordRow) { r.TS = ptr(int64(1700000001)) },
		"11 ts_end":         func(r *RecordRow) { r.TSEnd = ptr(int64(1700000061)) },
		"12 ts_basis":       func(r *RecordRow) { r.TSBasis = ptr("utc") },
		"13 tz_offset_min":  func(r *RecordRow) { r.TZOffsetMin = ptr(int64(-331)) },
		"14 deleted":        func(r *RecordRow) { r.Deleted = false },
		"15 recovered":      func(r *RecordRow) { r.Recovered = false },
		"16 recovery":       func(r *RecordRow) { r.RecoveryMethod = ptr("slack") },
		"17 confidence":     func(r *RecordRow) { r.Confidence = ptr(int64(88)) },
		"18 parser name":    func(r *RecordRow) { r.ParserName = "mms" },
		"19 parser version": func(r *RecordRow) { r.ParserVersion = "1.2.1" },
		"20 parser hash":    func(r *RecordRow) { r.ParserHash = ptr("abc124") },
		"21 summary":        func(r *RecordRow) { r.Summary += "." },
		"22 body":           func(r *RecordRow) { r.Body = ptr("ho") },
		"23 payload":        func(r *RecordRow) { r.Payload = `{"a":2}` },
		"24 times count: one more": func(r *RecordRow) {
			r.Times = append(r.Times, RecordTime{Kind: "ctime", TS: 1, Basis: "utc"})
		},
		"24 times count: one fewer": func(r *RecordRow) { r.Times = r.Times[:1] },
		"24 times count: none":      func(r *RecordRow) { r.Times = nil },
		"time kind":                 func(r *RecordRow) { r.Times[0].Kind = "mtimf" },
		"time ts":                   func(r *RecordRow) { r.Times[0].TS++ },
		"time basis":                func(r *RecordRow) { r.Times[0].Basis = "local-unknown" },
		"time tz_offset_min":        func(r *RecordRow) { r.Times[1].TZOffsetMin = ptr(int64(61)) },
		"time tz_offset_min NULL":   func(r *RecordRow) { r.Times[1].TZOffsetMin = nil },
	}
	seen := map[[32]byte]string{base: "base"}
	for name, mutate := range mutations {
		r := fullRow()
		mutate(&r)
		d := RowDigest(r, testArtifactSHA)
		if prev, dup := seen[d]; dup {
			t.Errorf("mutation %q gives the same digest as %q", name, prev)
		}
		seen[d] = name
	}
	if d := RowDigest(fullRow(), strings.Replace(testArtifactSHA, "0", "1", 1)); d == base {
		t.Error("5 artifact_sha256 does not change the digest")
	}
}

func TestRowDigestNullDiffersFromEmpty(t *testing.T) {
	strFields := map[string]func(r *RecordRow, v *string){
		"source_path":     func(r *RecordRow, v *string) { r.SourcePath = v },
		"locator":         func(r *RecordRow, v *string) { r.Locator = v },
		"ts_basis":        func(r *RecordRow, v *string) { r.TSBasis = v },
		"recovery_method": func(r *RecordRow, v *string) { r.RecoveryMethod = v },
		"parser_hash":     func(r *RecordRow, v *string) { r.ParserHash = v },
		"body":            func(r *RecordRow, v *string) { r.Body = v },
	}
	for name, set := range strFields {
		var ds [3][32]byte
		for i, v := range []*string{nil, ptr(""), ptr("0")} {
			r := fullRow()
			set(&r, v)
			ds[i] = RowDigest(r, testArtifactSHA)
		}
		if ds[0] == ds[1] || ds[0] == ds[2] || ds[1] == ds[2] {
			t.Errorf("%s: NULL, \"\" and \"0\" are not all distinct", name)
		}
	}
	intFields := map[string]func(r *RecordRow, v *int64){
		"src_offset":    func(r *RecordRow, v *int64) { r.SrcOffset = v },
		"src_length":    func(r *RecordRow, v *int64) { r.SrcLength = v },
		"ts":            func(r *RecordRow, v *int64) { r.TS = v },
		"ts_end":        func(r *RecordRow, v *int64) { r.TSEnd = v },
		"tz_offset_min": func(r *RecordRow, v *int64) { r.TZOffsetMin = v },
		"confidence":    func(r *RecordRow, v *int64) { r.Confidence = v },
		"time 1 tz":     func(r *RecordRow, v *int64) { r.Times[1].TZOffsetMin = v },
		"time 0 tz":     func(r *RecordRow, v *int64) { r.Times[0].TZOffsetMin = v },
	}
	for name, set := range intFields {
		r1, r2 := fullRow(), fullRow()
		set(&r1, nil)
		set(&r2, ptr(int64(0)))
		if RowDigest(r1, testArtifactSHA) == RowDigest(r2, testArtifactSHA) {
			t.Errorf("%s: NULL and 0 give the same digest", name)
		}
	}
}

func TestRowDigestTimesOrderIndependent(t *testing.T) {
	times := []RecordTime{
		{Kind: "ctime", TS: 3, Basis: "utc"},
		{Kind: "atime", TS: 1, Basis: "local-unknown"},
		{Kind: "mtime", TS: 2, Basis: "local-offset", TZOffsetMin: ptr(int64(-60))},
		{Kind: "Btime", TS: 4, Basis: "utc"}, // byte order: upper case sorts before lower case
	}
	perms := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}}
	var first [32]byte
	for i, p := range perms {
		r := fullRow()
		r.Times = nil
		for _, j := range p {
			r.Times = append(r.Times, times[j])
		}
		order := append([]RecordTime(nil), r.Times...)
		d := RowDigest(r, testArtifactSHA)
		if i == 0 {
			first = d
		} else if d != first {
			t.Errorf("permutation %v changes the digest", p)
		}
		for k := range order {
			if r.Times[k].Kind != order[k].Kind {
				t.Fatal("RowDigest reordered the caller's Times")
			}
		}
	}
	// the encoding sorts by kind in byte order
	r := fullRow()
	r.Times = []RecordTime{times[0], times[1], times[3]}
	enc := string(encodeRow(r, testArtifactSHA))
	iB, iA, iC := strings.Index(enc, "Btime"), strings.Index(enc, "atime"), strings.Index(enc, "ctime")
	if iB >= iA || iA >= iC {
		t.Errorf("times not in byte order of kind: Btime@%d atime@%d ctime@%d", iB, iA, iC)
	}
}

func TestRowDigestBindsArtifactSHA256(t *testing.T) {
	a := RowDigest(fullRow(), strings.Repeat("a", 64))
	b := RowDigest(fullRow(), strings.Repeat("b", 64))
	if a == b {
		t.Fatal("the digest does not depend on the artifact's SHA-256")
	}
	if a != RowDigest(fullRow(), strings.Repeat("a", 64)) {
		t.Fatal("the digest is not deterministic")
	}
}

func TestBatchDigestGoldenAndOrderSensitive(t *testing.T) {
	var d1, d2 [32]byte
	for i := range d1 {
		d1[i] = byte(i + 1)
		d2[i] = byte(0xa0 + i)
	}
	hexOf := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	prefix := "minutiae-records-v1\n"
	if DigestPrefix != prefix {
		t.Fatalf("DigestPrefix = %q", DigestPrefix)
	}

	empty := NewBatchDigest()
	if got, want := empty.Sum(), hexOf(prefix); got != want {
		t.Errorf("empty batch = %s, want SHA-256 of the prefix %s", got, want)
	}

	// each row digest is framed by uvarint(32) = 0x20
	b := NewBatchDigest()
	b.Add(d1)
	b.Add(d2)
	want := hexOf(prefix + "\x20" + string(d1[:]) + "\x20" + string(d2[:]))
	if got := b.Sum(); got != want {
		t.Errorf("two-row batch = %s, want %s", got, want)
	}
	if b.Sum() != want {
		t.Error("Sum changed the state")
	}
	if want != "ca32494ba726c1be3cec653046ded54349fee4aee248fb9a77d560b831623e7d" {
		t.Errorf("two-row batch digest = %s, pinned value differs", want)
	}

	swapped := NewBatchDigest()
	swapped.Add(d2)
	swapped.Add(d1)
	if swapped.Sum() == want {
		t.Error("the batch digest does not depend on row order")
	}
	if unframed := hexOf(prefix + string(d1[:]) + string(d2[:])); unframed == want {
		t.Error("length framing has no effect")
	}
	one := NewBatchDigest()
	one.Add(d1)
	if one.Sum() == want {
		t.Error("a one-row batch equals a two-row batch")
	}
}

func TestIngestRollup(t *testing.T) {
	hexOf := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	prefix := "minutiae-records-rollup-v1\n"
	if got, want := IngestRollup(nil), hexOf(prefix); got != want {
		t.Errorf("rollup of zero batches = %s, want the hash of the prefix %s", got, want)
	}
	// each digest is framed by uvarint(len(hex)) and taken in the order given
	want := hexOf(prefix + "\x02aa" + "\x03bbb")
	if got := IngestRollup([]string{"aa", "bbb"}); got != want {
		t.Errorf("rollup = %s, want %s", got, want)
	}
	if want != "fb0100b6444800aa975a706b17582d3d72d20a397732df14a305cc9d24a6319f" {
		t.Errorf("rollup = %s, pinned value differs", want)
	}
	if IngestRollup([]string{"bbb", "aa"}) == want {
		t.Error("the rollup does not depend on batch order")
	}
	if IngestRollup([]string{"ab", "c"}) == IngestRollup([]string{"a", "bc"}) {
		t.Error("the rollup is not length framed")
	}
	if IngestRollup([]string{"a"}) == IngestRollup([]string{"a", ""}) {
		t.Error("an empty trailing digest is ignored")
	}
}
