package evidence

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"math/bits"
)

// The postings digest of P11 (5D-14i, plan decision V6). Both sides of the comparison, the postings the
// records produce (replica tokenizers, in the record stream) and the postings the leaf pages hold (the
// strict decoder, in the index pass), are folded into one 128-bit sum per (table, bucket). The two sums
// of a bucket are equal exactly when the two posting sets are equal, up to the forgery bound below.
//
// A posting (table, term, rowid, column, position) is mapped to a 128-bit value by a keyed PRF: the
// AES-128 CBC-MAC of a prefix-free message (the length of the message is fixed by its first block, so
// no message is a prefix of another, which is what makes CBC-MAC a PRF). The key is 16 bytes from
// crypto/rand, new for every verify run, held in memory only and never stored or logged, so the values
// cannot be predicted by whoever wrote the case. The values of a bucket are summed modulo 2^128 and
// counted: a sum, unlike an XOR, is not cancelled by a posting that appears twice, and the count stands
// beside it. A forged bucket (a different posting set with the same sum and count) needs a collision of
// a PRF that the author of the case cannot query: the bound is about 2^-64 per bucket (limitations.md).
//
// The message M of a posting, in 16-byte blocks:
//
//	B0   = u16be len(term) | u8 table | the first 13 bytes of the term (zero padded)
//	B1.. = the rest of the term in 16-byte blocks (zero padded)
//	Bz   = u64be rowid | u32be position | u8 column | 3 zero bytes
//
// mac = CBC-MAC(key, M) with the IV 0. The chain after the term blocks is cached while the term repeats
// (the index side visits the postings in term order). A term is at most fts5MaxTokenSize bytes (the
// tokenizers cut there and the decoder refuses a longer one), so the 16-bit length never wraps; add
// clamps a longer term rather than index past it, which no caller can reach. Positions are 32-bit
// varints in SQLite and the decoder refuses larger ones.

// ftsSum is a sum modulo 2^128 (hi, lo) of the MACs of the postings of a bucket, and their number.
type ftsSum struct{ hi, lo, n uint64 }

func (s *ftsSum) addMAC(mac *[16]byte) {
	var c uint64
	s.lo, c = bits.Add64(s.lo, binary.BigEndian.Uint64(mac[8:]), 0)
	s.hi, _ = bits.Add64(s.hi, binary.BigEndian.Uint64(mac[:8]), c)
	s.n++
}

func (s *ftsSum) addSum(o ftsSum) {
	var c uint64
	s.lo, c = bits.Add64(s.lo, o.lo, 0)
	s.hi, _ = bits.Add64(s.hi, o.hi, c)
	s.n += o.n
}

// ftsBucketKey names a bucket: batch >= 0 is the index in the sorted audited batches, batch == -1 is the
// "outside" block of the ids no audited batch holds (block = id >> 16).
type ftsBucketKey struct {
	batch int
	block int64
}

// ftsBucketOf is the bucket of a record id (V7). Both sides use this one function.
func ftsBucketOf(sorted []auditedBatch, id int64) ftsBucketKey {
	if i, ok := findBatch(sorted, id); ok {
		return ftsBucketKey{batch: i}
	}
	return ftsBucketKey{batch: -1, block: id >> 16}
}

// ftsBucketLess orders buckets the way the audit log lists the batches: the audited batches first, then
// the outside blocks by id.
func ftsBucketLess(a, b ftsBucketKey) bool {
	switch {
	case a.batch >= 0 && b.batch >= 0:
		return a.batch < b.batch
	case a.batch >= 0:
		return true
	case b.batch >= 0:
		return false
	}
	return a.block < b.block
}

// ftsDigester holds the per-table, per-bucket sums of one side.
type ftsDigester struct {
	block cipher.Block
	sums  [2]map[ftsBucketKey]*ftsSum

	// the cached chain after the term blocks
	haveChain  bool
	chainTable int
	chainTerm  []byte
	chain      [16]byte
	// the bucket sum used last
	haveLast  bool
	lastTable int
	lastKey   ftsBucketKey
	lastSum   *ftsSum
}

