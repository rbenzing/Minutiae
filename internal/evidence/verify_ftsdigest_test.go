package evidence

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

type digestPosting struct {
	table int
	b     ftsBucketKey
	term  []byte
	rowid int64
	col   int
	off   int64
}

func randomPostings(n int, seed int64) []digestPosting {
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // a seeded generator: the test must be reproducible
	buckets := []ftsBucketKey{{batch: 0}, {batch: 1}, {batch: 7}, {batch: -1, block: 3}}
	out := make([]digestPosting, n)
	for i := range out {
		term := make([]byte, rng.Intn(40))
		for j := range term {
			term[j] = byte('a' + rng.Intn(26))
		}
		out[i] = digestPosting{rng.Intn(2), buckets[rng.Intn(len(buckets))], term, rng.Int63n(1 << 40), rng.Intn(2), rng.Int63n(1000)}
	}
	return out
}

func digestOf(t *testing.T, key [16]byte, ps []digestPosting) *ftsDigester {
	t.Helper()
	d, err := newFTSDigester(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		d.add(p.table, p.b, p.term, p.rowid, p.col, p.off)
	}
	return d
}

func sumsOf(d *ftsDigester) [2]map[ftsBucketKey]ftsSum {
	var out [2]map[ftsBucketKey]ftsSum
	for t := range d.sums {
		out[t] = map[ftsBucketKey]ftsSum{}
		for k, s := range d.sums[t] {
			out[t][k] = *s
		}
	}
	return out
}

func TestFTSDigestIsOrderIndependentAndCountsDuplicates(t *testing.T) {
	var key [16]byte
	copy(key[:], "0123456789abcdef")
	ps := randomPostings(1000, 1)
	a := digestOf(t, key, ps)
	rev := make([]digestPosting, len(ps))
	for i, p := range ps {
		rev[len(ps)-1-i] = p
	}
	rand.New(rand.NewSource(2)).Shuffle(len(rev), func(i, j int) { rev[i], rev[j] = rev[j], rev[i] }) //nolint:gosec // a seeded generator: the test must be reproducible
	b := digestOf(t, key, rev)
	if !reflect.DeepEqual(sumsOf(a), sumsOf(b)) {
		t.Fatal("the same postings in two orders gave different sums")
	}
	// one posting added twice: a different sum and a different count (an XOR would cancel it)
	c := digestOf(t, key, append(slicesClone(ps), ps[17]))
	if reflect.DeepEqual(sumsOf(a), sumsOf(c)) {
		t.Fatal("a duplicated posting did not change the digest")
	}
	k := ps[17].b
	if got, want := c.sums[ps[17].table][k].n, a.sums[ps[17].table][k].n+1; got != want {
		t.Fatalf("count after a duplicate = %d, want %d", got, want)
	}
	// the same posting twice is not the same as the posting once, and twice-with-nothing cancels nothing
	d := digestOf(t, key, append(slicesClone(ps), ps[17], ps[17]))
	if reflect.DeepEqual(sumsOf(c), sumsOf(d)) {
		t.Fatal("a triplicated posting equals a duplicated one")
	}
	// a pair of duplicates changes the sum itself (not only the count): an XOR would cancel the pair
	pair := digestOf(t, key, append(slicesClone(ps), ps[17], ps[17]))
	for tb := range pair.sums {
		for k, s := range pair.sums[tb] {
			o := a.sums[tb][k]
			if k == ps[17].b && tb == ps[17].table && s.hi == o.hi && s.lo == o.lo {
				t.Fatal("a posting added twice more left the 128-bit sum unchanged: the digest cancels pairs")
			}
		}
	}
	// another key: another sum
	var key2 [16]byte
	copy(key2[:], "fedcba9876543210")
	e := digestOf(t, key2, ps)
	if reflect.DeepEqual(sumsOf(a), sumsOf(e)) {
		t.Fatal("two keys gave the same sums")
	}
	// the digest sees every field of a posting
	base := digestPosting{0, ftsBucketKey{batch: 0}, []byte("term"), 5, 0, 3}
	ref := sumsOf(digestOf(t, key, []digestPosting{base}))
	for name, mut := range map[string]func(p *digestPosting){
		"table": func(p *digestPosting) { p.table = 1 },
		"term":  func(p *digestPosting) { p.term = []byte("terN") },
		"rowid": func(p *digestPosting) { p.rowid = 6 },
		"col":   func(p *digestPosting) { p.col = 1 },
		"off":   func(p *digestPosting) { p.off = 4 },
	} {
		m := base
		m.term = bytes.Clone(base.term)
		mut(&m)
		got := sumsOf(digestOf(t, key, []digestPosting{m}))
		// the bucket and table are where the sum is kept: compare the MAC through the single sum of the bucket
		if name == "table" {
			if got[1][m.b] == ref[0][m.b] {
				t.Errorf("the table is not part of the digested message")
			}
			continue
		}
		if got[0][m.b] == ref[0][m.b] {
			t.Errorf("the %s is not part of the digested message", name)
		}
	}
}

