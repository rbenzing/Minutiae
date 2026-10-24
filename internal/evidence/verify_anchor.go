package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// VerifyOptions are the inputs of VerifyWithOptions. Every field only adds work or observes: there is no
// field that skips, narrows or caches a check, and a test pins the field list.
type VerifyOptions struct {
	Progress   *VerifyProgress   // observes only
	Anchor     *AuditAnchor      // nil: no anchor given
	TrustedKey ed25519.PublicKey // nil: the signature is checked against the key the anchor carries
	AnchorPath string            // where the anchor came from, for the inside-the-case notice; "" for a head literal
	Checks     []VerifyCheck     // composed by a layer above this package
}

// VerifyWithOptions is Verify with the given options: every check of a plain verify runs, then the
// anchor check A1 when an anchor is given (nothing is skipped because an anchor matched).
func (c *Case) VerifyWithOptions(ctx context.Context, o VerifyOptions) (VerifyReport, error) {
	return c.verifyRun(ctx, nil, o)
}

// AnchorResult is what the anchor check found. It is a finding of this run, never a stored verdict.
type AnchorResult struct {
	Seq     int64  `json:"seq"`
	Hash    string `json:"hash"`
	OK      bool   `json:"ok"`      // every A1 rule ran and none failed
	Checked bool   `json:"checked"` // every A1 rule ran (false when the log lacks the entry or a cancel cut the batch-set part)
	After   int    `json:"after"`   // audit entries after the anchored one, when verify read the log
}

const maxAnchorActionsListed = 20

// anchorRun is the state of A1 in one verify run.
type anchorRun struct {
	a       AuditAnchor
	trusted ed25519.PublicKey
	path    string

	failed  bool            // an A1 problem was reported
	skipped bool            // a rule could not run because the anchored entry is unusable
	after   int             // entries after the anchored one
	want    *BatchSetDigest // the batch set the case.anchor entry recorded; nil: the batch rule does not apply
	seq     int64           // seq of the anchored entry when want is set
	done    bool            // the batch-set rule ran
}

func (ar *anchorRun) problemf(rep *VerifyReport, format string, a ...any) {
	ar.failed = true
	rep.problemf("anchor: "+format, a...)
}

func (ar *anchorRun) result() *AnchorResult {
	checked := !ar.skipped && (ar.want == nil || ar.done)
	return &AnchorResult{Seq: ar.a.Seq, Hash: ar.a.Hash, OK: checked && !ar.failed, Checked: checked, After: ar.after}
}

// anchorAudit is A1 except the batch-set rule (which needs the stored rows and runs inside the record
// checks): case id, the anchored entry, its hash, the signature, and the notices about what follows it.
func (c *Case) anchorAudit(rep *VerifyReport, ar *anchorRun, entries []AuditEntry, readable bool) {
	a := ar.a
	if ar.path != "" && c.pathInCase(ar.path) {
		rep.noticef("anchor: the anchor file is inside the case directory: whoever can rewrite the case can rewrite this anchor")
	}
	if a.CaseID == "" {
		rep.noticef("anchor: case id not checked (a head literal names no case)")
	} else if a.CaseID != c.Meta.ID {
		ar.problemf(rep, "names case %q, this case is %q", a.CaseID, c.Meta.ID)
	}
	ar.checkSignature(rep)
	if !readable {
		ar.skipped = true
		ar.problemf(rep, "the audit log is unreadable, so the anchor cannot be checked")
		return
	}
	idx := -1
	for i, e := range entries {
		if e.Seq == a.Seq {
			idx = i
			break
		}
	}
	if idx < 0 {
		ar.skipped = true
		ar.problemf(rep, "names audit seq %d, which the log does not hold (%d entries)", a.Seq, len(entries))
		return
	}
	e := entries[idx]
	for _, later := range entries[idx+1:] {
		if later.Seq > a.Seq {
			ar.after++
		}
	}
	if ar.after > 0 {
		rep.noticef("anchor: the audit log holds %d entries after seq %d: %s", ar.after, a.Seq, actionCounts(entries[idx+1:]))
	}
	if e.Hash != a.Hash {
		ar.skipped = true
		ar.problemf(rep, "audit seq %d has hash %s, the anchor records %s (the log up to the anchor was rewritten)", a.Seq, e.Hash, a.Hash)
		return
	}
	if a.CaseID != "" { // a head literal carries neither field
		if a.Created != e.Time {
			ar.problemf(rep, "created %q differs from the time of audit seq %d (%q)", a.Created, a.Seq, e.Time)
		}
		if a.ToolVersion != e.ToolVersion {
			ar.problemf(rep, "tool_version %q differs from audit seq %d (%q)", a.ToolVersion, a.Seq, e.ToolVersion)
		}
	}
	if e.Action != ActionCaseAnchor {
		rep.noticef("anchor: audit seq %d is %s, not %s: record batches not checked", a.Seq, e.Action, ActionCaseAnchor)
		return
	}
	det, err := decodeAnchorDetails(e.Details)
	if err != nil {
		ar.skipped = true
		ar.problemf(rep, "audit seq %d is a %s entry whose details cannot be read: %v", a.Seq, ActionCaseAnchor, err)
		return
	}
	d := BatchSetDigest{SHA256: emptyBatchSetSHA}
	if det.RecordBatches != nil {
		d = *det.RecordBatches
	}
	ar.want, ar.seq = &d, a.Seq
}

