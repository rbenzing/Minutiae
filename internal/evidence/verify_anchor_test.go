package evidence_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func verifyAnchored(t *testing.T, c *evidence.Case, o evidence.VerifyOptions) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWithOptions(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func anchorProblems(rep evidence.VerifyReport) []string {
	var out []string
	for _, p := range rep.Problems {
		if strings.HasPrefix(p, "anchor: ") {
			out = append(out, p)
		}
	}
	return out
}

func requireOnlyAnchorProblems(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	if len(anchorProblems(rep)) == 0 {
		t.Fatalf("no anchor problem; problems:\n%s", strings.Join(rep.Problems, "\n"))
	}
	for _, p := range rep.Problems {
		if !strings.HasPrefix(p, "anchor: ") {
			t.Errorf("unexpected non-anchor problem: %s", p)
		}
	}
	if rep.OK() {
		t.Error("verify reported OK")
	}
}

func headOf(a evidence.AuditAnchor) *evidence.AuditAnchor {
	h, err := evidence.ParseAnchorHead(fmt.Sprintf("%d:%s", a.Seq, a.Hash))
	if err != nil {
		panic(err)
	}
	return &h
}

func TestVerifyWithMatchingAnchorIsClean(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	anchorIngest(t, c, "b.db", 7)
	if _, err := c.Audit.Append("test.note", "", map[string]any{"k": "v"}); err != nil {
		t.Fatal(err)
	}
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	wantAfter := len(entries) - int(a.Seq)
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireClean(t, rep)
	if rep.Anchor == nil || !rep.Anchor.OK || !rep.Anchor.Checked || rep.Anchor.Seq != a.Seq || rep.Anchor.Hash != a.Hash || rep.Anchor.After != wantAfter {
		t.Fatalf("anchor result = %+v, want OK and %d entries after", rep.Anchor, wantAfter)
	}
	evidencetest.RequireNotice(t, rep, fmt.Sprintf("anchor: the audit log holds %d entries after seq %d", wantAfter, a.Seq))
	evidencetest.RequireNotice(t, rep, "test.note")
	evidencetest.RequireNotice(t, rep, "records.batch")
	evidencetest.RequireNotice(t, rep, "unsigned") // an unsigned anchor is as strong as the place it was stored
}

func TestVerifyWithoutAnchorReportsNull(t *testing.T) {
	c, _ := anchorCase(t)
	rep := verifyAnchored(t, c, evidence.VerifyOptions{})
	evidencetest.RequireClean(t, rep)
	if rep.Anchor != nil {
		t.Errorf("anchor = %+v without an anchor", rep.Anchor)
	}
	b, err := json.Marshal(rep)
	if err != nil || !strings.Contains(string(b), `"anchor":null`) {
		t.Errorf("json = %s, %v: the anchor key must be present and null", b, err)
	}
	for _, n := range rep.Notices {
		if strings.HasPrefix(n, "anchor:") {
			t.Errorf("anchor notice without an anchor: %s", n)
		}
	}
}

func TestRechainAuditMakesAValidChain(t *testing.T) {
	c, _ := anchorCase(t)
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := evidence.ReadAuditEntries(dir + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RechainAudit(t, dir, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		es[1].Actor = "someone-else"
		return append(es[:3:3], es[4:]...) // also drop one entry: seq is renumbered
	})
	n, problems, err := evidence.VerifyAuditLog(dir + "/audit.jsonl")
	if err != nil || len(problems) != 0 || n != len(before)-1 {
		t.Fatalf("n=%d (want %d) problems=%v err=%v", n, len(before)-1, problems, err)
	}
	after, err := evidence.ReadAuditEntries(dir + "/audit.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if after[1].Actor != "someone-else" || after[3].Seq != 4 {
		t.Errorf("entries after rechain: actor %q, seq %d", after[1].Actor, after[3].Seq)
	}
}

func TestAnchorDetectsRewrittenPrefix(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	evidencetest.RechainAudit(t, dir, func(es []evidence.AuditEntry) []evidence.AuditEntry {
		es[0].Actor = "forger" // a valid chain again, but every hash from here on differs
		return es
	})
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	rep := verifyAnchored(t, c2, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: audit seq %d has hash", a.Seq))
	if rep.Anchor == nil || rep.Anchor.OK {
		t.Errorf("anchor result = %+v, want not OK", rep.Anchor)
	}
	// the rechained log is valid by itself and the artifacts are untouched, so the anchor is the only finding
	for _, p := range rep.Problems {
		if strings.Contains(p, "chain") || strings.Contains(p, "hash mismatch") {
			t.Errorf("the chain should be valid again: %s", p)
		}
	}
}

func TestAnchorDetectsTruncatedLog(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := c.Dir
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	evidencetest.RechainAudit(t, dir, func(es []evidence.AuditEntry) []evidence.AuditEntry { return es[:a.Seq-4] }) // well short of the anchor: opening the case appends its own entry
	c2, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	rep := verifyAnchored(t, c2, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: names audit seq %d, which the log does not hold (", a.Seq))
	if rep.Anchor == nil || rep.Anchor.OK || rep.Anchor.Checked {
		t.Errorf("anchor result = %+v, want neither OK nor checked", rep.Anchor)
	}
}

func TestAnchorDetectsErasedBatchRow(t *testing.T) {
	c, ingestID := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	// L67's scenario: erase a batch, then append a well-formed recover that explains it
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	var end evidence.IngestConclusion
	for _, e := range entries {
		if e.Action == evidence.ActionIngestEnd {
			if end, err = evidence.DecodeDetails[evidence.IngestConclusion](e.Details); err != nil {
				t.Fatal(err)
			}
		}
	}
	recordstest.DeleteBatch(t, c.Dir, ingestID, 2)
	forged := evidence.IngestRecover{IngestConclusion: end, ByIngestID: "ing-forged", Reason: "forged", BatchNos: []int{2}}
	if _, err := c.Audit.Append(evidence.ActionIngestRecover, "", forged.Details()); err != nil {
		t.Fatal(err)
	}
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: the record batches announced up to seq %d differ from the set the anchor recorded (3 rows then, 2 now)", a.Seq))
	if rep.OK() || rep.Anchor == nil || rep.Anchor.OK {
		t.Errorf("OK=%v anchor=%+v, want a failure", rep.OK(), rep.Anchor)
	}
	// without the anchor the forgery is not caught by this rule
	plain := verifyAnchored(t, c, evidence.VerifyOptions{})
	evidencetest.RequireNoProblem(t, plain, "anchor:")
}

func TestAnchorOfAnotherCaseIsAProblem(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	a.CaseID = "OTHERCASE"
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: names case %q, this case is %q", "OTHERCASE", c.Meta.ID))
	requireOnlyAnchorProblems(t, rep)
}

func TestAnchorSignedByAnotherKeyIsAProblem(t *testing.T) {
	c, _ := anchorCase(t)
	pub1, key1, _ := ed25519.GenerateKey(rand.Reader)
	pub2, key2, _ := ed25519.GenerateKey(rand.Reader)
	a, err := c.Anchor(context.Background(), key1, "")
	if err != nil {
		t.Fatal(err)
	}
	// the right key: clean, and no "not bound" notice
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a, TrustedKey: pub1})
	evidencetest.RequireClean(t, rep)
	for _, n := range rep.Notices {
		if strings.Contains(n, "not bound") {
			t.Errorf("notice with a trusted key: %s", n)
		}
	}
	// re-signed with another key
	resigned := evidence.SignAnchor(a, key2)
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &resigned, TrustedKey: pub1})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: signed by key %s, --anchor-key is %s", evidence.KeyFingerprint(pub2), evidence.KeyFingerprint(pub1)))
	requireOnlyAnchorProblems(t, rep)
	// unsigned, but a trusted key was given
	unsigned := a
	unsigned.Signature = nil
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &unsigned, TrustedKey: pub1})
	evidencetest.RequireProblem(t, rep, "anchor: unsigned, but a trusted key was given")
	// a damaged signature fails with and without a trusted key
	bad := a
	sig := *a.Signature
	sig.Sig = strings.Repeat("0", len(sig.Sig))
	bad.Signature = &sig
	for _, tk := range []ed25519.PublicKey{nil, pub1} {
		rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &bad, TrustedKey: tk})
		evidencetest.RequireProblem(t, rep, "anchor: signature does not verify")
	}
	// no trusted key: a valid embedded signature is accepted with a notice that the signer is not bound
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireClean(t, rep)
	evidencetest.RequireNotice(t, rep, "anchor: signed by key "+evidence.KeyFingerprint(pub1))
	evidencetest.RequireNotice(t, rep, "not bound")
}

