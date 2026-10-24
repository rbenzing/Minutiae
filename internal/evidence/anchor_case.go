package evidence

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
)

// ActionCaseAnchor is the audit action of an anchor: the anchor names this very entry.
const ActionCaseAnchor = "case.anchor"

// BatchSetDigest digests a set of stored record batches (K7).
type BatchSetDigest struct {
	Count  int64  `json:"count"`
	SHA256 string `json:"sha256"`
}

// CaseAnchorDetails is the details of a case.anchor entry.
type CaseAnchorDetails struct {
	Signed         bool            `json:"signed"`
	KeyFingerprint string          `json:"key_fingerprint"` // "" when unsigned
	RecordBatches  *BatchSetDigest `json:"record_batches"`  // null: schema v1, no record tables
	Out            string          `json:"out"`             // where the CLI writes the anchor file, "" for stdout only
}

// batchSetDigest is the digest of rows (already filtered to the batches announced up to the anchored entry): SHA-256 over
// "ingest_id 0x00 batch_no 0x00 digest 0x0A" per row, rows sorted by (ingest_id, batch_no), and the row count.
func batchSetDigest(rows []storedBatch) BatchSetDigest {
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b storedBatch) int {
		if a.ingest != b.ingest {
			if a.ingest < b.ingest {
				return -1
			}
			return 1
		}
		return a.no - b.no
	})
	h := sha256.New()
	var buf bytes.Buffer
	for _, r := range sorted {
		buf.Reset()
		buf.WriteString(r.ingest)
		buf.WriteByte(0)
		buf.WriteString(strconv.Itoa(r.no))
		buf.WriteByte(0)
		buf.WriteString(r.digest)
		buf.WriteByte('\n')
		_, _ = h.Write(buf.Bytes())
	}
	return BatchSetDigest{Count: int64(len(sorted)), SHA256: hex.EncodeToString(h.Sum(nil))}
}

// announcedBatchSet is the batch set at audit seq upto: the stored rows whose records.batch announcement has a
// seq no greater than upto. A row with no announcement is not in the set (verify reports it on its own).
func announcedBatchSet(rows []storedBatch, audit *recordAudit, upto int64) BatchSetDigest {
	var in []storedBatch
	if audit != nil {
		for _, r := range rows {
			if i, ok := audit.byKey[batchKey{r.ingest, r.no}]; ok && audit.batches[i].Seq <= upto {
				in = append(in, r)
			}
		}
	}
	return batchSetDigest(in)
}

// Anchor appends ONE case.anchor entry and returns the anchor naming that entry, signed when signer is not nil.
// It recomputes the audit chain first and appends only if the chain is intact, the records entries it digests
// decode and the log's last entry is the open log's head (all checked under one hold of the log's lock): any
// failure is ErrIntegrity and nothing is appended. The entry is audited BEFORE the anchor exists anywhere; an
// audit failure returns no anchor. Anchoring is an act of record, not a verdict: it does not run the full verify.
func (c *Case) Anchor(ctx context.Context, signer ed25519.PrivateKey, out string) (AuditAnchor, error) {
	if signer != nil && len(signer) != ed25519.PrivateKeySize {
		return AuditAnchor{}, errors.New("anchor: the signing key is not an Ed25519 private key")
	}
	_, entries, problems, err := verifyAudit(filepath.Join(c.Dir, auditFile))
	if err != nil {
		return AuditAnchor{}, fmt.Errorf("anchor: audit log unreadable: %w", err)
	}
	if len(problems) > 0 {
		return AuditAnchor{}, fmt.Errorf("%w: the audit log does not verify, so it is not anchored: %s", ErrIntegrity, problems[0])
	}
	head := entries[len(entries)-1]
	batches, err := c.anchorBatches(ctx, entries)
	if err != nil {
		return AuditAnchor{}, err
	}
	det := CaseAnchorDetails{Signed: signer != nil, RecordBatches: batches, Out: out}
	if signer != nil {
		det.KeyFingerprint = KeyFingerprint(signer.Public().(ed25519.PublicKey))
	}
	e, err := c.Audit.appendLocked(func(seq int64, last string) error {
		if seq != head.Seq || last != head.Hash {
			return fmt.Errorf("%w: the audit log changed while the anchor was prepared (head seq %d, read %d)", ErrIntegrity, seq, head.Seq)
		}
		return nil
	}, ActionCaseAnchor, "", detailsOf(det))
	if err != nil {
		return AuditAnchor{}, fmt.Errorf("anchor: %w", err)
	}
	a := AuditAnchor{Format: AnchorFormat, CaseID: c.Meta.ID, Seq: e.Seq, Hash: e.Hash, Created: e.Time, ToolVersion: e.ToolVersion}
	if signer != nil {
		a = SignAnchor(a, signer)
	}
	return a, nil
}

// anchorBatches is the batch set a new anchor commits to: every stored, announced row. nil for a schema older
// than v2 (no record tables). Any problem reading the announcements or the rows is ErrIntegrity.
func (c *Case) anchorBatches(ctx context.Context, entries []AuditEntry) (*BatchSetDigest, error) {
	dbv, err := c.store.SchemaVersion()
	if err != nil {
		return nil, fmt.Errorf("anchor: schema version unreadable: %w", err)
	}
	if dbv < 2 {
		return nil, nil
	}
	rep := &VerifyReport{}
	ps := &problemSet{rep: rep, counts: map[string]int{}}
	audit := readRecordAudit(entries, ps)
	rows, ok := c.readStoredBatches(ctx, rep, ps)
	if rep.cancelCut {
		return nil, fmt.Errorf("anchor: %w", ctx.Err())
	}
	if !ok || len(rep.Problems) > 0 {
		first := "the record batches could not be read"
		if len(rep.Problems) > 0 {
			first = rep.Problems[0]
		}
		return nil, fmt.Errorf("%w: the record batches do not verify, so the case is not anchored: %s", ErrIntegrity, first)
	}
	d := announcedBatchSet(rows, audit, entries[len(entries)-1].Seq)
	return &d, nil
}
