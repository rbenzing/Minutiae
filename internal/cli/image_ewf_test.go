package cli

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // format-stored hashes, not a security control
	"crypto/sha1" //nolint:gosec // format-stored hashes, not a security control
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// ewfEnv is a case holding one 3-segment E01 image built around the MTFS disk.
type ewfEnv struct {
	d      Deps
	c      string
	recs   []evidence.ManifestRecord
	media  []byte
	chunks int // number of chunks in the image
	md5    string
	sha1   string
}

const ewfChunkBytes = 8 * 512

// newEWFEnv pads the MTFS disk to a whole number of chunks, a multiple of three,
// builds a 3-segment E01 from it with o, and imports it with `image import`.
func newEWFEnv(t *testing.T, o ewftest.Options) *ewfEnv {
	t.Helper()
	media := imgDisk(defaultNodes()...)
	n := (len(media) + ewfChunkBytes - 1) / ewfChunkBytes
	n = (n + 2) / 3 * 3
	media = append(media, make([]byte, n*ewfChunkBytes-len(media))...)
	o.SectorsPerChunk, o.ChunksPerSegment = 8, n/3
	if o.Compress == ewftest.CompressNone {
		o.Compress = ewftest.CompressMixed
	}

	dir := t.TempDir()
	segs := ewftest.Build(o, media)
	if len(segs) != 3 {
		t.Fatalf("built %d segments, want 3", len(segs))
	}
	args := []string{"image", "import", "--case", "", "--json"}
	for i, s := range segs {
		p := filepath.Join(dir, "disk.E0"+string(rune('1'+i)))
		if err := os.WriteFile(p, s, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	e := &ewfEnv{d: Deps{FSDrivers: mtfsDrivers}, c: newCLICase(t), media: media, chunks: n}
	args[3] = e.c
	code, out := run(t, e.d, args...)
	if code != 0 {
		t.Fatalf("image import: %d %s", code, out)
	}
	if err := json.Unmarshal(jsonPart(out), &e.recs); err != nil || len(e.recs) != 3 {
		t.Fatalf("import json %q: %v", out, err)
	}
	m, s := md5.Sum(media), sha1.Sum(media) //nolint:gosec // format-stored hashes
	e.md5, e.sha1 = hex.EncodeToString(m[:]), hex.EncodeToString(s[:])
	return e
}

// runSplit is run with stdout and stderr captured apart.
func runSplit(ctx context.Context, t *testing.T, d Deps, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	d.Out, d.Err = &out, &errb
	if d.In == nil {
		d.In = strings.NewReader("")
	}
	if d.Registry == nil {
		d.Registry = device.NewRegistry()
	}
	root := newRootCmd(d)
	root.SetArgs(args)
	err := root.ExecuteContext(ctx)
	if err != nil {
		errb.WriteString("error: " + err.Error() + "\n")
	}
	return ExitCode(err), out.String(), errb.String()
}

func (e *ewfEnv) info(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	full := append([]string{"image", "info", "--case", e.c, e.recs[0].ID}, args...)
	return runSplit(context.Background(), t, e.d, full...)
}

func (e *ewfEnv) audit(t *testing.T) []evidence.AuditEntry {
	t.Helper()
	return auditOf(t, e.c)
}

// newEntries returns the audit entries appended after the first n, leaving out
// "case.open": every command opens the case, which the audit log records.
func newEntries(all []evidence.AuditEntry, n int) []evidence.AuditEntry {
	var out []evidence.AuditEntry
	for _, a := range all[n:] {
		if a.Action != "case.open" {
			out = append(out, a)
		}
	}
	return out
}

func (e *ewfEnv) verifyEntries(t *testing.T) []evidence.AuditEntry {
	t.Helper()
	var out []evidence.AuditEntry
	for _, a := range e.audit(t) {
		if a.Action == "image.verify" {
			out = append(out, a)
		}
	}
	return out
}

func (e *ewfEnv) caseVerifyOK(t *testing.T) {
	t.Helper()
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
}

func hashDetail(t *testing.T, d map[string]any, key string) map[string]any {
	t.Helper()
	m, ok := d[key].(map[string]any)
	if !ok {
		t.Fatalf("details[%q] = %#v", key, d[key])
	}
	return m
}

func TestImageVerifyAuditsResult(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	before := len(e.audit(t))

	code, out, errs := e.info(t, "--verify")
	if code != 0 {
		t.Fatalf("info --verify: %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	for _, want := range []string{
		"Format:", "ewf",
		"Verify:       MD5", "stored " + e.md5, "computed " + e.md5,
		"Verify:       SHA-1", "stored " + e.sha1, "computed " + e.sha1,
		"Verify:       result  match (" + itoa(len(e.media)) + " bytes hashed)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, " match") != 3 {
		t.Errorf("want three match words (md5, sha1, result):\n%s", out)
	}
	if !strings.Contains(errs, "verify:") {
		t.Errorf("no progress on stderr: %q", errs)
	}

	added := newEntries(e.audit(t), before)
	if len(added) != 1 {
		t.Fatalf("the command appended %d entries besides case.open, want exactly one: %+v", len(added), added)
	}
	ent := added[0]
	if ent.Action != "image.verify" || ent.DeviceID != e.recs[0].Source.DeviceID || ent.DeviceID != "import" {
		t.Fatalf("entry %+v", ent)
	}
	d := ent.Details
	if d["parent_id"] != e.recs[0].ID || d["result"] != "match" || d["format"] != "ewf" {
		t.Fatalf("details %v", d)
	}
	segs, ok := d["segments"].([]any)
	if !ok || len(segs) != 3 {
		t.Fatalf("segments %#v", d["segments"])
	}
	for i, sg := range segs {
		m, _ := sg.(map[string]any)
		if m["id"] != e.recs[i].ID || m["sha256"] != e.recs[i].SHA256 {
			t.Fatalf("segment %d = %v", i, sg)
		}
	}
	for key, want := range map[string]string{"md5": e.md5, "sha1": e.sha1} {
		h := hashDetail(t, d, key)
		if h["stored"] != want || h["computed"] != want || h["status"] != "match" {
			t.Fatalf("%s = %v, want %s", key, h, want)
		}
	}
	e.caseVerifyOK(t)
}

func TestImageVerifyMismatchExits4(t *testing.T) {
	probe := newEWFEnv(t, ewftest.Options{}) // only to learn the media's md5
	flipped, _ := hex.DecodeString(probe.md5)
	flipped[0] ^= 1
	e := newEWFEnv(t, ewftest.Options{MD5Override: flipped})

	code, out, errs := e.info(t, "--verify")
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitIntegrity, out, errs)
	}
	for _, want := range []string{"stored " + hex.EncodeToString(flipped), "computed " + e.md5, "mismatch", "result  mismatch"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q (the result is printed before the error):\n%s", want, out)
		}
	}
	if !strings.Contains(errs, "integrity check failed") || !strings.Contains(errs, "MD5") {
		t.Errorf("stderr lacks the error: %q", errs)
	}
	es := e.verifyEntries(t)
	if len(es) != 1 || es[0].Details["result"] != "mismatch" {
		t.Fatalf("%+v", es)
	}
	h := hashDetail(t, es[0].Details, "md5")
	if h["stored"] != hex.EncodeToString(flipped) || h["computed"] != e.md5 || h["status"] != "mismatch" {
		t.Fatalf("md5 = %v", h)
	}
	if sh := hashDetail(t, es[0].Details, "sha1"); sh["status"] != "match" {
		t.Fatalf("sha1 = %v", sh)
	}
	e.caseVerifyOK(t)
}