// emptyBatchSetSHA is the digest of the empty set; a null record_batches (schema v1) requires it.
var emptyBatchSetSHA = batchSetDigest(nil).SHA256

func decodeAnchorDetails(d map[string]any) (CaseAnchorDetails, error) {
	var out CaseAnchorDetails
	b, err := json.Marshal(d)
	if err != nil {
		return out, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return CaseAnchorDetails{}, err
	}
	return out, nil
}

// checkSignature applies K4.
func (ar *anchorRun) checkSignature(rep *VerifyReport) {
	a := ar.a
	if ar.trusted != nil {
		switch err := a.CheckSignature(ar.trusted); {
		case err == nil:
		case errors.Is(err, ErrAnchorUnsigned):
			ar.problemf(rep, "unsigned, but a trusted key was given")
		case errors.Is(err, ErrAnchorKeyMismatch):
			ar.problemf(rep, "signed by key %s, --anchor-key is %s", embeddedFingerprint(a), KeyFingerprint(ar.trusted))
		default:
			ar.problemf(rep, "signature does not verify")
		}
		return
	}
	if a.Signature == nil {
		rep.noticef("anchor: unsigned: it is only as strong as the place it was stored")
		return
	}
	if err := a.CheckSignature(nil); err != nil {
		ar.problemf(rep, "signature does not verify")
		return
	}
	rep.noticef("anchor: signed by key %s, which is not bound to a trusted key (give --anchor-key to bind it)", embeddedFingerprint(a))
}

func embeddedFingerprint(a AuditAnchor) string {
	if a.Signature != nil {
		if pub, err := hex.DecodeString(a.Signature.PublicKey); err == nil && len(pub) == ed25519.PublicKeySize {
			return KeyFingerprint(pub)
		}
	}
	return "(unreadable)"
}

// actionCounts lists the actions of entries with their counts, sorted, at most maxAnchorActionsListed.
func actionCounts(entries []AuditEntry) string {
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
	}
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	var parts []string
	for i, n := range names {
		if i == maxAnchorActionsListed {
			parts = append(parts, fmt.Sprintf("and %d more actions", len(names)-i))
			break
		}
		parts = append(parts, fmt.Sprintf("%s x%d", n, counts[n]))
	}
	return strings.Join(parts, ", ")
}

// anchorBatchRule is the batch-set rule of A1, run by the record checks once the stored rows are read:
// the rows announced up to the anchored entry must be the set the anchor recorded. rows and audit are
// nil for a schema older than v2.
func (c *Case) anchorBatchRule(rep *VerifyReport, rows []storedBatch, audit *recordAudit) {
	ar := c.verifyAnchor
	if ar == nil || ar.want == nil || ar.done {
		return
	}
	ar.done = true
	got := announcedBatchSet(rows, audit, ar.seq)
	if got != *ar.want {
		ar.problemf(rep, "the record batches announced up to seq %d differ from the set the anchor recorded (%d rows then, %d now): a batch row was removed or altered", ar.seq, ar.want.Count, got.Count)
	}
}

// pathInCase reports whether p lies inside the case directory (symlinks resolved where they exist).
func (c *Case) pathInCase(p string) bool {
	in, err := PathInside(c.Dir, p)
	return err == nil && in
}

// PathInside reports whether p lies inside dir (or is dir). Both are made absolute and the symlinks of
// their longest existing prefix are resolved, so a symlinked parent or a not yet existing file cannot hide
// a path inside the directory. It is the one implementation behind the anchor commands and the verify notice.
func PathInside(dir, p string) (bool, error) {
	d, err := resolvedPath(dir)
	if err != nil {
		return false, err
	}
	q, err := resolvedPath(p)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(d, q)
	if err != nil {
		return false, nil //nolint:nilerr // different volumes: not inside
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}

// resolvedPath is the absolute, lexically clean form of p with the symlinks of its longest existing
// prefix resolved.
func resolvedPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
