package sqlitefile_test

// Engine oracle, damaged write-ahead logs (plan 3I, Task 13): one multi-
// transaction WAL is mutated; every mutated copy is opened by the engine and
// compared with Live() of the same bytes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

type walMutation struct {
	name            string
	db, wal         []byte
	expectLiveError bool // the engine refuses the database and Live() must not present one (ruling: unsupported WAL version)
}

// walBase builds the base WAL: a handful of small transactions on 1 KiB pages.
func walBase(t *testing.T) (db, wal []byte, ps int) {
	t.Helper()
	ps = 1024
	r := newWALRun(t, ps, 0, 0)
	steps := []walStep{
		{"create", []string{"create table a(id integer primary key, v)", "create table b(id integer primary key, v)", "create index ia on a(v)"}},
		{"insert a", []string{rowsInsert("a", 1, 60, 100)}},
		{"insert b", []string{rowsInsert("b", 1, 20, 700)}},
		{"update", []string{"update a set v = v || 'u' where id % 3 = 0"}},
		{"delete", []string{"delete from a where id % 5 = 0"}},
		{"alter", []string{"alter table b add column w default 'dw'", "update b set w = 'x' where id < 5"}},
		{"drop", []string{"drop table b"}},
		{"insert c", []string{"create table c(id integer primary key, v)", rowsInsert("c", 1, 15, 300)}},
	}
	for _, st := range steps {
		r.tx(st.stmts...)
	}
	db, wal = r.snap()
	return db, wal, ps
}

// resealFrom recomputes the checksums of frames k.. so that they chain from the
// previous frame (or the header) as the engine requires.
func resealFrom(wal []byte, ps, k int) {
	big := binary.BigEndian.Uint32(wal[0:])&1 == 1
	slot := 24 + ps
	var s0, s1 uint32
	if k == 0 {
		s0 = binary.BigEndian.Uint32(wal[24:])
		s1 = binary.BigEndian.Uint32(wal[28:])
	} else {
		p := 32 + (k-1)*slot
		s0 = binary.BigEndian.Uint32(wal[p+16:])
		s1 = binary.BigEndian.Uint32(wal[p+20:])
	}
	for i := k; 32+(i+1)*slot <= len(wal); i++ {
		p := 32 + i*slot
		s0, s1 = walSum(wal[p:p+8], big, s0, s1)
		s0, s1 = walSum(wal[p+24:p+slot], big, s0, s1)
		binary.BigEndian.PutUint32(wal[p+16:], s0)
		binary.BigEndian.PutUint32(wal[p+20:], s1)
	}
}

func resealHeader(wal []byte) {
	big := binary.BigEndian.Uint32(wal[0:])&1 == 1
	s0, s1 := walSum(wal[:24], big, 0, 0)
	binary.BigEndian.PutUint32(wal[24:], s0)
	binary.BigEndian.PutUint32(wal[28:], s1)
}