func TestImageVerifyUnreadableChunkExits1(t *testing.T) {
	// Chunk 6 lies in the zero gap between the filesystem and the backup GPT, so
	// the image still opens (reading the partition table touches chunks 0-5 and
	// 7-11); its stored bytes are the right length with a wrong Adler-32.
	const last = 6
	bad := bytes.Repeat([]byte{0x55}, ewfChunkBytes+4)
	e := newEWFEnv(t, ewftest.Options{Override: map[int]ewftest.RawChunk{last: {Data: bad}}})

	code, out, errs := e.info(t, "--verify")
	if code != ExitError {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, out, errs)
	}
	if !strings.Contains(out, "Verify:       first unreadable chunk "+itoa(last)+": ") ||
		!strings.Contains(out, "Verify:       result  unverified") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errs)
	}
	es := e.verifyEntries(t)
	if len(es) != 1 {
		t.Fatalf("%d image.verify entries", len(es))
	}
	d := es[0].Details
	if d["result"] != "unverified" || numOf(d["first_bad_chunk"]) != last {
		t.Fatalf("details %v", d)
	}
	for _, key := range []string{"md5", "sha1"} {
		if h := hashDetail(t, d, key); h["status"] != "unverified" || h["computed"] != "" {
			t.Fatalf("%s = %v", key, h)
		}
	}
	e.caseVerifyOK(t)
}

func TestImageVerifyCancelledExits1AndIsAudited(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	full := []string{"image", "info", "--case", e.c, e.recs[0].ID, "--verify"}
	code, out, errs := runSplit(ctx, t, e.d, full...)
	if code != ExitError {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, out, errs)
	}
	if !strings.Contains(out, "result  cancelled") {
		t.Errorf("stdout lacks the cancelled result:\n%s", out)
	}
	es := e.verifyEntries(t)
	if len(es) != 1 || es[0].Details["result"] != "cancelled" {
		t.Fatalf("%+v", es)
	}
	if h := hashDetail(t, es[0].Details, "md5"); h["status"] != "unverified" || h["computed"] != "" {
		t.Fatalf("md5 = %v", h)
	}
	e.caseVerifyOK(t)
}

func TestImageInfoWithoutVerifyAppendsNothing(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	before := len(e.audit(t))
	code, out, errs := e.info(t)
	if code != 0 {
		t.Fatalf("info: %d %s %s", code, out, errs)
	}
	if strings.Contains(out, "Verify:") {
		t.Errorf("plain info prints verification:\n%s", out)
	}
	if added := newEntries(e.audit(t), before); len(added) != 0 {
		t.Fatalf("plain info appended %+v", added)
	}
}