func slicesClone(ps []digestPosting) []digestPosting { return append([]digestPosting(nil), ps...) }

// the digester is the CBC-MAC of the message, computed here the long way
func TestFTSDigestEncodingIsPrefixFree(t *testing.T) {
	var key [16]byte
	copy(key[:], "0123456789abcdef")
	blk, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatal(err)
	}
	var msgs [][]byte
	for _, n := range []int{0, 13, 14, 29, 30, 31, 45, 46} {
		for table := range 2 {
			term := bytes.Repeat([]byte{'x'}, n)
			m := appendFTSMessage(nil, table, term, 7, 1, 9)
			wantBlocks := 2 + (max(n-13, 0)+15)/16
			if len(m)%16 != 0 || len(m) != 16*wantBlocks {
				t.Errorf("term of %d bytes: len(M) = %d, want %d", n, len(m), 16*wantBlocks)
			}
			msgs = append(msgs, m)
			// the digester computes this message's CBC-MAC
			var st [16]byte
			for i := 0; i < len(m); i += 16 {
				for j := range st {
					st[j] ^= m[i+j]
				}
				blk.Encrypt(st[:], st[:])
			}
			d := digestOf(t, key, []digestPosting{{table, ftsBucketKey{batch: 3}, term, 7, 1, 9}})
			s := d.sums[table][ftsBucketKey{batch: 3}]
			if s.hi != binary.BigEndian.Uint64(st[:8]) || s.lo != binary.BigEndian.Uint64(st[8:]) || s.n != 1 {
				t.Errorf("term of %d bytes, table %d: the digester's MAC is not the CBC-MAC of the message", n, table)
			}
		}
	}
	for i, a := range msgs {
		for j, b := range msgs {
			if i != j && !bytes.Equal(a, b) && bytes.HasPrefix(a, b) {
				t.Errorf("message %d is a prefix of message %d", j, i)
			}
		}
	}
	// the cached term chain does not leak between terms or tables
	d, _ := newFTSDigester(key)
	d.add(0, ftsBucketKey{}, []byte("abc"), 1, 0, 0)
	d.add(1, ftsBucketKey{}, []byte("abc"), 1, 0, 0)
	d.add(0, ftsBucketKey{}, []byte("abd"), 1, 0, 0)
	e, _ := newFTSDigester(key)
	e.add(0, ftsBucketKey{}, []byte("abd"), 1, 0, 0)
	if *d.sums[0][ftsBucketKey{}] == *e.sums[0][ftsBucketKey{}] {
		t.Error("the chain cache returned a stale value")
	}
	// the widest term the decoder can produce
	long := bytes.Repeat([]byte{0xAB}, fts5MaxTokenSize)
	if m := appendFTSMessage(nil, 0, long, 1, 0, 0); len(m)%16 != 0 {
		t.Errorf("len(M) of a %d-byte term = %d", len(long), len(m))
	}
	if _, err := newFTSDigester(key); err != nil {
		t.Fatal(err)
	}
}