func TestAnchorHeadLiteralChecksBatchesToo(t *testing.T) {
	c, ingestID := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: headOf(a)})
	evidencetest.RequireClean(t, rep)
	evidencetest.RequireNotice(t, rep, "anchor: case id not checked")
	recordstest.DeleteBatch(t, c.Dir, ingestID, 3)
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: headOf(a)})
	evidencetest.RequireProblem(t, rep, "anchor: the record batches announced up to seq")
}

func TestAnchorToNonAnchorEntryNoticesBatches(t *testing.T) {
	c, _ := anchorCase(t)
	entries, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	h := headOf(evidence.AuditAnchor{Seq: 1, Hash: entries[0].Hash})
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: h})
	evidencetest.RequireClean(t, rep)
	evidencetest.RequireNotice(t, rep, "record batches not checked")
	if rep.Anchor == nil || !rep.Anchor.OK {
		t.Errorf("anchor result = %+v", rep.Anchor)
	}
}

func TestAnchorHashMismatchIsAProblem(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	a.Hash = strings.Repeat("0", 64)
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("anchor: audit seq %d has hash", a.Seq))
	requireOnlyAnchorProblems(t, rep)
}

func TestAnchorInsideTheCaseGivesANotice(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a, AnchorPath: c.Dir + "/anchor.json"})
	evidencetest.RequireNotice(t, rep, "inside the case directory")
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a, AnchorPath: t.TempDir() + "/anchor.json"})
	for _, n := range rep.Notices {
		if strings.Contains(n, "inside the case directory") {
			t.Errorf("notice for an anchor outside the case: %s", n)
		}
	}
}

