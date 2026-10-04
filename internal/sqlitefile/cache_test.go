package sqlitefile_test

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func fakePages(n, size int) *sqlitefile.FakeSource {
	src := sqlitefile.NewFakeSource()
	for i := 1; i <= n; i++ {
		p := make([]byte, size)
		for j := range p {
			p[j] = byte(i)
		}
		src.Pages[uint32(i)] = p
	}
	return src
}

func TestPageCacheLRUAndBudget(t *testing.T) {
	const ps = 512
	src := fakePages(10, ps)
	budget := newRecBudget(1 << 20)
	env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget, Limits: sqlitefile.Limits{PageCacheBytes: 4 * ps}})
	c := env.NewTestCache(src, ps)
	read := func(pg uint32) {
		t.Helper()
		d, loc, err := c.Read(pg)
		if err != nil || len(d) != ps || d[0] != byte(pg) {
			t.Fatalf("Read(%d): %d bytes, %v", pg, len(d), err)
		}
		if loc.File != sqlitefile.FileDB || loc.Offset != int64(pg-1)*ps {
			t.Fatalf("Read(%d): loc %+v", pg, loc)
		}
	}
	for pg := uint32(1); pg <= 4; pg++ {
		read(pg)
	}
	if s := c.Stats(); s.PageReads != 4 || s.CacheHits != 0 {
		t.Fatalf("after 4 distinct pages: %+v", s)
	}
	if budget.used != 4*ps {
		t.Errorf("4 cached pages are charged %d bytes, want %d", budget.used, 4*ps)
	}
	read(1) // a hit; page 2 is now the least recently used
	if s := c.Stats(); s.PageReads != 4 || s.CacheHits != 1 {
		t.Errorf("a cached page must be a hit: %+v", s)
	}
	read(5) // evicts page 2
	if budget.used != 4*ps {
		t.Errorf("the cache keeps at most 4 pages: %d bytes charged", budget.used)
	}
	read(1)
	read(3)
	read(4)
	read(5) // all still cached
	if s := c.Stats(); s.PageReads != 5 || s.CacheHits != 5 {
		t.Errorf("pages 1, 3, 4, 5 must be cached: %+v", s)
	}
	read(2) // was evicted: a new read, and it evicts page 1 (the least recently used)
	if s := c.Stats(); s.PageReads != 6 {
		t.Errorf("an evicted page must be read again: %+v", s)
	}
	if len(src.Reads) != 6 {
		t.Errorf("the source saw %d reads, want 6: %v", len(src.Reads), src.Reads)
	}
	before := c.Stats().PageReads
	read(1)
	if c.Stats().PageReads != before+1 {
		t.Error("page 1 was the least recently used and must have been evicted")
	}
	c.Clear()
	budget.check(t)
	if budget.peak > 5*ps { // the cache plus the page being read
		t.Errorf("peak %d exceeds the cache size %d plus one page", budget.peak, 4*ps)
	}
}

func TestPageCacheEdgeCases(t *testing.T) {
	const ps = 512
	t.Run("a page larger than the cache is read through", func(t *testing.T) {
		budget := newRecBudget(1 << 20)
		env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget, Limits: sqlitefile.Limits{PageCacheBytes: 100}})
		c := env.NewTestCache(fakePages(2, ps), ps)
		for i := 0; i < 3; i++ {
			if _, _, err := c.Read(1); err != nil {
				t.Fatal(err)
			}
		}
		if s := c.Stats(); s.PageReads != 3 || s.CacheHits != 0 {
			t.Errorf("stats %+v", s)
		}
		budget.check(t)
		if budget.peak != ps {
			t.Errorf("the read itself is charged: peak %d", budget.peak)
		}
	})
	t.Run("a refusing budget gives ErrBudget and keeps nothing", func(t *testing.T) {
		budget := newRecBudget(ps - 1)
		env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})
		src := fakePages(2, ps)
		c := env.NewTestCache(src, ps)
		if _, _, err := c.Read(1); !errors.Is(err, sqlitefile.ErrBudget) {
			t.Errorf("err = %v, want ErrBudget", err)
		}
		if len(src.Reads) != 0 {
			t.Error("the charge comes before the read")
		}
		budget.check(t)
	})
	t.Run("errors are not cached and give their charge back", func(t *testing.T) {
		budget := newRecBudget(1 << 20)
		env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})
		src := fakePages(2, ps)
		boom := errors.New("disk on fire")
		src.Errs[1] = boom
		src.Errs[2] = fmt.Errorf("gone: %w", sqlitefile.ErrPageUnavailable)
		c := env.NewTestCache(src, ps)
		if _, _, err := c.Read(1); !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
		if _, _, err := c.Read(2); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("err = %v", err)
		}
		budget.check(t)
		delete(src.Errs, 1)
		if d, _, err := c.Read(1); err != nil || len(d) != ps {
			t.Errorf("a failed read must not poison the cache: %v", err)
		}
		c.Clear()
		budget.check(t)
	})
	t.Run("a short page is kept as it is", func(t *testing.T) {
		src := fakePages(1, ps)
		src.Pages[1] = src.Pages[1][:100]
		c := sqlitefile.NewTestEnv(sqlitefile.Options{}).NewTestCache(src, ps)
		if d, _, err := c.Read(1); err != nil || len(d) != 100 {
			t.Errorf("(%d bytes, %v)", len(d), err)
		}
	})
	t.Run("concurrent readers", func(t *testing.T) {
		budget := newRecBudget(1 << 20)
		env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget, Limits: sqlitefile.Limits{PageCacheBytes: 3 * ps}})
		c := env.NewTestCache(fakePages(8, ps), ps)
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 300; i++ {
					pg := uint32((i*7+g)%8 + 1)
					if d, _, err := c.Read(pg); err != nil || d[0] != byte(pg) {
						t.Errorf("Read(%d): %v", pg, err)
						return
					}
				}
			}()
		}
		wg.Wait()
		c.Clear()
		budget.check(t)
	})
}

