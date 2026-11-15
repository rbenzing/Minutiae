package filesys_test

import (
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// decodeSlackFuzz reads size, unit, fsSize, then up to 16 runs and 16 alloc runs (16 bytes each, a leading count byte
// for each list) from data.
func decodeSlackFuzz(data []byte) (size, unit, fsSize int64, runs, alloc []filesys.Run) {
	next := func() int64 {
		if len(data) < 8 {
			data = nil
			return 0
		}
		v := int64(binary.LittleEndian.Uint64(data))
		data = data[8:]
		return v
	}
	size, unit, fsSize = next(), next(), next()
	list := func() []filesys.Run {
		if len(data) == 0 {
			return nil
		}
		n := int(data[0]) % 17
		data = data[1:]
		var out []filesys.Run
		for range n {
			o, l := next(), next()
			out = append(out, filesys.Run{Offset: o, Length: l})
		}
		return out
	}
	runs = list()
	alloc = list()
	return
}

func encodeSlackFuzz(size, unit, fsSize int64, runs, alloc []filesys.Run) []byte {
	var b []byte
	put := func(v int64) { b = binary.LittleEndian.AppendUint64(b, uint64(v)) }
	put(size)
	put(unit)
	put(fsSize)
	for _, l := range [][]filesys.Run{runs, alloc} {
		b = append(b, byte(len(l)))
		for _, r := range l {
			put(r.Offset)
			put(r.Length)
		}
	}
	return b
}

func FuzzTailSlack(f *testing.F) {
	r := func(o, l int64) filesys.Run { return filesys.Run{Offset: o, Length: l} }
	f.Add(encodeSlackFuzz(700, 512, 1<<20, []filesys.Run{r(4096, 700)}, []filesys.Run{r(4096, 1024)}))
	f.Add(encodeSlackFuzz(1024, 512, 1<<20, []filesys.Run{r(4096, 1024)}, []filesys.Run{r(4096, 1024)}))
	f.Add(encodeSlackFuzz(700, 512, 1<<20, []filesys.Run{r(-1, 700)}, nil))
	f.Add(encodeSlackFuzz(700, 512, 1<<20, []filesys.Run{r(4096, 512)}, []filesys.Run{r(4096, 512)}))
	f.Add(encodeSlackFuzz(700, 512, 1<<20, []filesys.Run{r(4096, 512), r(-1, 188)}, []filesys.Run{r(4096, 512)}))
	f.Add(encodeSlackFuzz(1500, 512, 1<<20, []filesys.Run{r(-1, 512), r(4096, 988)}, []filesys.Run{r(4096, 1024)}))
	f.Fuzz(func(t *testing.T, data []byte) {
		size, unit, fsSize, runs, alloc := decodeSlackFuzz(data)
		run, out, _ := filesys.TailSlack(size, runs, alloc, unit, fsSize)
		if out != filesys.SlackFound {
			if run != (filesys.Run{}) {
				t.Fatalf("outcome %q came with the run %v", out, run)
			}
			return
		}
		if run.Length <= 0 || run.Length >= unit {
			t.Fatalf("slack run %v is not in 1..%d", run, unit-1)
		}
		var lastRun, lastAlloc filesys.Run
		for _, x := range runs {
			if x.Offset >= 0 && x.Length > 0 {
				lastRun = x
			}
		}
		for _, x := range alloc {
			if x.Offset >= 0 && x.Length > 0 {
				lastAlloc = x
			}
		}
		if run.Offset != lastRun.Offset+lastRun.Length {
			t.Fatalf("slack %v does not start at the end of the last data run %v", run, lastRun)
		}
		if run.Offset+run.Length != lastAlloc.Offset+lastAlloc.Length {
			t.Fatalf("slack %v does not end at the end of the last allocation %v", run, lastAlloc)
		}
	})
}