func TestAnchorSchemaV1(t *testing.T) {
	dir := recordstest.NewV1Case(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	evidencetest.RequireClean(t, rep)
	if rep.Anchor == nil || !rep.Anchor.OK || !rep.Anchor.Checked {
		t.Errorf("anchor result = %+v", rep.Anchor)
	}
}

func TestAnchorCancelledIsNotChecked(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := c.VerifyWithOptions(ctx, evidence.VerifyOptions{Anchor: &a})
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Fatal("a cancelled verify reported OK")
	}
	if rep.Anchor == nil || rep.Anchor.Checked || rep.Anchor.OK {
		t.Errorf("anchor result = %+v, want unchecked", rep.Anchor)
	}
}

func TestVerifyOptionsOnlyAdd(t *testing.T) {
	var names []string
	typ := reflect.TypeOf(evidence.VerifyOptions{})
	for i := range typ.NumField() {
		names = append(names, typ.Field(i).Name)
	}
	want := []string{"Progress", "Anchor", "TrustedKey", "AnchorPath", "Checks"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("VerifyOptions fields = %v, want exactly %v (a new field needs a test and review: nothing may skip a check)", names, want)
	}
}

// TestVerifyObservedEqualsWithOptions (PASSES ON ARRIVAL after the refactor; mutation: drop Checks in the wrapper)
// pins that the old entry points are the options form.
func TestVerifyObservedEqualsWithOptions(t *testing.T) {
	c, _ := anchorCase(t)
	chk := func(_ context.Context, _ *evidence.Case, _ []evidence.ManifestRecord, rep *evidence.VerifyReport) {
		rep.AddProblem("composed check ran")
		rep.AddNotice("composed notice")
	}
	a, err := c.VerifyObserved(context.Background(), evidence.NewVerifyProgress(), chk)
	if err != nil {
		t.Fatal(err)
	}
	b := verifyAnchored(t, c, evidence.VerifyOptions{Progress: evidence.NewVerifyProgress(), Checks: []evidence.VerifyCheck{chk}})
	w, err := c.VerifyWith(context.Background(), chk)
	if err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]evidence.VerifyReport{"observed": a, "options": b, "with": w} {
		if !reflect.DeepEqual(r.Problems, []string{"composed check ran"}) || !reflect.DeepEqual(r.Notices, []string{"composed notice"}) || r.Anchor != nil {
			t.Errorf("%s: problems %q notices %q anchor %v", name, r.Problems, r.Notices, r.Anchor)
		}
	}
}

func TestVerifyRunAuditRecordsTheAnchor(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &a})
	d := lastEntry(t, c)
	if d.Action != "verify.run" || d.Details["anchor_seq"] != json.Number(fmt.Sprint(a.Seq)) || d.Details["anchor_hash"] != a.Hash || d.Details["anchor_ok"] != true {
		t.Errorf("verify.run details = %v", d.Details)
	}
	verifyAnchored(t, c, evidence.VerifyOptions{})
	d = lastEntry(t, c)
	for _, k := range []string{"anchor_seq", "anchor_hash", "anchor_ok"} {
		if _, ok := d.Details[k]; ok {
			t.Errorf("verify.run without an anchor carries %s", k)
		}
	}
}

func TestAnchorCreatedAndToolVersionMustMatchTheEntry(t *testing.T) {
	c, _ := anchorCase(t)
	a, err := c.Anchor(context.Background(), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	cr := a
	cr.Created = "2001-01-01T00:00:00Z"
	rep := verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &cr})
	evidencetest.RequireProblem(t, rep, "anchor: created ")
	tv := a
	tv.ToolVersion = "v0.0.0-forged"
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: &tv})
	evidencetest.RequireProblem(t, rep, "anchor: tool_version ")
	if rep.OK() || rep.Anchor == nil || rep.Anchor.OK {
		t.Errorf("OK=%v anchor=%+v", rep.OK(), rep.Anchor)
	}
	// a head literal carries neither field and is not flagged
	rep = verifyAnchored(t, c, evidence.VerifyOptions{Anchor: headOf(a)})
	evidencetest.RequireClean(t, rep)
}