func TestPageSetMarksOnce(t *testing.T) {
	budget := newRecBudget(1 << 20)
	env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})
	l := env.Ledger()
	v, err := env.NewPageSetVisited(l, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, pg := range []uint32{1, 2, 63, 64, 65, 999, 1000} {
		if !v.Mark(pg) {
			t.Errorf("first mark of %d must report true", pg)
		}
		if v.Mark(pg) {
			t.Errorf("second mark of %d must report false", pg)
		}
	}
	for _, pg := range []uint32{0, 1001, 4294967295} {
		if v.Mark(pg) {
			t.Errorf("page %d is outside 1..1000 and must never report first", pg)
		}
	}
	if budget.used < 1000/8 || budget.used > 1000/8+64 {
		t.Errorf("the set is charged %d bytes for 1000 pages", budget.used)
	}
	v.Release(l)
	budget.check(t)
	// A huge addressable count is charged up front and can be refused.
	small := newRecBudget(1 << 10)
	env2 := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: small})
	if _, err := env2.NewPageSetVisited(env2.Ledger(), 1<<25); !errors.Is(err, sqlitefile.ErrBudget) {
		t.Errorf("err = %v, want ErrBudget", err)
	}
	small.check(t)
}

func TestMapVisitorIsBounded(t *testing.T) {
	budget := newRecBudget(1 << 24)
	env := sqlitefile.NewTestEnv(sqlitefile.Options{Budget: budget})
	l := env.Ledger()
	v, err := env.NewMapVisited(l, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Mark(10) || !v.Mark(20) || !v.Mark(30) {
		t.Fatal("the first three pages fit")
	}
	if v.Mark(10) || v.Mark(40) {
		t.Error("a repeat, and a page past the capacity, must report false")
	}
	v.Release(l)
	budget.check(t)
	// The capacity itself is bounded whatever is asked for.
	v, err = env.NewMapVisited(l, 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if budget.used > 1<<22 {
		t.Errorf("a visitor for 2^40 pages charged %d bytes", budget.used)
	}
	v.Release(l)
	budget.check(t)
}

func TestDBSourceReadsPagesAsFound(t *testing.T) {
	b := sqlitetestNew(512)
	file := b.Bytes()
	db, err := sqlitefile.Open(bytes.NewReader(file), int64(len(file)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := db.DBCache()
	for pg := uint32(1); pg <= db.Info().PageCount; pg++ {
		d, loc, err := c.Read(pg)
		if err != nil {
			t.Fatal(err)
		}
		if want := file[(pg-1)*512 : pg*512]; !bytes.Equal(d, want) {
			t.Errorf("page %d differs from the file", pg)
		}
		if loc != (sqlitefile.PageLoc{File: sqlitefile.FileDB, Offset: int64(pg-1) * 512}) {
			t.Errorf("page %d: loc %+v", pg, loc)
		}
	}
	for _, pg := range []uint32{0, db.Info().PageCount + 1, 1 << 31} {
		if _, _, err := c.Read(pg); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("page %d: %v, want ErrPageUnavailable", pg, err)
		}
	}
	// A page read twice is read from the file once.
	before := c.Stats().PageReads
	if _, _, err := c.Read(2); err != nil {
		t.Fatal(err)
	}
	if c.Stats().PageReads != before {
		t.Error("a cached page must not be read again")
	}
}
