package sqlitefile

import (
	"context"
	"fmt"
	"slices"
	"sort"
)

// The as-of ("snapshot") sources of page images. A cell of an old page image must
// follow its overflow chain in the state of the writer at the time of the image,
// never in the live state, so every image has its own page source:
//
//   - a WAL frame F of generation g at slot s: page p is (1) the latest frame of
//     generation g at a slot not above s; else (2) only when g is the current
//     anchored generation and p has no frame of g at all, the database-file page
//     (a page the log never rewrote is as old as the generation start; a page with
//     a later frame may already have been checkpointed into the file, so it is not
//     trusted); else (3) unavailable. Slot order is time order inside one
//     generation only. A stale, detached, broken or unanchored generation never
//     takes (2): the database state of that time is unknown, and a page of a newer
//     generation or of the current database is never followed.
//   - the database image under the WAL: database-file pages only, and only those
//     no committed frame overlays.
//   - the rolled-back image: the database file as found, the WAL ignored.
//   - a journal before-image: the rollback state (the database plus the journal's
//     playable records, the last write wins) as of the journal, the WAL ignored.
//   - a freelist, orphan or beyond-end image: the live state, restricted to pages
//     that are freelist leaves or orphans there (and, for a beyond-end image, the
//     database pages past the live count).
//   - a live page: the live state.
//
// A page a source cannot supply is ErrPageUnavailable and a snapshot-unavailable
// warning; the chain that asked ends there.

// snapshotSource returns the page source of the image's as-of state.
func (p PageImage) snapshotSource() pageSource { return &snapSource{img: p} }

type snapSource struct{ img PageImage }

func (s *snapSource) has(pgno uint32) bool { return pgno != 0 }

func (s *snapSource) read(pgno uint32) ([]byte, PageLoc, error) {
	data, loc, why, err := s.resolve(pgno)
	if err == nil {
		return data, loc, nil
	}
	if isUnavailable(err) {
		h := s.img.h
		file := FileDB
		if s.img.WAL != nil {
			file = FileWAL
		} else if s.img.Journal != nil && s.img.Origin == OriginJournalBefore {
			file = FileJournal
		}
		h.warns.add(Warning{Code: WarnSnapshotUnavailable, File: file, Page: pgno, Msg: fmt.Sprintf(
			"page %d is not available in the state of the writer at the time of the %s image of page %d at offset %d: %s", pgno, s.img.Origin, s.img.Number, s.img.Loc.Offset, why)})
	}
	return nil, PageLoc{}, err
}

func unavail(pgno uint32, format string, a ...any) (data []byte, loc PageLoc, why string, err error) {
	why = fmt.Sprintf(format, a...)
	return nil, PageLoc{}, why, fmt.Errorf("%w: page %d: %s", ErrPageUnavailable, pgno, why)
}

func (s *snapSource) resolve(pgno uint32) ([]byte, PageLoc, string, error) {
	h := s.img.h
	switch s.img.Origin {
	case OriginWALSuperseded, OriginWALUncommitted, OriginWALStale, OriginWALUnverified:
		return s.resolveWAL(pgno)
	case OriginDBUnderWAL:
		if s.overlaid(pgno) {
			return unavail(pgno, "a committed WAL frame overlays it, so the database image of that time is not the image under the log")
		}
		return s.raw(pgno)
	case OriginDBRolledBack:
		return s.raw(pgno)
	case OriginJournalBefore:
		return s.resolveJournal(pgno)
	case OriginLiveBTree:
		data, loc, err := h.live.cache.read(pgno)
		return data, loc, "", err
	default: // freelist leaf and trunk, orphan, beyond end
		return s.resolveLive(pgno)
	}
}

// raw reads page pgno of the database file as found.
func (s *snapSource) raw(pgno uint32) ([]byte, PageLoc, string, error) {
	h := s.img.h
	data, err := h.d.readRawPage(pgno)
	if err != nil {
		if isUnavailable(err) {
			return unavail(pgno, "the database file does not hold it")
		}
		return nil, PageLoc{}, "", err
	}
	return data, PageLoc{File: FileDB, Offset: PageOffset(h.d.info.PageSize, pgno)}, "", nil
}

// overlaid reports whether a committed frame within the commit's size overlays
// page pgno.
func (s *snapSource) overlaid(pgno uint32) bool {
	h := s.img.h
	if h.a == nil || !h.a.scan.Info.UsedByLive || h.a.scan.Info.LastCommit == 0 {
		return false
	}
	_, ok := h.a.scan.latest[pgno]
	return ok && pgno <= h.a.scan.Info.DBPagesAfterCommit
}

// walIndex returns the slots of the frames of each generation by page,
// ascending, built once and charged to the budget until Release.
func (h *Hist) walIndex() (map[genPage][]uint32, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.idx != nil {
		return h.idx, nil
	}
	frames := h.a.scan.Frames
	cost := int64(len(frames)) * walFrameCost
	if err := h.d.env.budget.Alloc(cost); err != nil {
		return nil, fmt.Errorf("index of the WAL frames: %w", err)
	}
	h.charge += cost
	idx := make(map[genPage][]uint32)
	for i := range frames {
		k := genPage{frames[i].Generation, frames[i].Page}
		idx[k] = append(idx[k], frames[i].Slot) // slot order: ascending
	}
	h.idx = idx
	return idx, nil
}