func TestFTSBucketOfMatchesFindBatch(t *testing.T) {
	mk := func(first int64, count int) auditedBatch {
		return auditedBatch{BatchCommit: BatchCommit{FirstID: first, Count: count}}
	}
	sorted := []auditedBatch{mk(10, 5), mk(20, 10)}
	for _, tc := range []struct {
		id   int64
		want ftsBucketKey
	}{
		{10, ftsBucketKey{batch: 0}},
		{14, ftsBucketKey{batch: 0}},
		{15, ftsBucketKey{batch: -1, block: 0}},
		{19, ftsBucketKey{batch: -1, block: 0}}, // between batches
		{20, ftsBucketKey{batch: 1}},
		{29, ftsBucketKey{batch: 1}},
		{30, ftsBucketKey{batch: -1, block: 0}},
		{1 << 20, ftsBucketKey{batch: -1, block: 16}}, // above the last
		{9, ftsBucketKey{batch: -1, block: 0}},
		{-5, ftsBucketKey{batch: -1, block: -1}}, // below the first
		{math.MaxInt64, ftsBucketKey{batch: -1, block: math.MaxInt64 >> 16}},
		{math.MinInt64, ftsBucketKey{batch: -1, block: math.MinInt64 >> 16}},
	} {
		if got := ftsBucketOf(sorted, tc.id); got != tc.want {
			t.Errorf("ftsBucketOf(%d) = %+v, want %+v", tc.id, got, tc.want)
		}
		i, in := findBatch(sorted, tc.id)
		if in != (tc.want.batch >= 0) || (in && i != tc.want.batch) {
			t.Errorf("findBatch(%d) = %d, %v disagrees with the bucket %+v", tc.id, i, in, tc.want)
		}
	}
	for _, id := range []int64{0, 1, 65535, 65536, -1, 1 << 40} {
		if got, want := ftsBucketOf(nil, id), (ftsBucketKey{batch: -1, block: id >> 16}); got != want {
			t.Errorf("ftsBucketOf(nil, %d) = %+v, want %+v", id, got, want)
		}
	}
	// the order of buckets: audited batches first, then the outside blocks
	want := []ftsBucketKey{{batch: 0}, {batch: 3}, {batch: -1, block: -1}, {batch: -1, block: 2}}
	for i, a := range want {
		for j, b := range want {
			if ftsBucketLess(a, b) != (i < j) {
				t.Errorf("ftsBucketLess(%+v, %+v) is wrong", a, b)
			}
		}
	}
}

// TestFTSDocsizeAndTotalsEncodingsMatchSQLite: on every genuine workload (no deleted documents), the
// _docsize blob and the totals record SQLite wrote are what the encodings of the recomputation give from the
// token counts that fts5vocab shows.
func TestFTSDocsizeAndTotalsEncodingsMatchSQLite(t *testing.T) {
	for name, load := range genuineFTSWorkloads() {
		t.Run(name, func(t *testing.T) {
			db := genuineFTSWorkload(t, name, load)
			for _, table := range FTSTables() {
				ensureVocab(t, db, table)
				counts := map[int64]*[2]uint64{}
				rows, err := db.Query("SELECT doc, col, count(*) FROM " + ftsVocabName(table) + " GROUP BY doc, col") //nolint:gosec // a constant table name in a test
				if err != nil {
					t.Fatal(err)
				}
				for rows.Next() {
					var doc int64
					var col string
					var n uint64
					if err := rows.Scan(&doc, &col, &n); err != nil {
						t.Fatal(err)
					}
					if counts[doc] == nil {
						counts[doc] = &[2]uint64{}
					}
					counts[doc][ftsVocabCols[col]] = n
				}
				_ = rows.Close()
				sz, err := db.Query("SELECT id, sz FROM " + table + "_docsize ORDER BY id") //nolint:gosec // a constant table name in a test
				if err != nil {
					t.Fatal(err)
				}
				ex := &ftsExpect{}
				for sz.Next() {
					var id int64
					var blob []byte
					if err := sz.Scan(&id, &blob); err != nil {
						t.Fatal(err)
					}
					c := counts[id]
					if c == nil {
						c = &[2]uint64{}
					}
					want := appendSQLiteVarint(appendSQLiteVarint(nil, c[0]), c[1])
					if !bytes.Equal(blob, want) {
						t.Fatalf("%s doc %d: _docsize is x'%x', the encoding gives x'%x'", table, id, blob, want)
					}
					ex.docs++
					ex.tokens[ftsTableIndex(table)][0] += c[0]
					ex.tokens[ftsTableIndex(table)][1] += c[1]
				}
				_ = sz.Close()
				var totals []byte
				if err := db.QueryRow("SELECT block FROM " + table + "_data WHERE id = 1").Scan(&totals); err != nil {
					t.Fatal(err)
				}
				if got := ex.expectedTotals(ftsTableIndex(table)); !bytes.Equal(totals, got) {
					t.Fatalf("%s: the totals record is x'%x', the encoding gives x'%x' (%d documents)", table, totals, got, ex.docs)
				}
			}
		})
	}
	// a table without a document holds an empty totals record, as a fresh table and 'delete-all' leave it
	if got := (&ftsExpect{}).expectedTotals(0); got == nil || len(got) != 0 {
		t.Errorf("the totals of no document = %v, want an empty record", got)
	}
}
