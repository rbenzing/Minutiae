package sqlitefile

import (
	"fmt"
	"io"
)

// walOverlay is a pageSource that serves, for each page the WAL's committed
// frames hold, the image of the newest such frame, and every other page from
// the lower source. Only frames up to the last commit frame of the current
// generation are in it (scan.latest), so uncommitted, broken, detached and
// stale frames never reach a view. Pages above the size the last commit frame
// declares are unavailable, as for the engine.
type walOverlay struct {
	lower    pageSource
	wal      io.ReaderAt
	walSize  int64
	scan     *WALScan
	dbPageSz int
	limit    uint32 // database size in pages after the last commit
}

func (o *walOverlay) has(pgno uint32) bool {
	if pgno == 0 || pgno > o.limit {
		return false
	}
	if slot, ok := o.scan.latest[pgno]; ok {
		return o.frameReadable(slot)
	}
	return o.lower.has(pgno)
}

// frameOffset is where the page image of slot lies in the WAL file.
func (o *walOverlay) frameOffset(slot uint32) int64 {
	return o.scan.Frames[slot-1].Offset + walFrameHeader
}

// frameReadable reports whether a whole database page of bytes lies inside the
// WAL file at the frame: the engine reads the database's page size from there
// whatever the WAL's own page size is.
func (o *walOverlay) frameReadable(slot uint32) bool {
	return o.frameOffset(slot)+int64(o.dbPageSz) <= o.walSize
}

func (o *walOverlay) read(pgno uint32) ([]byte, PageLoc, error) {
	if pgno == 0 || pgno > o.limit {
		return nil, PageLoc{}, fmt.Errorf("%w: page %d is beyond the %d pages the last commit left", ErrPageUnavailable, pgno, o.limit)
	}
	slot, ok := o.scan.latest[pgno]
	if !ok {
		return o.lower.read(pgno)
	}
	if !o.frameReadable(slot) {
		return nil, PageLoc{}, fmt.Errorf("%w: frame %d for page %d does not hold a whole page inside the WAL", ErrPageUnavailable, slot, pgno)
	}
	off := o.frameOffset(slot)
	p := make([]byte, o.dbPageSz)
	if err := readFull(o.wal, p, off); err != nil {
		return nil, PageLoc{}, err
	}
	return p, PageLoc{File: FileWAL, Offset: off, Frame: slot}, nil
}

// attachedWAL is a WAL attached to a DB.
type attachedWAL struct {
	r    io.ReaderAt
	size int64
	scan *WALScan
}

// AttachWAL scans the write-ahead log wal (size bytes) and attaches it: a view
// taken by Live afterwards presents the database with its committed frames
// applied. A second call is ErrAlreadyAttached. The WAL is used when its
// header is valid, whatever its page size: the engine applies such a log too,
// reading the first database-page-size bytes of each frame (the
// wal-page-size-mismatch warning says so). Views taken before the call do not
// change.
func (d *DB) AttachWAL(wal io.ReaderAt, size int64) (info *WALInfo, err error) {
	defer guard(&err)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.wal != nil {
		return nil, ErrAlreadyAttached
	}
	scan, err := ScanWAL(wal, size, d.info.PageSize, d.env.opts)
	if err != nil {
		return nil, err
	}
	scan.Info.UsedByLive = scan.Info.HeaderValid
	for _, x := range scan.Warnings {
		d.warns.add(x)
	}
	d.wal = &attachedWAL{r: wal, size: size, scan: scan}
	cp := scan.Info
	cp.Generations = append([]WALGeneration(nil), scan.Info.Generations...)
	return &cp, nil
}

// WAL returns the scan of the attached write-ahead log, or nil when none is
// attached. The result is shared and must not be modified.
func (d *DB) WAL() *WALScan {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.wal == nil {
		return nil
	}
	return d.wal.scan
}

// applyWAL layers the attached WAL over v's sources: v.src, v.info and the
// warnings follow the state the last commit frame left. d.wal is a snapshot
// taken under the lock.
func (v *View) applyWAL(a *attachedWAL) {
	s := a.scan
	if !s.Info.UsedByLive || s.Info.LastCommit == 0 {
		return
	}
	d := v.d
	limit := s.Info.DBPagesAfterCommit
	ov := &walOverlay{lower: v.src, wal: a.r, walSize: a.size, scan: s, dbPageSz: d.info.PageSize, limit: limit}
	v.src = ov
	// Pages that exist in some source: the database's own, or named by a
	// committed frame. The commit frame's size claim is an upper bound only.
	phys := max(v.info.FilePages, s.Info.MaxPageNumber)
	info := v.info
	if _, ok := s.latest[1]; ok {
		if hdr, _, err := ov.read(1); err == nil {
			sz := min(int64(phys)*int64(d.info.PageSize), 1<<62)
			scratch := newWarnings(v.e.opts.Limits.MaxWarnings)
			if pi, perr := parseHeader(hdr[:headerSize], sz, scratch); perr != nil || pi.PageSize != d.info.PageSize || pi.Reserved != d.info.Reserved {
				v.warns.add(Warning{Code: WarnWALPage1Mismatch, File: FileWAL, Page: 1, Msg: "page 1 in the WAL has a header that does not fit the database (page size, reserved bytes or structure); the database's header is kept"})
			} else {
				for _, x := range scratch.snapshot() {
					x.File = FileWAL
					v.warns.add(x)
				}
				info = pi
			}
		}
	}
	info.FileSize = d.size
	info.FilePages = phys
	info.PageCount = limit
	v.info = info
	if info.WriteVersion != 2 || info.ReadVersion != 2 {
		v.warns.add(Warning{
			Code: WarnWALModeMismatch, File: FileWAL,
			Msg: fmt.Sprintf("a WAL is attached but the header says write version %d, read version %d (2 is WAL mode); applied like the engine does", info.WriteVersion, info.ReadVersion),
		})
	}
}