func (s *snapSource) resolveWAL(pgno uint32) ([]byte, PageLoc, string, error) {
	h := s.img.h
	if h.a == nil || s.img.WAL == nil {
		return unavail(pgno, "no WAL")
	}
	idx, err := h.walIndex()
	if err != nil {
		return nil, PageLoc{}, "", err
	}
	gen, slot := s.img.WAL.Generation, s.img.WAL.Frame
	slots := idx[genPage{gen, pgno}]
	if i := sort.Search(len(slots), func(i int) bool { return slots[i] > slot }) - 1; i >= 0 {
		loc := PageLoc{File: FileWAL, Offset: h.a.scan.Frames[slots[i]-1].Offset + walFrameHeader, Frame: slots[i]}
		data, err := h.readLoc(loc)
		if err != nil {
			if isUnavailable(err) {
				return unavail(pgno, "its frame at slot %d does not lie wholly inside the WAL", slots[i])
			}
			return nil, PageLoc{}, "", err
		}
		return data, loc, "", nil
	}
	if len(slots) == 0 && gen < len(h.a.scan.Info.Generations) && h.a.scan.Info.Generations[gen].Anchored {
		return s.raw(pgno)
	}
	if len(slots) > 0 {
		return unavail(pgno, "generation %d first writes it at slot %d, after slot %d, and the database copy may already be checkpointed", gen, slots[0], slot)
	}
	return unavail(pgno, "generation %d never wrote it and is not the current anchored generation", gen)
}

// journalState is the rollback state the journal describes: which record
// supplies each page, and the size the database had. It is the scan's own when
// the journal is applied; for one that is not (no hot journal), it is computed
// the same way: records in order up to the first bad checksum, page 0 or the
// lock-byte page, the last write to
// a page wins, pages above the initial size skipped.
func (h *Hist) journalState() (win map[uint32]int, initial uint32, limited bool, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.jready {
		return h.jwin, h.jinitial, h.jlimited, nil
	}
	in := &h.jr.scan.Info
	win = h.jr.scan.winner
	initial, limited = in.InitialPages, in.HeaderValid
	if !in.Applied {
		win = map[uint32]int{}
		if in.HeaderValid && in.PageSizeMatchesDB {
			lock := LockBytePage(int(in.PageSize))
			cost := int64(len(h.jr.scan.Records)) * journalMapCost
			if err := h.d.env.budget.Alloc(cost); err != nil {
				return nil, 0, false, fmt.Errorf("rollback state of the journal: %w", err)
			}
			h.charge += cost
			for i, r := range h.jr.scan.Records {
				if !r.ChecksumOK {
					break
				}
				if r.Page == 0 || r.Page == lock {
					break // playback ends here, as the scan's does
				}
				if r.Page <= initial {
					win[r.Page] = i
				}
			}
		}
	}
	h.jwin, h.jinitial, h.jlimited, h.jready = win, initial, limited, true
	return win, initial, limited, nil
}

func (s *snapSource) resolveJournal(pgno uint32) ([]byte, PageLoc, string, error) {
	h := s.img.h
	if h.jr == nil {
		return unavail(pgno, "no journal")
	}
	win, initial, limited, err := h.journalState()
	if err != nil {
		return nil, PageLoc{}, "", err
	}
	if limited && pgno > initial {
		return unavail(pgno, "it lies above the %d pages the database had when the journal was written", initial)
	}
	if i, ok := win[pgno]; ok {
		r := h.jr.scan.Records[i]
		loc := PageLoc{File: FileJournal, Offset: r.Offset, Record: r.Index}
		data, err := h.readLoc(loc)
		if err != nil {
			if isUnavailable(err) {
				return unavail(pgno, "its journal record %d does not lie wholly inside the journal", r.Index)
			}
			return nil, PageLoc{}, "", err
		}
		return data, loc, "", nil
	}
	return s.raw(pgno)
}

// resolveLive reads page pgno of the live state for a freelist, orphan or
// beyond-end image: only free pages and orphans (and, past the live count, pages
// of the database file) are followed.
func (s *snapSource) resolveLive(pgno uint32) ([]byte, PageLoc, string, error) {
	h := s.img.h
	lay, err := h.live.Layout(context.Background())
	if err != nil {
		return nil, PageLoc{}, "", err
	}
	if pgno <= lay.Addressable {
		if c := lay.Class[pgno]; !slices.Contains([]PageClass{ClassFreelistLeaf, ClassOrphan}, c) {
			return unavail(pgno, "in the live state it is a %s, not a free page or an orphan", c)
		}
		data, loc, err := h.live.cache.read(pgno)
		return data, loc, "", err
	}
	if s.img.Origin != OriginBeyondEnd || pgno <= h.live.info.PageCount {
		return unavail(pgno, "it is not a page of the live state")
	}
	return s.raw(pgno)
}
