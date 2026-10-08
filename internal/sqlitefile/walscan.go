package sqlitefile

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	walHeaderSize  = 32
	walFrameHeader = 24
	walMagic       = 0x377f0682
	walVersion     = 3007000
	walChunkTarget = 1 << 20 // bytes read per chunk (at least one slot)
	walFrameCost   = 160     // per scanned slot: its Frames entry, its share of a generation and slice growth slack
	walLatestCost  = 64      // one page-to-frame map entry
	walMinPageSize = 512
	walMaxPageSize = 65536
)

func validWALPageSize(p uint32) bool {
	return p >= walMinPageSize && p <= walMaxPageSize && p&(p-1) == 0
}

// ScanWAL classifies every complete slot of a WAL of the given size.
// dbPageSize is the database's page size, or 0 when its header is invalid or
// absent. The slot size is 24 plus the WAL header's page size when that is a
// power of two in 512..65536 (even when the header's magic or checksum fail),
// else dbPageSize, else no slots are defined. The file is read once, in
// disjoint chunks of whole slots (a torn tail is not read); what is kept is
// charged to the budget and grows only with slots actually read.
func ScanWAL(wal io.ReaderAt, size int64, dbPageSize int, opts Options) (*WALScan, error) {
	return scanWALWith(wal, size, dbPageSize, opts, nil)
}