// walMutations lists the mutations of the base WAL. The reduced set cuts at
// every frame boundary and mid-frame and adds about forty byte-level mutations;
// the full set mutates every frame in every way.
func walMutations(db, wal []byte, ps int, full bool) []walMutation {
	slot := 24 + ps
	n := (len(wal) - 32) / slot
	var out []walMutation
	add := func(name string, w []byte) { out = append(out, walMutation{name: name, db: db, wal: w}) }
	clone := func() []byte { return bytes.Clone(wal) }
	fo := func(k int) int { return 32 + k*slot }

	for k := 0; k <= n; k++ {
		add(fmt.Sprintf("truncate at the boundary before frame %d", k), clone()[:fo(k)])
	}
	for k := 0; k < n; k++ {
		if !full && k%4 != 0 {
			continue // the reduced set cuts inside every fourth frame
		}
		add(fmt.Sprintf("truncate inside the header of frame %d", k), clone()[:fo(k)+10])
		add(fmt.Sprintf("truncate inside the page of frame %d", k), clone()[:fo(k)+24+ps/2])
	}
	frames := []int{0, 1, n / 2, n - 2, n - 1}
	if full {
		frames = frames[:0]
		for k := range n {
			frames = append(frames, k)
		}
	}
	for _, k := range frames {
		if k < 0 || k >= n {
			continue
		}
		flip := func(what string, off int, bit byte) {
			w := clone()
			w[fo(k)+off] ^= bit
			add(fmt.Sprintf("frame %d: flip a %s bit", k, what), w)
		}
		flip("data", 24+100, 0x04)
		flip("data (last byte)", 24+ps-1, 0x80)
		flip("salt-1", 8, 0x01)
		flip("salt-2", 12, 0x10)
		flip("checksum-1", 16, 0x01)
		flip("checksum-2", 20, 0x80)
		flip("page number", 3, 0x01)
		flip("commit size", 7, 0x01)
		w := clone()
		clear(w[fo(k) : fo(k)+24])
		add(fmt.Sprintf("frame %d: zero the header", k), w)
		if k+1 < n {
			w = clone()
			a, b := w[fo(k):fo(k)+slot], w[fo(k+1):fo(k+1)+slot]
			tmp := bytes.Clone(a)
			copy(a, b)
			copy(b, tmp)
			add(fmt.Sprintf("swap frames %d and %d", k, k+1), w)
		}
		// re-sealed: the engine accepts the changed page, and so must Live()
		w = clone()
		w[fo(k)+24+ps-3] ^= 0x20
		resealFrom(w, ps, k)
		add(fmt.Sprintf("frame %d: flip a data bit and re-seal the chain", k), w)
		w = clone()
		w[fo(k)+3] ^= 0x01
		resealFrom(w, ps, k)
		add(fmt.Sprintf("frame %d: change the page number and re-seal the chain", k), w)
	}
	w := append(clone(), bytes.Repeat([]byte{0xa5}, 1000)...)
	add("append garbage", w)
	add("append a partial frame header", append(clone(), clone()[32:32+17]...))
	add("append a copy of the first frame", append(clone(), clone()[32:32+slot]...))
	add("append zeros", append(clone(), make([]byte, slot)...))

	hdr := func(name string, mutate func(w []byte), seal bool) {
		w := clone()
		mutate(w)
		if seal {
			resealHeader(w)
		}
		add("header: "+name, w)
	}
	hdr("flip a salt-1 bit", func(w []byte) { w[16] ^= 1 }, false)
	hdr("flip a salt-2 bit", func(w []byte) { w[20] ^= 1 }, false)
	hdr("flip a salt-1 bit, re-sealed", func(w []byte) { w[16] ^= 1 }, true)
	hdr("flip the checkpoint sequence", func(w []byte) { w[15] ^= 1 }, true)
	hdr("flip the endianness bit of the magic", func(w []byte) { w[3] ^= 1 }, false)
	hdr("zero the header", func(w []byte) { clear(w[:32]) }, false)
	hdr("page size 512, unsealed", func(w []byte) { binary.BigEndian.PutUint32(w[8:], 512) }, false)
	hdr("page size 512, re-sealed", func(w []byte) { binary.BigEndian.PutUint32(w[8:], 512) }, true)
	hdr("page size 4096, re-sealed", func(w []byte) { binary.BigEndian.PutUint32(w[8:], 4096) }, true)
	hdr("page size 1000 (not a power of two), re-sealed", func(w []byte) { binary.BigEndian.PutUint32(w[8:], 1000) }, true)
	hdr("version 3007001, unsealed", func(w []byte) { binary.BigEndian.PutUint32(w[4:], 3007001) }, false)
	w = clone()
	binary.BigEndian.PutUint32(w[4:], 3007001)
	resealHeader(w)
	out = append(out, walMutation{name: "header: unsupported version, re-sealed", db: db, wal: w, expectLiveError: true})
	add("empty WAL", nil)
	add("only the header", clone()[:32])
	add("header cut at 31 bytes", clone()[:31])

	// the database file's own header bytes 18/19 (the Task 2 probe: a WAL beside
	// a database that says "rollback")
	for _, v := range [][2]byte{{1, 1}, {2, 1}, {1, 2}, {0, 0}} {
		d := bytes.Clone(db)
		d[18], d[19] = v[0], v[1]
		out = append(out, walMutation{name: fmt.Sprintf("database header bytes 18/19 = %d/%d", v[0], v[1]), db: d, wal: clone()})
	}
	return out
}

// liveDumpRowsErr is liveDumpRows that returns the failure.
func liveDumpRowsErr(t testing.TB, v *sqlitefile.View) (out map[string][]engineRow, err error) {
	t.Helper()
	ft := &failCatcher{TB: t}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(engineFailure); !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", ft.msg)
		}
	}()
	out = liveDumpRows(ft, v)
	return out, nil
}

// runWALMutations compares every mutation with the engine.
func runWALMutations(t *testing.T, full bool) {
	db, wal, ps := walBase(t)
	muts := walMutations(db, wal, ps, full)
	t.Logf("%d mutations of a WAL of %d frames", len(muts), (len(wal)-32)/(24+ps))
	var compared, engineRefused int
	distinct := map[[32]byte]bool{}
	for _, m := range muts {
		eng, err := engineCopyDump(t, "m.db", m.db, m.wal, nil)
		d, oerr := sqlitefile.Open(bytes.NewReader(m.db), int64(len(m.db)), sqlitefile.Options{})
		if oerr != nil {
			t.Errorf("%s: library open: %v", m.name, oerr)
			continue
		}
		var lib map[string][]engineRow
		var lerr error
		if len(m.wal) > 0 {
			if _, aerr := d.AttachWAL(bytes.NewReader(m.wal), int64(len(m.wal))); aerr != nil {
				lerr = aerr
			}
		}
		if lerr == nil {
			v := d.Live()
			lib, lerr = liveDumpRowsErr(t, v)
			v.Release()
		}
		if err != nil {
			engineRefused++
			if m.expectLiveError && lerr == nil {
				t.Errorf("%s: the engine refuses the database (%v) but Live() presents one", m.name, err)
			}
			continue
		}
		if m.expectLiveError {
			t.Errorf("%s: expected the engine to refuse the database", m.name)
		}
		if lerr != nil {
			t.Errorf("%s: the engine reads the copy, Live() fails: %v", m.name, lerr)
			continue
		}
		if diff, _ := diffDumps(lib, eng); diff != "" {
			t.Errorf("%s: %s", m.name, diff)
			continue
		}
		compared++
		distinct[sha256.Sum256([]byte(fmt.Sprint(eng)))] = true
	}
	t.Logf("%d mutations compared equal (%d distinct engine views), %d refused by the engine", compared, len(distinct), engineRefused)
	if len(distinct) < 8 {
		t.Errorf("only %d distinct engine views: the mutations did not change what the engine reads", len(distinct))
	}
	_ = context.Background
}

// TestLiveMatchesEngineWALMutations: the reduced matrix of WAL mutations.
func TestLiveMatchesEngineWALMutations(t *testing.T) { runWALMutations(t, false) }
