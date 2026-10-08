package parse

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type countingReader struct {
	r     io.ReaderAt
	calls atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

type readerFunc func(p []byte, off int64) (int, error)

func (f readerFunc) ReadAt(p []byte, off int64) (int, error) { return f(p, off) }

func seq(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func TestSealedReaderAt(t *testing.T) {
	t.Run("reads pass through", func(t *testing.T) {
		data := seq(100)
		s := NewSealedReaderAt(bytes.NewReader(data), 0)
		p := make([]byte, 10)
		n, err := s.ReadAt(p, 20)
		if n != 10 || err != nil || !bytes.Equal(p, data[20:30]) {
			t.Fatalf("ReadAt = %d, %v, %v", n, err, p)
		}
		n, err = s.ReadAt(p, 95)
		if n != 5 || !errors.Is(err, io.EOF) {
			t.Fatalf("short read = %d, %v, want 5, EOF", n, err)
		}
		if n, err = s.ReadAt(p, 100); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read at end = %d, %v", n, err)
		}
	})
	t.Run("unlimited when the probe limit is 0", func(t *testing.T) {
		s := NewSealedReaderAt(bytes.NewReader(seq(1000)), 0)
		p := make([]byte, 1000)
		for range 5 {
			if n, err := s.ReadAt(p, 0); n != 1000 || err != nil {
				t.Fatalf("ReadAt = %d, %v", n, err)
			}
		}
	})
	t.Run("probe limit across calls and a read crossing it is cut", func(t *testing.T) {
		data := seq(100)
		s := NewSealedReaderAt(bytes.NewReader(data), 10)
		p := make([]byte, 4)
		for i := range 2 {
			if n, err := s.ReadAt(p, int64(i*4)); n != 4 || err != nil {
				t.Fatalf("read %d = %d, %v", i, n, err)
			}
		}
		n, err := s.ReadAt(p, 8)
		if n != 2 || !errors.Is(err, ErrProbeLimit) || !bytes.Equal(p[:2], data[8:10]) {
			t.Fatalf("crossing read = %d, %v, %v; want 2 bytes and ErrProbeLimit", n, err, p[:n])
		}
		if n, err = s.ReadAt(p, 0); n != 0 || !errors.Is(err, ErrProbeLimit) {
			t.Fatalf("read past the limit = %d, %v", n, err)
		}
	})
	t.Run("probe limit is exact under concurrency", func(t *testing.T) {
		s := NewSealedReaderAt(bytes.NewReader(seq(1000)), 50)
		var total atomic.Int64
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				p := make([]byte, 10)
				for range 5 {
					n, _ := s.ReadAt(p, 0)
					total.Add(int64(n))
				}
			}()
		}
		wg.Wait()
		if total.Load() != 50 {
			t.Fatalf("returned %d bytes with a limit of 50", total.Load())
		}
	})
	t.Run("a read that hits the end of the input under the limit reports EOF", func(t *testing.T) {
		s := NewSealedReaderAt(bytes.NewReader(seq(3)), 100)
		p := make([]byte, 10)
		n, err := s.ReadAt(p, 0)
		if n != 3 || !errors.Is(err, io.EOF) {
			t.Fatalf("= %d, %v", n, err)
		}
	})
	t.Run("sealed refuses and Seal is idempotent", func(t *testing.T) {
		cr := &countingReader{r: bytes.NewReader(seq(10))}
		s := NewSealedReaderAt(cr, 0)
		s.Seal()
		s.Seal()
		n, err := s.ReadAt(make([]byte, 4), 0)
		if n != 0 || !errors.Is(err, ErrSealed) {
			t.Fatalf("after Seal = %d, %v", n, err)
		}
		if cr.calls.Load() != 0 {
			t.Fatal("a sealed reader reached the underlying reader")
		}
	})
	t.Run("seal during a read from another goroutine", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		s := NewSealedReaderAt(readerFunc(func(p []byte, _ int64) (int, error) {
			close(entered)
			<-release
			return copy(p, "abcd"), nil
		}), 0)
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			n, err := s.ReadAt(make([]byte, 4), 0)
			done <- result{n, err}
		}()
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("read never started")
		}
		s.Seal()
		close(release)
		select {
		case r := <-done:
			if r.n != 0 || !errors.Is(r.err, ErrSealed) {
				t.Fatalf("read in flight when sealed = %d, %v; want 0, ErrSealed", r.n, r.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("read never returned")
		}
	})
	t.Run("negative offset and nil reader are errors", func(t *testing.T) {
		s := NewSealedReaderAt(bytes.NewReader(seq(4)), 0)
		if n, err := s.ReadAt(make([]byte, 1), -1); n != 0 || err == nil {
			t.Fatalf("negative offset = %d, %v", n, err)
		}
		if n, err := NewSealedReaderAt(nil, 0).ReadAt(make([]byte, 1), 0); n != 0 || err == nil {
			t.Fatalf("nil reader = %d, %v", n, err)
		}
	})
}

func TestSealedReaderAtExposesOnlyReading(t *testing.T) {
	var x any = NewSealedReaderAt(bytes.NewReader(nil), 0)
	if _, ok := x.(io.ReaderAt); !ok {
		t.Fatal("not an io.ReaderAt")
	}
	if _, ok := x.(io.Writer); ok {
		t.Error("implements io.Writer")
	}
	if _, ok := x.(io.WriterAt); ok {
		t.Error("implements io.WriterAt")
	}
	if _, ok := x.(io.Seeker); ok {
		t.Error("implements io.Seeker")
	}
	if _, ok := x.(io.Closer); ok {
		t.Error("implements io.Closer")
	}
	if _, ok := x.(interface{ Fd() uintptr }); ok {
		t.Error("implements Fd")
	}
	typ := reflect.TypeOf(x)
	var names []string
	for i := range typ.NumMethod() {
		names = append(names, typ.Method(i).Name)
	}
	sort.Strings(names)
	if want := []string{"ReadAt", "Reader", "Seal"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("method set = %v, want %v", names, want)
	}
}

func TestSharedReadBudgetCountsAcrossReaders(t *testing.T) {
	data := bytes.NewReader(make([]byte, 100))
	b := NewReadBudget(40)
	r1, r2 := NewSealedReaderAtShared(data, b), NewSealedReaderAtShared(data, b)
	buf := make([]byte, 30)
	if n, err := r1.ReadAt(buf, 0); n != 30 || err != nil {
		t.Fatalf("first read: %d %v", n, err)
	}
	if n, err := r2.ReadAt(buf, 0); n != 10 || !errors.Is(err, ErrProbeLimit) {
		t.Errorf("the second reader may read only what is left of the shared budget: n=%d err=%v, want 10 and ErrProbeLimit", n, err)
	}
	if n, err := r1.ReadAt(buf[:1], 0); n != 0 || !errors.Is(err, ErrProbeLimit) {
		t.Errorf("the budget is used up for every reader: n=%d err=%v", n, err)
	}
	if n, err := NewSealedReaderAtShared(data, nil).ReadAt(buf, 0); n != 30 || err != nil {
		t.Errorf("a nil budget is unlimited: n=%d err=%v", n, err)
	}
	if n, err := NewSealedReaderAtShared(data, NewReadBudget(0)).ReadAt(buf, 0); n != 30 || err != nil {
		t.Errorf("a zero budget is unlimited: n=%d err=%v", n, err)
	}
}