func TestImageVerifyRawAppendsNothing(t *testing.T) {
	e := newImgEnv(t)
	before := len(auditOf(t, e.c))
	code, out, errs := runSplit(context.Background(), t, e.d, "image", "info", "--case", e.c, e.ref, "--verify")
	if code != 0 || !strings.Contains(out, "container has no stored hashes") || !strings.Contains(out, "raw images carry none") {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if strings.Contains(out, "Verify:") {
		t.Errorf("raw image prints a verification:\n%s", out)
	}
	if added := newEntries(auditOf(t, e.c), before); len(added) != 0 {
		t.Fatalf("a raw image appended %+v", added)
	}
}

func TestImageInfoVerifyJSON(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	code, out, errs := e.info(t, "--verify", "--json")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
	}
	v, ok := doc["verify"].(map[string]any)
	if !ok {
		t.Fatalf("no verify object: %v", doc)
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"bytes_hashed", "md5", "result", "sha1", "size"}; !slices.Equal(keys, want) {
		t.Fatalf("verify keys %v, want %v", keys, want)
	}
	n := float64(len(e.media))
	if v["result"] != "match" || v["size"] != n || v["bytes_hashed"] != n {
		t.Fatalf("verify = %v", v)
	}
	for key, want := range map[string]string{"md5": e.md5, "sha1": e.sha1} {
		h, _ := v[key].(map[string]any)
		if len(h) != 3 || h["stored"] != want || h["computed"] != want || h["status"] != "match" {
			t.Fatalf("%s = %v", key, v[key])
		}
	}
	if _, has := doc["partitions"]; !has {
		t.Errorf("the usual info is missing: %v", doc)
	}

	// Without --verify the object is omitted.
	_, out, _ = e.info(t, "--json")
	var plain map[string]any
	if err := json.Unmarshal([]byte(out), &plain); err != nil {
		t.Fatal(err)
	}
	if _, has := plain["verify"]; has {
		t.Errorf("plain info --json carries verify: %v", plain["verify"])
	}
}

func TestImageInfoVerifyJSONMismatchAndRaw(t *testing.T) {
	probe := newEWFEnv(t, ewftest.Options{})
	flipped, _ := hex.DecodeString(probe.md5)
	flipped[0] ^= 1
	e := newEWFEnv(t, ewftest.Options{MD5Override: flipped})
	code, out, _ := e.info(t, "--verify", "--json")
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want %d", code, ExitIntegrity)
	}
	var doc struct {
		Verify struct {
			Result string `json:"result"`
			MD5    struct{ Stored, Computed, Status string }
		}
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the JSON must be printed before the error: %v\n%s", err, out)
	}
	if doc.Verify.Result != "mismatch" || doc.Verify.MD5.Stored != hex.EncodeToString(flipped) || doc.Verify.MD5.Computed != e.md5 {
		t.Fatalf("%+v", doc.Verify)
	}

	r := newImgEnv(t)
	code, out, errs := runSplit(context.Background(), t, r.d, "image", "info", "--case", r.c, r.ref, "--verify", "--json")
	if code != 0 || !strings.Contains(errs, "container has no stored hashes") {
		t.Fatalf("raw: %d\n%s\n%s", code, out, errs)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("stdout must stay one JSON document: %v\n%s", err, out)
	}
	if _, has := raw["verify"]; has {
		t.Errorf("raw image has a verify object: %v", raw["verify"])
	}
}

func TestImageVerifyOutputEscapesContainerText(t *testing.T) {
	const evil = "x\x1b[31mred\u202eevil"
	e := newEWFEnv(t, ewftest.Options{Examiner: evil, Description: "d\x1b]0;title\x07\u202e", Case: "c\x1b[2J", Evidence: "e\u2066"})
	code, out, errs := e.info(t, "--verify")
	if code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	if !strings.Contains(out, "Verify:       result  match") {
		t.Fatalf("no verification in the output:\n%s", out)
	}
	for name, s := range map[string]string{"stdout": out, "stderr": errs} {
		for _, r := range s {
			if r == '\x1b' || r == '\x07' || r == '\u202e' || r == '\u2066' {
				t.Errorf("%s holds the control or bidi character %U:\n%q", name, r, s)
			}
		}
	}
}

func TestImageInfoShowsEWFMetadata(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{Case: "CASE-7", Evidence: "EV-9", Description: "Phone image", Examiner: "A. Examiner", Notes: "n1", Acquired: "1700000000"})
	code, out, errs := e.info(t)
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{
		"Format:", "ewf", "case_number:", "CASE-7", "evidence_number:", "EV-9", "description:", "Phone image",
		"examiner:", "A. Examiner", "notes:", "n1", "segments:", "3", "md5:", e.md5, "sha1:", e.sha1,
		"gpt", "mtfs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// numOf reads a number from audit details (json.Number when read back); -1 otherwise.
func numOf(v any) int {
	n, ok := v.(json.Number)
	if !ok {
		return -1
	}
	i, err := n.Int64()
	if err != nil {
		return -1
	}
	return int(i)
}