func newFTSDigester(key [16]byte) (*ftsDigester, error) {
	b, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return &ftsDigester{block: b, sums: [2]map[ftsBucketKey]*ftsSum{{}, {}}}, nil
}

// fork returns a digester with the same key and no sums: one segment's contribution, merged into the
// table's digester only when the whole segment decoded (the ftsSink contract, D147).
func (d *ftsDigester) fork() *ftsDigester {
	return &ftsDigester{block: d.block, sums: [2]map[ftsBucketKey]*ftsSum{{}, {}}}
}

// merge adds the sums of o into d.
func (d *ftsDigester) merge(o *ftsDigester) {
	for t := range o.sums {
		for k, s := range o.sums[t] {
			d.sum(t, k).addSum(*s)
		}
	}
	d.haveLast = false
}

func (d *ftsDigester) sum(table int, k ftsBucketKey) *ftsSum {
	if d.haveLast && d.lastTable == table && d.lastKey == k {
		return d.lastSum
	}
	s := d.sums[table][k]
	if s == nil {
		s = &ftsSum{}
		d.sums[table][k] = s
	}
	d.haveLast, d.lastTable, d.lastKey, d.lastSum = true, table, k, s
	return s
}

// termChain returns the CBC-MAC chain value after the term blocks (B0 and the rest of the term).
func (d *ftsDigester) termChain(table int, term []byte) [16]byte {
	if len(term) > 0xFFFF {
		term = term[:0xFFFF]
	}
	if d.haveChain && d.chainTable == table && bytes.Equal(d.chainTerm, term) {
		return d.chain
	}
	var st, blk [16]byte
	blk[0], blk[1], blk[2] = byte(len(term)>>8), byte(len(term)), byte(table)
	n := copy(blk[3:], term)
	d.block.Encrypt(st[:], blk[:]) // the IV is 0, so the chain after B0 is E(B0)
	for rest := term[n:]; len(rest) > 0; {
		blk = [16]byte{}
		k := copy(blk[:], rest)
		rest = rest[k:]
		for i := range st {
			st[i] ^= blk[i]
		}
		d.block.Encrypt(st[:], st[:])
	}
	d.haveChain, d.chainTable, d.chain = true, table, st
	d.chainTerm = append(d.chainTerm[:0], term...)
	return st
}

// add folds one posting into the sum of its bucket. table is 0 for the word index and 1 for the
// substring index; col is 0 for the summary and 1 for the body.
func (d *ftsDigester) add(table int, b ftsBucketKey, term []byte, rowid int64, col int, off int64) {
	st := d.termChain(table, term)
	var blk [16]byte
	binary.BigEndian.PutUint64(blk[:8], uint64(rowid))
	binary.BigEndian.PutUint32(blk[8:12], uint32(off))
	blk[12] = byte(col)
	for i := range st {
		st[i] ^= blk[i]
	}
	d.block.Encrypt(st[:], st[:])
	d.sum(table, b).addMAC(&st)
}

// appendFTSMessage appends the message M of a posting (the specification above, written out in full; the
// digester computes the same chain incrementally). Tests use it as the reference.
func appendFTSMessage(dst []byte, table int, term []byte, rowid int64, col int, off int64) []byte {
	if len(term) > 0xFFFF {
		term = term[:0xFFFF]
	}
	var b0 [16]byte
	b0[0], b0[1], b0[2] = byte(len(term)>>8), byte(len(term)), byte(table)
	n := copy(b0[3:], term)
	dst = append(dst, b0[:]...)
	for rest := term[n:]; len(rest) > 0; {
		var blk [16]byte
		k := copy(blk[:], rest)
		rest = rest[k:]
		dst = append(dst, blk[:]...)
	}
	var bz [16]byte
	binary.BigEndian.PutUint64(bz[:8], uint64(rowid))
	binary.BigEndian.PutUint32(bz[8:12], uint32(off))
	bz[12] = byte(col)
	return append(dst, bz[:]...)
}