// scanWALWith is ScanWAL with the panic-injection hook of the tests.
func scanWALWith(wal io.ReaderAt, size int64, dbPageSize int, opts Options, hook func(site string)) (res *WALScan, err error) {
	defer guard(&err)
	e := newEnv(opts, hook)
	e.at("walscan")
	l := e.newLedger()
	defer l.guard(&err)
	defer func() {
		if err != nil {
			res = nil
		}
	}()
	if size < 0 {
		return nil, fmt.Errorf("sqlitefile: negative WAL size %d", size)
	}
	w := newWarnings(e.opts.Limits.MaxWarnings)
	e.reportClamps(w)
	s := &WALScan{}
	in := &s.Info
	in.Size = size
	in.Present = size > 0
	finish := func() (*WALScan, error) {
		s.Warnings = w.snapshot()
		return s, nil
	}
	if size == 0 {
		return finish()
	}
	if size < walHeaderSize {
		in.HeaderProblem = fmt.Sprintf("file of %d bytes is shorter than the %d-byte header", size, walHeaderSize)
		in.TrailingBytes = size
		w.add(Warning{Code: WarnWALHeaderInvalid, File: FileWAL, Msg: in.HeaderProblem})
		return finish()
	}

	var hdr [walHeaderSize]byte
	if err := readFull(wal, hdr[:], 0); err != nil {
		return nil, err
	}
	magic := binary.BigEndian.Uint32(hdr[0:])
	version := binary.BigEndian.Uint32(hdr[4:])
	hdrPage := binary.BigEndian.Uint32(hdr[8:])
	in.CheckpointSeq = binary.BigEndian.Uint32(hdr[12:])
	in.Salt1 = binary.BigEndian.Uint32(hdr[16:])
	in.Salt2 = binary.BigEndian.Uint32(hdr[20:])
	stored1 := binary.BigEndian.Uint32(hdr[24:])
	stored2 := binary.BigEndian.Uint32(hdr[28:])
	big := magic&1 == 1
	in.BigEndianChecksums = big

	switch {
	case magic&^1 != walMagic:
		in.HeaderProblem = fmt.Sprintf("magic %#x is not a WAL magic", magic)
	case version != walVersion:
		in.HeaderProblem = fmt.Sprintf("version %d is not %d", version, walVersion)
	case !validWALPageSize(hdrPage):
		in.HeaderProblem = fmt.Sprintf("page size %d is not a power of two in %d..%d", hdrPage, walMinPageSize, walMaxPageSize)
	default:
		if c1, c2 := WALChecksum(hdr[:24], big, 0, 0); c1 != stored1 || c2 != stored2 {
			in.HeaderProblem = "header checksum does not match"
		} else {
			in.HeaderValid = true
		}
	}

	if magic&^1 == walMagic && validWALPageSize(hdrPage) && version != walVersion {
		// The engine refuses to open a database whose log has a sound header of
		// another version (TestEngineWALHeaderResealed).
		if c1, c2 := WALChecksum(hdr[:24], big, 0, 0); c1 == stored1 && c2 == stored2 {
			in.VersionRefused = true
		}
	}

	var pageSize uint32
	switch {
	case validWALPageSize(hdrPage):
		pageSize = hdrPage
	case dbPageSize > 0 && dbPageSize <= walMaxPageSize && validWALPageSize(uint32(dbPageSize)):
		pageSize = uint32(dbPageSize)
	}
	in.PageSize = pageSize
	if !in.HeaderValid {
		w.add(Warning{Code: WarnWALHeaderInvalid, File: FileWAL, Msg: in.HeaderProblem})
	}
	if validWALPageSize(hdrPage) && dbPageSize > 0 && int64(hdrPage) != int64(dbPageSize) {
		w.add(Warning{
			Code: WarnWALPageSizeMismatch, File: FileWAL, Offset: 8,
			Msg: fmt.Sprintf("WAL page size %d differs from the database's %d; slots are cut by the WAL's", hdrPage, dbPageSize),
		})
	}

	avail := size - walHeaderSize
	if pageSize == 0 {
		in.TrailingBytes = avail
		return finish()
	}
	slot := int64(walFrameHeader) + int64(pageSize)
	n := avail / slot
	in.TrailingBytes = avail % slot
	if in.TrailingBytes > 0 {
		w.add(Warning{
			Code: WarnWALTornTail, File: FileWAL, Offset: walHeaderSize + n*slot,
			Msg: fmt.Sprintf("%d bytes after the last complete frame", in.TrailingBytes),
		})
	}
	in.FrameSlots = uint32(min(n, math.MaxUint32))
	limit := min(e.opts.Limits.MaxWALFrames, math.MaxUint32)
	scan := min(n, limit)
	if n > limit {
		w.add(Warning{
			Code: WarnLimitReached, File: FileWAL,
			Msg: fmt.Sprintf("%d frame slots in the file; only the first %d are scanned", n, limit),
		})
	}
	if scan == 0 {
		return finish()
	}

	perChunk := min(max(1, walChunkTarget/slot), scan)
	bufSize := perChunk * slot
	if err := l.alloc(bufSize); err != nil {
		return nil, fmt.Errorf("WAL read buffer: %w", err)
	}
	buf := make([]byte, bufSize)
	defer l.free(bufSize) // transient: the result keeps only what it holds

	anchored := in.HeaderValid
	chain := anchored // the valid chain is still growing
	prev1, prev2 := stored1, stored2
	var prevSalt1, prevSalt2 uint32
	var lastCommit uint32
	for first := int64(0); first < scan; first += perChunk {
		cnt := min(perChunk, scan-first)
		if err := l.alloc(cnt * walFrameCost); err != nil {
			return nil, fmt.Errorf("WAL frames: %w", err)
		}
		chunk := buf[:cnt*slot]
		if err := readFull(wal, chunk, walHeaderSize+first*slot); err != nil {
			return nil, err
		}
		for i := range cnt {
			idx := first + i
			raw := chunk[i*slot : (i+1)*slot]
			f := WALFrame{
				Slot:   uint32(idx + 1),
				Page:   binary.BigEndian.Uint32(raw[0:]),
				DBSize: binary.BigEndian.Uint32(raw[4:]),
				Salt1:  binary.BigEndian.Uint32(raw[8:]),
				Salt2:  binary.BigEndian.Uint32(raw[12:]),
				Check1: binary.BigEndian.Uint32(raw[16:]),
				Check2: binary.BigEndian.Uint32(raw[20:]),
				Offset: walHeaderSize + idx*slot,
			}
			c1, c2 := WALChecksum(raw[:8], big, prev1, prev2)
			c1, c2 = WALChecksum(raw[walFrameHeader:], big, c1, c2)
			f.Linked = c1 == f.Check1 && c2 == f.Check2
			saltMatch := f.Salt1 == in.Salt1 && f.Salt2 == in.Salt2

			switch {
			case !anchored:
				f.State = FrameUnanchored
			case chain && !saltMatch:
				f.State = FrameStale
				chain = false
			case chain && (f.Page == 0 || !f.Linked):
				f.State = FrameBroken
				chain = false
			case chain:
				f.State = FrameUncommitted // settled below, once the last commit is known
				in.FramesValid++
				if f.DBSize != 0 {
					lastCommit = f.Slot
					in.Commits++
					in.DBPagesAfterCommit = f.DBSize
				}
			case saltMatch:
				f.State = FrameDetached
			default:
				f.State = FrameStale
			}
			switch f.State {
			case FrameBroken:
				in.FramesBroken++
			case FrameDetached:
				in.FramesDetached++
			case FrameStale:
				in.FramesStale++
			}

			if len(in.Generations) == 0 || f.Salt1 != prevSalt1 || f.Salt2 != prevSalt2 || !f.Linked {
				in.Generations = append(in.Generations, WALGeneration{Salt1: f.Salt1, Salt2: f.Salt2, FirstSlot: f.Slot})
			}
			g := &in.Generations[len(in.Generations)-1]
			g.Slots++
			if f.DBSize != 0 {
				g.Commits++
			}
			f.Generation = len(in.Generations) - 1
			prevSalt1, prevSalt2 = f.Salt1, f.Salt2
			prev1, prev2 = f.Check1, f.Check2
			s.Frames = append(s.Frames, f)
		}
	}

	in.LastCommit = lastCommit
	in.FramesCommitted = lastCommit
	in.FramesUncommitted = in.FramesValid - lastCommit
	if lastCommit > 0 {
		if err := l.alloc(int64(lastCommit) * walLatestCost); err != nil {
			return nil, fmt.Errorf("WAL page map: %w", err)
		}
		s.latest = make(map[uint32]uint32)
	}
	for i := range s.Frames {
		f := &s.Frames[i]
		if f.State != FrameUncommitted || f.Slot > lastCommit {
			continue
		}
		f.State = FrameCommitted
		in.MaxPageNumber = max(in.MaxPageNumber, f.Page)
		s.latest[f.Page] = f.Slot
	}
	s.warnNotApplied(w)
	if in.HeaderValid {
		// The generation holding the valid chain is the one that starts at slot
		// 1 when that slot verified; every age is the salt-1 distance from it.
		if in.FramesValid > 0 {
			in.Generations[0].Anchored = true
		}
		for i := range in.Generations {
			in.Generations[i].Age = in.Salt1 - in.Generations[i].Salt1
		}
	}
	return finish()
}

// warnNotApplied adds the one wal-frames-not-applied warning of a scan: frames
// the engine does not apply (broken, detached, stale, uncommitted) may still
// hold evidence, so they are counted by class with the first slot of each.
func (s *WALScan) warnNotApplied(w *warnings) {
	classes := []FrameState{FrameBroken, FrameDetached, FrameStale, FrameUncommitted}
	var count, first [4]uint32
	for i := range s.Frames {
		f := &s.Frames[i]
		for c, st := range classes {
			if f.State == st {
				if count[c] == 0 {
					first[c] = f.Slot
				}
				count[c]++
			}
		}
	}
	if count == [4]uint32{} {
		return
	}
	msg := "frames the engine does not apply:"
	for c, st := range classes {
		sep := ","
		if c == 0 {
			sep = ""
		}
		if count[c] == 0 {
			msg += fmt.Sprintf("%s %s 0", sep, st)
		} else {
			msg += fmt.Sprintf("%s %s %d (first slot %d)", sep, st, count[c], first[c])
		}
	}
	w.add(Warning{Code: WarnWALFramesNotApplied, File: FileWAL, Msg: msg})
}
