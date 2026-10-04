package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// A chunk that the partition probe needs is unreadable: `image info --verify`
// still verifies and audits the container, reports the partition failure
// apart, and exits 1 (the verification itself found no mismatch).
func TestImageVerifyUnreadablePartitionChunk(t *testing.T) {
	probe := newEWFEnv(t, ewftest.Options{})
	for _, tc := range []struct {
		name  string
		chunk int
	}{{"primary GPT", 0}, {"backup GPT", probe.chunks - 1}} {
		t.Run(tc.name, func(t *testing.T) {
			bad := bytes.Repeat([]byte{0x55}, ewfChunkBytes+4)
			e := newEWFEnv(t, ewftest.Options{Override: map[int]ewftest.RawChunk{tc.chunk: {Data: bad}}})

			// Without --verify the image cannot be examined.
			if code, out, errs := e.info(t); code != ExitError || strings.Contains(out, "Verify:") {
				t.Fatalf("plain info: exit %d\n%s\n%s", code, out, errs)
			}

			code, out, errs := e.info(t, "--verify")
			if code != ExitError {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, out, errs)
			}
			for _, want := range []string{"Format:", "ewf", "Verify:       first unreadable chunk " + itoa(tc.chunk) + ": ", "Verify:       result  unverified", "partition table"} {
				if !strings.Contains(out+errs, want) {
					t.Errorf("output lacks %q:\nstdout:\n%s\nstderr:\n%s", want, out, errs)
				}
			}
			if !strings.Contains(errs, "partition table") {
				t.Errorf("the partition failure is not reported on stderr: %q", errs)
			}
			es := e.verifyEntries(t)
			if len(es) != 1 || es[0].Details["result"] != "unverified" || numOf(es[0].Details["first_bad_chunk"]) != tc.chunk {
				t.Fatalf("image.verify entries: %+v", es)
			}
			e.caseVerifyOK(t)

			// JSON: one document with the verification and the partition failure.
			code, out, errs = e.info(t, "--verify", "--json")
			if code != ExitError {
				t.Fatalf("json exit %d\n%s\n%s", code, out, errs)
			}
			var doc struct {
				Verify         struct{ Result string } `json:"verify"`
				PartitionError string                  `json:"partition_error"`
				Format         string                  `json:"format"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("stdout is not one JSON document: %v\n%s", err, out)
			}
			if doc.Verify.Result != "unverified" || !strings.Contains(doc.PartitionError, "partition table") || doc.Format != "ewf" {
				t.Fatalf("%+v", doc)
			}
		})
	}
}

// ---- a stub container, to reach paths a real E01 cannot produce ------------

// stubEWF is an image of the EWF format whose partition reads and Verify are scripted.
type stubEWF struct {
	readErr error
	res     image.VerifyResult
	panics  bool
	verr    error
}

func (s *stubEWF) ReadAt(p []byte, off int64) (int, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	if off >= s.Size() {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.Size()-off)
	clear(p[:n])
	return int(n), nil
}
func (*stubEWF) Size() int64          { return 1 << 20 }
func (*stubEWF) SectorSize() int      { return 512 }
func (*stubEWF) Format() string       { return "ewf" }
func (*stubEWF) Metadata() []image.KV { return nil }
func (*stubEWF) Close() error         { return nil }
func (s *stubEWF) Verify(context.Context, func(done, total int64)) (image.VerifyResult, error) {
	if s.panics {
		panic("boom in stub verify")
	}
	return s.res, s.verr
}

// stubEnv imports a file that carries the EWF signature and routes the E01
// opener to stub.
func stubEnv(t *testing.T, stub *stubEWF) (d Deps, caseDir, ref string) {
	t.Helper()
	image.RegisterEWF(func(files []*os.File) (image.Image, error) {
		for _, f := range files {
			_ = f.Close()
		}
		return stub, nil
	})
	t.Cleanup(func() { image.RegisterEWF(nil) })
	caseDir = newCLICase(t)
	p := filepath.Join(t.TempDir(), "x.E01")
	if err := os.WriteFile(p, append([]byte("EVF\x09\x0d\x0a\xff\x00"), make([]byte, 1024)...), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, Deps{}, "image", "import", "--case", caseDir, "--json", p)
	if code != 0 {
		t.Fatalf("import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	return Deps{FSDrivers: mtfsDrivers}, caseDir, recs[0].ID
}

func chunkErr(msg string) error {
	return fmt.Errorf("%w: %s", image.ErrChunkCorrupt, msg)
}

// A mismatch is an integrity finding and exits 4 even when the partition table
// of the same image cannot be read.
func TestImageVerifyMismatchBeatsPartitionFailure(t *testing.T) {
	stub := &stubEWF{
		readErr: chunkErr("chunk 0 unreadable"),
		res: image.VerifyResult{
			Size: 1 << 20, BytesHashed: 1 << 20, BadChunk: -1,
			MD5:  image.HashCheck{Stored: strings.Repeat("a", 32), Computed: strings.Repeat("b", 32), Status: image.HashMismatch},
			SHA1: image.HashCheck{Status: image.HashAbsent},
		},
	}
	d, c, ref := stubEnv(t, stub)
	code, out, errs := runSplit(context.Background(), t, d, "image", "info", "--case", c, ref, "--verify")
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitIntegrity, out, errs)
	}
	if !strings.Contains(out, "result  mismatch") || !strings.Contains(errs, "partition table") {
		t.Errorf("stdout:\n%s\nstderr:\n%s", out, errs)
	}
}

// After a recovered parser panic nothing is known about the stored hashes: say
// "unknown", not "absent".
func TestImageVerifyPanicShowsStoredHashesUnknown(t *testing.T) {
	d, c, ref := stubEnv(t, &stubEWF{panics: true})
	code, out, errs := runSplit(context.Background(), t, d, "image", "info", "--case", c, ref, "--verify")
	if code != ExitError {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errs)
	}
	for _, want := range []string{"stored unknown", "result  error"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stored absent") {
		t.Errorf("claims the container stores no hash:\n%s", out)
	}
	if !strings.Contains(errs, "boom in stub verify") {
		t.Errorf("stderr: %q", errs)
	}
}

// No progress, no stray blank line on stderr.
func TestImageVerifyWritesNoStrayBlankLine(t *testing.T) {
	stub := &stubEWF{res: image.VerifyResult{
		Size: 1 << 20, BytesHashed: 1 << 20, BadChunk: -1,
		MD5: image.HashCheck{Status: image.HashAbsent}, SHA1: image.HashCheck{Status: image.HashAbsent},
	}}
	d, c, ref := stubEnv(t, stub)
	for _, args := range [][]string{{"--verify"}, {"--verify", "--json"}} {
		code, out, errs := runSplit(context.Background(), t, d, append([]string{"image", "info", "--case", c, ref}, args...)...)
		if code != 0 || errs != "" {
			t.Errorf("%v: exit %d, stderr %q (want empty)\n%s", args, code, errs, out)
		}
	}
}

// Text a container controls (the error of the first unreadable chunk) is escaped
// on the verify path, in the output and in the error.
func TestImageVerifyEscapesChunkErrorText(t *testing.T) {
	const evil = "bad\x1b[31m chunk\x07 \u202eevil\x00"
	stub := &stubEWF{res: image.VerifyResult{
		Size: 1 << 20, BytesHashed: 4096, BadChunk: 3, BadChunkError: evil,
		MD5:  image.HashCheck{Status: image.HashUnverified},
		SHA1: image.HashCheck{Status: image.HashUnverified},
	}}
	d, c, ref := stubEnv(t, stub)
	code, out, errs := runSplit(context.Background(), t, d, "image", "info", "--case", c, ref, "--verify")
	if code != ExitError {
		t.Fatalf("exit %d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(out, "first unreadable chunk 3: ") {
		t.Fatalf("stdout:\n%s", out)
	}
	for name, s := range map[string]string{"stdout": out, "stderr": errs} {
		for _, r := range s {
			if r == '\x1b' || r == '\x07' || r == 0 || r == '\u202e' {
				t.Errorf("%s holds the control or bidi character %U:\n%q", name, r, s)
			}
		}
	}
}

// A mismatch must never be masked by a failed audit append: exit 4, and the
// audit failure is reported as well.
func TestVerifyOutcomeMismatchSurvivesAuditFailure(t *testing.T) {
	cv := &examine.ContainerVerification{Result: "mismatch"}
	cv.Size, cv.BytesHashed, cv.BadChunk = 10, 10, -1
	cv.MD5.Status = image.HashMismatch
	err := verifyOutcome(cv, errors.New("append audit entry: disk full"))
	if err == nil || !errors.Is(err, evidence.ErrIntegrity) || ExitCode(err) != ExitIntegrity {
		t.Fatalf("err = %v, exit %d", err, ExitCode(err))
	}
	if !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "MD5") {
		t.Fatalf("err = %v lacks the audit failure or the mismatch", err)
	}
	// Any other result stays exit 1 when the audit append failed.
	cv.Result, cv.MD5.Status = "match", image.HashMatch
	if err := verifyOutcome(cv, errors.New("disk full")); ExitCode(err) != ExitError {
		t.Fatalf("exit %d, want %d", ExitCode(err), ExitError)
	}
}
