package evidence_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

// anchorIngest ingests n records (batches of 5) covering a new artifact of c and returns the ingest id.
func anchorIngest(t *testing.T, c *evidence.Case, name string, n int) string {
	t.Helper()
	ctx := context.Background()
	a := recordstest.AddArtifact(t, c, name, make([]byte, recordstest.ArtifactSize))
	w, err := records.NewWriter(c, records.Parser{Name: "p-" + name, Version: "1"}, records.WriterOptions{BatchRows: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(ctx, records.StartOptions{Artifacts: []string{a.ID}}); err != nil {
		t.Fatal(err)
	}
	for _, r := range recordstest.Records(a.ID, n, 3) {
		if err := w.Add(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := w.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res.IngestID
}

// anchorCase is a record case with three batches of one ingest.
func anchorCase(t *testing.T) (*evidence.Case, string) {
	t.Helper()
	c := recordstest.NewCase(t)
	return c, anchorIngest(t, c, "a.db", 12)
}

// wantBatchSet is K7 written out independently of the production code, from the announcements in the log.
func wantBatchSet(t *testing.T, entries []evidence.AuditEntry, upto int64) evidence.BatchSetDigest {
	t.Helper()
	type row struct {
		ingest string
		no     int
		digest string
	}
	var rows []row
	for _, e := range entries {
		if e.Action != evidence.ActionBatch || e.Seq > upto {
			continue
		}
		b, err := evidence.DecodeDetails[evidence.BatchCommit](e.Details)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row{b.IngestID, b.BatchNo, b.Digest})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ingest != rows[j].ingest {
			return rows[i].ingest < rows[j].ingest
		}
		return rows[i].no < rows[j].no
	})
	var buf bytes.Buffer
	for _, r := range rows {
		buf.WriteString(r.ingest + "\x00" + strconv.Itoa(r.no) + "\x00" + r.digest + "\n")
	}
	sum := sha256.Sum256(buf.Bytes())
	return evidence.BatchSetDigest{Count: int64(len(rows)), SHA256: hex.EncodeToString(sum[:])}
}

func anchorDetails(t *testing.T, e evidence.AuditEntry) evidence.CaseAnchorDetails {
	t.Helper()
	b, err := json.Marshal(e.Details)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d evidence.CaseAnchorDetails
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("case.anchor details do not decode strictly: %v\n%s", err, b)
	}
	return d
}

func lastEntry(t *testing.T, c *evidence.Case) evidence.AuditEntry {
	t.Helper()
	es, err := c.ReadAudit()
	if err != nil || len(es) == 0 {
		t.Fatalf("audit: %d entries, %v", len(es), err)
	}
	return es[len(es)-1]
}

func TestCaseAnchorAuditsFirstAndNamesItsEntry(t *testing.T) {
	c, _ := anchorCase(t)
	before, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	last := lastEntry(t, c)
	if last.Action != evidence.ActionCaseAnchor || last.Seq != int64(len(before))+1 {
		t.Fatalf("last entry = seq %d %q, want the %q entry after %d entries", last.Seq, last.Action, evidence.ActionCaseAnchor, len(before))
	}
	if a.Seq != last.Seq || a.Hash != last.Hash || a.CaseID != c.Meta.ID || a.Created != last.Time || a.ToolVersion != last.ToolVersion || a.Format != evidence.AnchorFormat || a.Signature != nil {
		t.Errorf("anchor = %+v, entry = seq %d hash %s time %s", a, last.Seq, last.Hash, last.Time)
	}
	d := anchorDetails(t, last)
	want := wantBatchSet(t, before, last.Seq)
	if want.Count != 3 || d.Signed || d.KeyFingerprint != "" || d.Out != "" || d.RecordBatches == nil || *d.RecordBatches != want {
		t.Errorf("details = %+v (batches %+v), want unsigned with %+v", d, d.RecordBatches, want)
	}
	// the anchor file round-trips and carries no verdict
	if got, err := evidence.ParseAnchor(a.Marshal()); err != nil || got.Seq != a.Seq {
		t.Errorf("ParseAnchor(Marshal) = %+v, %v", got, err)
	}
}

func TestCaseAnchorSigned(t *testing.T) {
	c, _ := anchorCase(t)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.Anchor(context.Background(), key, "anchors/a1.json")
	if err != nil {
		t.Fatal(err)
	}
	if a.Signature == nil || a.CheckSignature(pub) != nil || a.CheckSignature(nil) != nil {
		t.Fatalf("anchor signature = %+v", a.Signature)
	}
	d := anchorDetails(t, lastEntry(t, c))
	if !d.Signed || d.KeyFingerprint != evidence.KeyFingerprint(pub) || d.Out != "anchors/a1.json" {
		t.Errorf("details = %+v", d)
	}
}

func TestCaseAnchorRefusesABrokenChain(t *testing.T) {
	c, _ := anchorCase(t)
	recordstest.ReplaceInAuditLine(t, c.Dir, 2, `"actor":"`, `"actor":"x`)
	lines := func() int {
		b, err := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Count(b, []byte("\n"))
	}
	n := lines()
	a, err := c.Anchor(context.Background(), nil, "")
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("Anchor over an edited log: err = %v, want ErrIntegrity", err)
	}
	if a.Seq != 0 || a.Hash != "" {
		t.Errorf("an anchor came back with the error: %+v", a)
	}
	if got := lines(); got != n {
		t.Errorf("audit lines %d -> %d: a refused anchor appended", n, got)
	}
}

func TestCaseAnchorAuditFailureBlocksIt(t *testing.T) {
	c, _ := anchorCase(t)
	if err := c.Audit.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl"))
	a, err := c.Anchor(context.Background(), nil, "")
	if err == nil {
		t.Fatal("Anchor succeeded with a closed audit log")
	}
	if a.Seq != 0 || a.Hash != "" {
		t.Errorf("an anchor came back with the error: %+v", a)
	}
	if after, _ := os.ReadFile(filepath.Join(c.Dir, "audit.jsonl")); !bytes.Equal(after, b) {
		t.Error("the audit log changed")
	}
}

func TestCaseAnchorSchemaV1HasNullBatches(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Anchor(context.Background(), nil, ""); err != nil {
		t.Fatal(err)
	}
	last := lastEntry(t, c)
	if v, present := last.Details["record_batches"]; !present || v != nil {
		t.Errorf("record_batches = %#v (present %v), want an explicit null", v, present)
	}
	if d := anchorDetails(t, last); d.RecordBatches != nil {
		t.Errorf("decoded batches = %+v", d.RecordBatches)
	}
}
