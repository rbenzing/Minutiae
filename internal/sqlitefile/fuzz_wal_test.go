package sqlitefile_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// FuzzWAL: arbitrary WAL bytes and a database page size never panic, and the
// result obeys the structural rules, re-checked here by an independent walk.
func FuzzWAL(f *testing.F) {
	for _, big := range []bool{false, true} {
		f.Add(buildWAL(512, big, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 5}}).Bytes(), uint32(512))
		f.Add(buildWAL(512, big, 1, 2, []walSpec{{2, 0}, {3, 0}}).Bytes(), uint32(0))
	}
	f.Add(resetScenario().Bytes(), uint32(512))
	f.Add(buildWAL(4096, false, 9, 9, []walSpec{{2, 3}}).Bytes(), uint32(8192))
	torn := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}}).Bytes()
	f.Add(torn[:len(torn)-77], uint32(512))
	flipped := buildWAL(512, false, 1, 2, []walSpec{{2, 0}, {3, 4}, {4, 5}}).Bytes()
	flipped[32+536+40] ^= 8
	f.Add(flipped, uint32(512))
	f.Add([]byte{0x37, 0x7f, 0x06, 0x82}, uint32(512))
	f.Add(buildWAL(65536, false, 1, 2, []walSpec{{2, 0}, {3, 4}}).Bytes(), uint32(65536))
	f.Fuzz(func(t *testing.T, data []byte, dbPS uint32) {
		cb := &capBudget{limit: 256 << 20}
		s, err := sqlitefile.ScanWAL(bytes.NewReader(data), int64(len(data)), int(dbPS), sqlitefile.Options{Budget: cb})
		if err != nil {
			t.Fatalf("ScanWAL: %v", err)
		}
		in := s.Info
		if int64(len(s.Frames))*(24+512) > int64(len(data)) {
			t.Fatalf("%d frames from %d bytes", len(s.Frames), len(data))
		}
		if cb.peak > 1<<20+400*int64(len(s.Frames)) {
			t.Fatalf("%d bytes charged for %d frames", cb.peak, len(s.Frames))
		}
		if uint32(len(s.Frames)) != in.FrameSlots || in.FramesValid > in.FrameSlots || in.LastCommit > in.FramesValid || in.FramesCommitted != in.LastCommit {
			t.Fatalf("counts %+v (%d frames)", in, len(s.Frames))
		}
		gen := 0
		for i, fr := range s.Frames {
			if fr.Slot != uint32(i+1) {
				t.Fatalf("slot %d at index %d", fr.Slot, i)
			}
			if fr.Generation < 0 || fr.Generation >= len(in.Generations) {
				t.Fatalf("generation %d of %d", fr.Generation, len(in.Generations))
			}
			gen = max(gen, fr.Generation)
		}
		var slots uint32
		for _, g := range in.Generations {
			slots += g.Slots
		}
		if slots != uint32(len(s.Frames)) || (len(s.Frames) > 0 && gen != len(in.Generations)-1) {
			t.Fatalf("generations %+v for %d frames", in.Generations, len(s.Frames))
		}
		if !in.HeaderValid {
			for _, fr := range s.Frames {
				if fr.State != sqlitefile.FrameUnanchored {
					t.Fatalf("slot %d is %s under an invalid header", fr.Slot, fr.State)
				}
			}
			return
		}
		// Independent re-verification of the chain.
		big := binary.BigEndian.Uint32(data) == 0x377f0683
		slot := 24 + int(in.PageSize)
		c1, c2 := binary.BigEndian.Uint32(data[24:]), binary.BigEndian.Uint32(data[28:])
		if h1, h2 := naiveWALSum(data[:24], big, 0, 0); h1 != c1 || h2 != c2 {
			t.Fatalf("a valid header whose checksum does not verify")
		}
		valid, lastCommit := 0, 0
		for i := range s.Frames {
			raw := data[32+i*slot : 32+(i+1)*slot]
			a, b := naiveWALSum(raw[:8], big, c1, c2)
			a, b = naiveWALSum(raw[24:], big, a, b)
			ok := bytes.Equal(raw[8:16], data[16:24]) && binary.BigEndian.Uint32(raw) != 0 &&
				a == binary.BigEndian.Uint32(raw[16:]) && b == binary.BigEndian.Uint32(raw[20:])
			if !ok {
				break
			}
			c1, c2 = a, b
			valid++
			if binary.BigEndian.Uint32(raw[4:]) != 0 {
				lastCommit = i + 1
			}
		}
		if uint32(valid) != in.FramesValid || uint32(lastCommit) != in.LastCommit {
			t.Fatalf("independent chain: %d valid, last commit %d; scan: %d, %d", valid, lastCommit, in.FramesValid, in.LastCommit)
		}
		for i, fr := range s.Frames {
			if (fr.State == sqlitefile.FrameCommitted) != (i < lastCommit) {
				t.Fatalf("slot %d is %s, last commit %d", fr.Slot, fr.State, lastCommit)
			}
			if i >= valid && (fr.State == sqlitefile.FrameUncommitted || fr.State == sqlitefile.FrameCommitted) {
				t.Fatalf("slot %d is %s beyond the chain", fr.Slot, fr.State)
			}
		}
	})
}
