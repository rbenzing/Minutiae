package artparse_test

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// fake is a parser that does nothing; only its Meta matters to the host here.
type fake struct {
	meta  func() parse.Meta
	calls atomic.Int32
}

func (f *fake) Meta() parse.Meta {
	f.calls.Add(1)
	return f.meta()
}

func (f *fake) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: parse.Applicable}, nil
}
func (f *fake) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

func hashFor(name string) string {
	const hex = "0123456789abcdef"
	h := "src1:sha256:"
	for i := 0; i < 64; i++ {
		h += string(hex[(int(name[i%len(name)])+i)%16])
	}
	return h
}

// metaFor is a database parser: primary "db" with two Android globs, an
// optional "wal" companion, emitting message v1.
func metaFor(name, version string, primaryGlobs ...string) parse.Meta {
	if len(primaryGlobs) == 0 {
		primaryGlobs = []string{"android:/data/data/*/databases/app.db", "android:/data/user/*/*/databases/app.db"}
	}
	return parse.Meta{
		Name: name, Version: version, Title: name + " test parser",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{
			{Role: "db", Globs: primaryGlobs, Required: true},
			{Role: "wal", Companions: []string{"-wal"}},
		},
	}
}

func register(t testing.TB, name, version string, primaryGlobs ...string) artparse.Registered {
	t.Helper()
	m := metaFor(name, version, primaryGlobs...)
	r, err := artparse.Register(&fake{meta: func() parse.Meta { return m }}, hashFor(name+version))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newHost(t testing.TB, c *evidence.Case, ps ...artparse.Registered) *artparse.Host {
	t.Helper()
	h, err := artparse.New(c, ps, artparse.Options{Limits: parse.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// put captures data as an artifact of device dev, acquisition acq; the case
// path is files/<rel>.
func put(t testing.TB, c *evidence.Case, dev, acq, rel string, src evidence.Source, data string) evidence.ManifestRecord {
	t.Helper()
	src.DeviceID = dev
	rec, err := c.Capture(dev, acq, "files/"+rel, src, func(w io.Writer) error {
		_, err := io.WriteString(w, data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// putFile is a pulled file whose remote path doubles as its case path.
func putFile(t testing.TB, c *evidence.Case, dev, acq, remote, data string) evidence.ManifestRecord {
	t.Helper()
	return put(t, c, dev, acq, strings.TrimPrefix(remote, "/"), evidence.Source{Kind: "file", RemotePath: remote}, data)
}

// putExtract is an artifact extracted from an image filesystem.
func putExtract(t testing.TB, c *evidence.Case, dev, acq, rel, fsType, fsPath string, snap *evidence.SnapshotRef) evidence.ManifestRecord {
	t.Helper()
	d := &evidence.Derivation{ParentID: "p", ParentSHA256: strings.Repeat("0", 64), FSType: fsType, FSPath: fsPath, Snapshot: snap}
	return put(t, c, dev, acq, rel, evidence.Source{Kind: "extract", RemotePath: fsPath, Derived: d}, "x")
}

func acquireStart(t testing.TB, c *evidence.Case, dev, acq, typ string) {
	t.Helper()
	if _, err := c.Audit.Append("acquire.start", dev, map[string]any{"acquisition_id": acq, "type": typ}); err != nil {
		t.Fatal(err)
	}
}

func newCase(t testing.TB) *evidence.Case { return recordstest.NewCase(t) }

func snapshotOf(t testing.TB, h *artparse.Host) *artparse.Snapshot {
	t.Helper()
	s, err := h.TakeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func discover(t testing.TB, h *artparse.Host, sel artparse.Selection) ([]artparse.Job, artparse.Counts) {
	t.Helper()
	jobs, counts, err := h.Discover(context.Background(), snapshotOf(t, h), sel)
	if err != nil {
		t.Fatal(err)
	}
	return jobs, counts
}

func jobIDs(jobs []artparse.Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Primary.Artifact.ID)
	}
	return out
}
