package examine_test

import (
	"bytes"
	"context"
	"crypto/md5"  //nolint:gosec // format-stored hashes
	"crypto/sha1" //nolint:gosec // format-stored hashes
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// e01Media is a GPT disk with one mtfs partition, long enough for several chunks.
func e01Media() []byte {
	return disk(mtfsImage(map[string]string{"/a.txt": "hello", "/d/b.txt": strings.Repeat("z", 6000)}))
}

// importE01 builds an E01 set from media, imports its segments into c and
// returns the manifest records.
func importE01(t *testing.T, c *evidence.Case, o ewftest.Options, media []byte) []evidence.ManifestRecord {
	t.Helper()
	dir := t.TempDir()
	segs := ewftest.Build(o, media)
	paths := make([]string, len(segs))
	for i, s := range segs {
		p := filepath.Join(dir, "disk.E0"+string(rune('1'+i)))
		if err := os.WriteFile(p, s, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}
	recs, err := examine.Import(context.Background(), c, "img", paths, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return recs
}

func verifyEntries(t *testing.T, c *evidence.Case) []evidence.AuditEntry {
	t.Helper()
	return auditByAction(t, c, "image.verify")
}

func caseOK(t *testing.T, c *evidence.Case) {
	t.Helper()
	rep, err := c.Verify()
	if err != nil || !rep.OK() {
		t.Fatalf("case verify: %+v, %v", rep, err)
	}
}

// hashEntry reads details[key] as {stored, computed, status}.
func hashEntry(t *testing.T, d map[string]any, key string) map[string]any {
	t.Helper()
	m, ok := d[key].(map[string]any)
	if !ok {
		t.Fatalf("details[%q] = %#v", key, d[key])
	}
	return m
}

// num reads a JSON number from audit details (decoded with UseNumber); -1 otherwise.
func num(v any) int {
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

func hexOf(media []byte) (md5hex, sha1hex string) {
	m, s := md5.Sum(media), sha1.Sum(media) //nolint:gosec // format-stored hashes
	return hex.EncodeToString(m[:]), hex.EncodeToString(s[:])
}

func TestVerifyContainerAuditsResult(t *testing.T) {
	c := newCase(t)
	media := e01Media()
	recs := importE01(t, c, ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: 3, Compress: ewftest.CompressMixed}, media)
	if len(recs) < 2 {
		t.Fatalf("want a multi-segment image, got %d segments", len(recs))
	}
	s := openSession(t, c, recs[0].ID)
	var last int64
	cv, err := s.VerifyContainer(context.Background(), func(done, total int64) {
		if done < last || total != int64(len(media)) {
			t.Errorf("progress(%d, %d) after %d", done, total, last)
		}
		last = done
	})
	if err != nil {
		t.Fatal(err)
	}
	if cv.Format != "ewf" || cv.Result != "match" || cv.Size != int64(len(media)) || cv.BytesHashed != cv.Size || cv.BadChunk != -1 {
		t.Fatalf("%+v", cv)
	}
	m, sh := hexOf(media)
	if cv.MD5.Computed != m || cv.SHA1.Computed != sh || cv.MD5.Stored != m {
		t.Fatalf("%+v", cv)
	}

	es := verifyEntries(t, c)
	if len(es) != 1 {
		t.Fatalf("%d image.verify entries", len(es))
	}
	e := es[0]
	if e.DeviceID != recs[0].Source.DeviceID {
		t.Errorf("device id %q", e.DeviceID)
	}
	want := []string{"bytes_hashed", "format", "md5", "parent_id", "parent_path", "parent_sha256", "result", "segments", "sha1", "size"}
	if got := slices.Sorted(maps.Keys(e.Details)); !slices.Equal(got, want) {
		t.Fatalf("details keys %v, want %v", got, want)
	}
	d := e.Details
	if d["parent_id"] != recs[0].ID || d["parent_path"] != recs[0].Path || d["parent_sha256"] != recs[0].SHA256 ||
		d["format"] != "ewf" || d["result"] != "match" || num(d["size"]) != len(media) || num(d["bytes_hashed"]) != len(media) {
		t.Fatalf("details %v", d)
	}
	segs, ok := d["segments"].([]any)
	if !ok || len(segs) != len(recs) {
		t.Fatalf("segments %#v", d["segments"])
	}
	for i, sg := range segs {
		m, _ := sg.(map[string]any)
		if m["id"] != recs[i].ID || m["sha256"] != recs[i].SHA256 || len(m) != 2 {
			t.Fatalf("segment %d = %v", i, sg)
		}
	}
	for key, computed := range map[string]string{"md5": m, "sha1": sh} {
		h := hashEntry(t, d, key)
		if h["stored"] != computed || h["computed"] != computed || h["status"] != "match" || len(h) != 3 {
			t.Fatalf("%s = %v", key, h)
		}
	}
	caseOK(t, c)
}

func TestVerifyContainerMismatchAudited(t *testing.T) {
	c := newCase(t)
	media := e01Media()
	m, _ := hexOf(media)
	flipped, _ := hex.DecodeString(m)
	flipped[0] ^= 1
	recs := importE01(t, c, ewftest.Options{NoDigest: true, MD5Override: flipped}, media)
	s := openSession(t, c, recs[0].ID)
	cv, err := s.VerifyContainer(context.Background(), nil)
	if err != nil || cv.Result != "mismatch" {
		t.Fatalf("%+v, %v", cv, err)
	}
	es := verifyEntries(t, c)
	if len(es) != 1 || es[0].Details["result"] != "mismatch" {
		t.Fatalf("%+v", es)
	}
	h := hashEntry(t, es[0].Details, "md5")
	if h["stored"] != hex.EncodeToString(flipped) || h["computed"] != m || h["status"] != "mismatch" {
		t.Fatalf("md5 = %v", h)
	}
	if sh := hashEntry(t, es[0].Details, "sha1"); sh["status"] != "absent" {
		t.Fatalf("sha1 = %v", sh)
	}
	if _, has := es[0].Details["first_bad_chunk"]; has {
		t.Fatal("first_bad_chunk must be omitted when every chunk reads")
	}
}

func TestVerifyContainerUnreadableChunkAudited(t *testing.T) {
	c := newCase(t)
	media := e01Media()
	bad := bytes.Repeat([]byte{0x55}, 8*512+4) // right length, wrong Adler-32
	recs := importE01(t, c, ewftest.Options{SectorsPerChunk: 8, Override: map[int]ewftest.RawChunk{7: {Data: bad}}}, media)
	s := openSession(t, c, recs[0].ID)
	cv, err := s.VerifyContainer(context.Background(), nil)
	if err != nil || cv.Result != "unverified" || cv.BadChunk != 7 {
		t.Fatalf("%+v, %v", cv, err)
	}
	e := verifyEntries(t, c)[0]
	if e.Details["result"] != "unverified" || num(e.Details["first_bad_chunk"]) != 7 {
		t.Fatalf("%v", e.Details)
	}
	for _, key := range []string{"md5", "sha1"} {
		h := hashEntry(t, e.Details, key)
		if h["status"] != "unverified" || h["computed"] != "" || h["stored"] == "" {
			t.Fatalf("%s = %v", key, h)
		}
	}
	if num(e.Details["bytes_hashed"]) != 7*8*512 {
		t.Fatalf("bytes_hashed %v", e.Details["bytes_hashed"])
	}
	if _, has := e.Details["error"]; has {
		t.Fatal("an unreadable chunk is a result, not an error")
	}
}

func TestVerifyContainerCancelAudits(t *testing.T) {
	c := newCase(t)
	recs := importE01(t, c, ewftest.Options{}, e01Media())
	s := openSession(t, c, recs[0].ID)
	ctx, cancel := context.WithCancel(context.Background())
	cv, err := s.VerifyContainer(ctx, func(_, _ int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if cv.Result != "cancelled" || cv.BytesHashed == 0 || cv.BytesHashed >= cv.Size {
		t.Fatalf("%+v", cv)
	}
	es := verifyEntries(t, c)
	if len(es) != 1 || es[0].Details["result"] != "cancelled" {
		t.Fatalf("%+v", es)
	}
	if es[0].Details["error"] != context.Canceled.Error() {
		t.Fatalf("error = %v", es[0].Details["error"])
	}
	if h := hashEntry(t, es[0].Details, "md5"); h["status"] != "unverified" || h["computed"] != "" {
		t.Fatalf("md5 = %v", h)
	}
	caseOK(t, c)
}

func TestVerifyContainerRawIsNotVerifiable(t *testing.T) {
	c := newCase(t)
	recs := importImage(t, c, e01Media(), 1)
	s := openSession(t, c, recs[0].ID)
	before := len(auditEntries(t, c))
	if _, err := s.VerifyContainer(context.Background(), nil); !errors.Is(err, examine.ErrNoStoredHashes) {
		t.Fatalf("err = %v", err)
	}
	if after := len(auditEntries(t, c)); after != before {
		t.Fatalf("audit grew from %d to %d entries", before, after)
	}
}

// verifyStub is an EWF-format image of zeros whose Verify panics.
type verifyStub struct{ closed bool }

func (*verifyStub) ReadAt(p []byte, off int64) (int, error) {
	if off >= 1<<20 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), 1<<20-off)
	clear(p[:n])
	if int(n) < len(p) {
		return int(n), io.EOF
	}
	return int(n), nil
}
func (*verifyStub) Size() int64          { return 1 << 20 }
func (*verifyStub) SectorSize() int      { return 512 }
func (*verifyStub) Format() string       { return "ewf" }
func (*verifyStub) Metadata() []image.KV { return nil }
func (v *verifyStub) Close() error       { v.closed = true; return nil }
func (*verifyStub) Verify(context.Context, func(done, total int64)) (image.VerifyResult, error) {
	panic("boom in verify")
}

func TestVerifyContainerRecoversPanic(t *testing.T) {
	c := newCase(t)
	rec := ewfArtifact(t, c)
	stub := &verifyStub{}
	image.RegisterEWF(func(files []*os.File) (image.Image, error) {
		for _, f := range files {
			_ = f.Close()
		}
		return stub, nil
	})
	t.Cleanup(func() { image.RegisterEWF(nil) })
	s, err := examine.Open(c, rec.ID, mtfsOpts())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cv, err := s.VerifyContainer(context.Background(), nil)
	requireCorrupt(t, "VerifyContainer", err)
	var ce *filesys.CorruptError
	if !errors.As(err, &ce) || !strings.Contains(ce.Reason, "boom in verify") {
		t.Fatalf("err = %v", err)
	}
	if cv.Result != "error" {
		t.Fatalf("Result = %q", cv.Result)
	}
	es := verifyEntries(t, c)
	if len(es) != 1 || es[0].Details["result"] != "error" {
		t.Fatalf("%+v", es)
	}
	if msg, _ := es[0].Details["error"].(string); !strings.Contains(msg, "boom in verify") {
		t.Fatalf("error = %v", es[0].Details["error"])
	}
}

func TestVerifyContainerNeverModifiesSource(t *testing.T) {
	c := newCase(t)
	recs := importE01(t, c, ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: 3}, e01Media())
	type state struct {
		sum string
		mt  int64
	}
	before := map[string]state{}
	for _, r := range recs {
		p := filepath.Join(c.Dir, filepath.FromSlash(r.Path))
		sum, mt := fileState(t, p)
		before[p] = state{sum, mt}
	}
	s, err := examine.Open(c, recs[0].ID, mtfsOpts())
	if err != nil {
		t.Fatal(err)
	}
	if cv, err := s.VerifyContainer(context.Background(), nil); err != nil || cv.Result != "match" {
		t.Fatalf("%+v, %v", cv, err)
	}
	_ = s.Info()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for p, b := range before {
		sum, mt := fileState(t, p)
		if sum != b.sum || mt != b.mt {
			t.Errorf("%s changed", p)
		}
	}
	for _, r := range recs {
		if sum, _ := fileState(t, filepath.Join(c.Dir, filepath.FromSlash(r.Path))); sum != r.SHA256 {
			t.Errorf("%s: sha256 %s differs from the manifest %s", r.Path, sum, r.SHA256)
		}
	}
	caseOK(t, c)
}

func TestSessionInfoIncludesContainerWarnings(t *testing.T) {
	c := newCase(t)
	recs := importE01(t, c, ewftest.Options{ErrorRanges: [][2]uint32{{10, 5}}}, e01Media())
	s := openSession(t, c, recs[0].ID)
	var found bool
	for _, w := range s.Info().Warnings {
		found = found || strings.Contains(w, "unreadable at acquisition")
	}
	if !found {
		t.Fatalf("warnings %q", s.Info().Warnings)
	}
	// A clean image shows none, and Info does not write to the case.
	c2 := newCase(t)
	recs2 := importE01(t, c2, ewftest.Options{}, e01Media())
	before := len(auditEntries(t, c2))
	if w := openSession(t, c2, recs2[0].ID).Info().Warnings; len(w) != 0 {
		t.Fatalf("warnings %q", w)
	}
	if len(auditEntries(t, c2)) != before {
		t.Fatal("Info wrote to the audit log")
	}
}

// gpt4096 is a 4096-byte-sector GPT disk with one mtfs partition at LBA 40.
func gpt4096() []byte {
	fsImg := mtfsImage(map[string]string{"/a.txt": "hello"})
	sectors := uint64(len(fsImg)+4095) / 4096
	img := volumetest.GPT(4096, 40+sectors+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{
		{StartLBA: 40, Sectors: sectors, TypeGUID: linuxType, GUID: "00000000-0000-0000-0000-000000000001", Name: "p"},
	})
	copy(img[40*4096:], fsImg)
	return img
}

func TestE01Declared512ProbesGPT4096(t *testing.T) {
	media := gpt4096()
	for name, bps := range map[string]int{"declared 512": 512, "declared 4096": 4096} {
		t.Run(name, func(t *testing.T) {
			c := newCase(t)
			recs := importE01(t, c, ewftest.Options{BytesPerSector: bps, SectorsPerChunk: 32768 / bps}, media)
			s := openSession(t, c, recs[0].ID)
			info := s.Info()
			if info.Scheme != "gpt" || info.SectorSize != 4096 || len(info.Partitions) != 1 {
				t.Fatalf("scheme %q sector size %d partitions %d", info.Scheme, info.SectorSize, len(info.Partitions))
			}
			if p := info.Partitions[0].Partition; p.Start != 40*4096 {
				t.Fatalf("partition start %d", p.Start)
			}
			if info.Partitions[0].FSType != "mtfs" {
				t.Fatalf("fs %q (%s)", info.Partitions[0].FSType, info.Partitions[0].Error)
			}
		})
	}
}
