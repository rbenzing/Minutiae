package evidence

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"sort"
	"strconv"
)

// RecordTime is one secondary timestamp of a record (a record_times row).
type RecordTime struct {
	Kind        string
	TS          int64
	Basis       string
	TZOffsetMin *int64
}

// RecordRow is a stored record exactly as the digest sees it: every column of
// the records row except batch_id, plus the parser's identity (joined from
// parsers) and the record's times (joined from record_times).
type RecordRow struct {
	ID                        int64
	Type                      string
	PayloadV                  int
	ArtifactID                string
	SourcePath, Locator       *string
	SrcOffset, SrcLength      *int64
	TS, TSEnd                 *int64
	TSBasis                   *string
	TZOffsetMin               *int64
	Deleted, Recovered        bool
	RecoveryMethod            *string
	Confidence                *int64
	ParserName, ParserVersion string
	ParserHash                *string
	Summary                   string
	Body                      *string
	Payload                   string // canonical JSON, exactly as stored
	Times                     []RecordTime
}

// DigestPrefix versions the batch digest and so the whole row encoding below.
// Changing either needs a new prefix and a verify path for the old one.
const DigestPrefix = "minutiae-records-v1\n"

const rollupPrefix = "minutiae-records-rollup-v1\n"

// rowEncoder builds the canonical encoding of one record row.
type rowEncoder struct{ buf []byte }

// null appends the NULL marker.
func (e *rowEncoder) null() { e.buf = append(e.buf, 0x00) }

// str appends a present field: 0x01, uvarint(len), bytes.
func (e *rowEncoder) str(s string) {
	e.buf = append(e.buf, 0x01)
	e.buf = binary.AppendUvarint(e.buf, uint64(len(s)))
	e.buf = append(e.buf, s...)
}

func (e *rowEncoder) optStr(s *string) {
	if s == nil {
		e.null()
		return
	}
	e.str(*s)
}

func (e *rowEncoder) int(n int64) { e.str(strconv.FormatInt(n, 10)) }

func (e *rowEncoder) optInt(n *int64) {
	if n == nil {
		e.null()
		return
	}
	e.int(*n)
}

func (e *rowEncoder) boolean(b bool) {
	if b {
		e.str("1")
		return
	}
	e.str("0")
}

// encodeRow returns the canonical encoding of r bound to artifactSHA256, in the
// field order of the format reference (the batch id is not part of it). The
// times are encoded sorted by kind (byte order), whatever order r holds them in.
func encodeRow(r RecordRow, artifactSHA256 string) []byte {
	var e rowEncoder
	e.int(r.ID)
	e.str(r.Type)
	e.int(int64(r.PayloadV))
	e.str(r.ArtifactID)
	e.str(artifactSHA256)
	e.optStr(r.SourcePath)
	e.optStr(r.Locator)
	e.optInt(r.SrcOffset)
	e.optInt(r.SrcLength)
	e.optInt(r.TS)
	e.optInt(r.TSEnd)
	e.optStr(r.TSBasis)
	e.optInt(r.TZOffsetMin)
	e.boolean(r.Deleted)
	e.boolean(r.Recovered)
	e.optStr(r.RecoveryMethod)
	e.optInt(r.Confidence)
	e.str(r.ParserName)
	e.str(r.ParserVersion)
	e.optStr(r.ParserHash)
	e.str(r.Summary)
	e.optStr(r.Body)
	e.str(r.Payload)

	times := append([]RecordTime(nil), r.Times...)
	sort.SliceStable(times, func(i, j int) bool { return times[i].Kind < times[j].Kind })
	e.int(int64(len(times)))
	for _, t := range times {
		e.str(t.Kind)
		e.int(t.TS)
		e.str(t.Basis)
		e.optInt(t.TZOffsetMin)
	}
	return e.buf
}

// RowDigest is SHA-256 over the canonical encoding of r, bound to the SHA-256 of
// the artifact it was parsed from (artifactSHA256 comes from the manifest; it is
// not a column), so a record is tied to the exact bytes it came from.
func RowDigest(r RecordRow, artifactSHA256 string) [32]byte {
	return sha256.Sum256(encodeRow(r, artifactSHA256))
}

// BatchDigest accumulates the digest of one batch: SHA-256 of DigestPrefix
// followed by uvarint(32) || rowDigest for every row, added in id order.
type BatchDigest struct{ h hash.Hash }

// NewBatchDigest starts a batch digest.
func NewBatchDigest() *BatchDigest {
	h := sha256.New()
	_, _ = h.Write([]byte(DigestPrefix))
	return &BatchDigest{h: h}
}

// Add appends one row digest. Rows must be added in id order.
func (b *BatchDigest) Add(rowDigest [32]byte) {
	_, _ = b.h.Write(binary.AppendUvarint(nil, uint64(len(rowDigest))))
	_, _ = b.h.Write(rowDigest[:])
}

// Sum returns the batch digest as lowercase hex. It does not reset the state.
func (b *BatchDigest) Sum() string { return hex.EncodeToString(b.h.Sum(nil)) }

// IngestRollup is the digest of an ingest: SHA-256 of "minutiae-records-rollup-v1\n"
// followed by uvarint(len(hex)) || hex for every batch digest, in batch_no
// order, as lowercase hex. The rollup of zero batches is the hash of the prefix.
func IngestRollup(batchDigests []string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(rollupPrefix))
	for _, d := range batchDigests {
		_, _ = h.Write(binary.AppendUvarint(nil, uint64(len(d))))
		_, _ = h.Write([]byte(d))
	}
	return hex.EncodeToString(h.Sum(nil))
}
