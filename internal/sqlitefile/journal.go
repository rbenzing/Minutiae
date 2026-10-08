package sqlitefile

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

const (
	journalHeaderLen    = 28
	journalMinSector    = 32
	journalMaxSector    = 65536
	journalRecordCost   = 128     // per scanned record: 40 bytes of JournalRecord, three times over for append growth (old and new backing arrays)
	journalSegmentCost  = 64      // per segment
	journalMapCost      = 64      // one page-to-record map entry
	journalChunk        = 1 << 20 // bytes read per chunk (at least one record)
	journalMaxSuperName = 1040    // the longest super-journal name the engine honours (measured: 1041 bytes make it ignore the trailer)
	journalMinFile      = 512     // a journal shorter than the engine first sector is never played back
	journalMaxProblems  = 20      // per-record warnings kept per scan
	journalSampleRecs   = 64      // records sampled per candidate sector when the header is zeroed
)

// JournalSegment is one header of the journal and the records it governs.
type JournalSegment struct {
	Offset                   int64
	DeclaredRecords, Records uint32
	HeaderOK                 bool
}

// JournalRecord is one before-image page of the journal.
type JournalRecord struct {
	Index, Segment int
	Page           uint32
	Offset         int64 // of the page data in the journal file
	ChecksumOK     bool
	// Applied is set by the scan: this record's image is the one Live()
	// presents for its page (the last playable record of the page).
	Applied bool
}

// JournalInfo describes a rollback journal.
type JournalInfo struct {
	Present, Hot, HeaderValid, ZeroedHeader bool
	SectorValid, PageSizeMatchesDB          bool
	Size                                    int64
	PageSize, SectorSize, InitialPages      uint32
	Nonce                                   uint32
	NonceDerived                            bool
	HasSuperJournal                         bool
	Segments                                []JournalSegment
	RecordsTotal, RecordsValid              uint32
	RecordsBadChecksum                      uint32 // records whose checksum fails or could not be verified
	FirstBadRecord                          int    // index of the first record that is not ChecksumOK; -1 when none
	AppliedRecords                          uint32
	Applied                                 bool   // Live() presents the rolled-back state
	NotAppliedReason                        string // "not-hot", "zeroed-header", "header-invalid", "page-size-mismatch", "super-journal-unknown" or "limit-reached"
	TrailingBytes                           int64
}

// JournalScan is the result of ScanJournal. Records lists every whole record of
// every segment in file order, bad checksums included.
type JournalScan struct {
	Info     JournalInfo
	Records  []JournalRecord
	Warnings []Warning

	winner     map[uint32]int // page -> index of the record Live() presents
	maxApplied uint32         // highest page among the applied records
	capped     bool
}

func validJournalSector(s uint32) bool {
	return s >= journalMinSector && s <= journalMaxSector && s&(s-1) == 0
}

func validJournalPageSize(p uint32) bool {
	return p >= 512 && p <= 65536 && p&(p-1) == 0
}

// JournalChecksum is the checksum of a journal record: the nonce plus the bytes
// of the page at pagesize-200, pagesize-400, ... while the index is above zero.
func JournalChecksum(nonce uint32, page []byte) uint32 {
	c := nonce
	for i := len(page) - 200; i > 0; i -= 200 {
		c += uint32(page[i])
	}
	return c
}

// jscan is the state of one ScanJournal call.
type jscan struct {
	e      *env
	l      *ledger
	w      *warnings
	j      io.ReaderAt
	size   int64
	dbps   int
	opts   Options
	s      *JournalScan
	probs  int // record-level warnings raised
	hushed int // record-level warnings not raised
}

// ScanJournal scans a rollback journal of size bytes. dbPageSize is the page
// size of the database it belongs to (0 when unknown). A journal that is
// absent (j nil) or empty is reported, never an error; an I/O error is
// returned as it is. The scan reads the file once, in chunks of whole records,
// and what it keeps is charged to the budget and grows only with the records
// actually read. The verdict (Info.Applied, Record.Applied) follows the rules
// the engine probe recorded (TestEngineJournalRules).
func ScanJournal(j io.ReaderAt, size int64, dbPageSize int, opts Options) (*JournalScan, error) {
	return scanJournalWith(j, size, dbPageSize, opts, nil)
}

// scanJournalWith is ScanJournal with the panic-injection hook of the tests.
func scanJournalWith(j io.ReaderAt, size int64, dbPageSize int, opts Options, hook func(site string)) (res *JournalScan, err error) {
	defer guard(&err)
	e := newEnv(opts, hook)
	e.at("journalscan")
	l := e.newLedger()
	defer l.guard(&err)
	defer func() {
		if err != nil {
			res = nil
		}
	}()
	if size < 0 {
		return nil, fmt.Errorf("sqlitefile: negative journal size %d", size)
	}
	js := &jscan{e: e, l: l, w: newWarnings(e.opts.Limits.MaxWarnings), j: j, size: size, dbps: dbPageSize, opts: opts, s: &JournalScan{}}
	e.reportClamps(js.w)
	js.s.Info.FirstBadRecord = -1
	if err := js.scan(); err != nil {
		return nil, err
	}
	if js.hushed > 0 {
		js.w.add(Warning{Code: WarnJournalPageInvalid, File: FileJournal, Msg: fmt.Sprintf("%d further record problems are not listed", js.hushed)})
	}
	js.s.Warnings = js.w.snapshot()
	return js.s, nil
}

func (js *jscan) warn(code string, off int64, format string, a ...any) {
	js.w.add(Warning{Code: code, File: FileJournal, Offset: off, Msg: fmt.Sprintf(format, a...)})
}

// problem raises a record-level warning, at most journalMaxProblems per scan.
func (js *jscan) problem(page uint32, off int64, format string, a ...any) {
	if js.probs >= journalMaxProblems {
		js.hushed++
		return
	}
	js.probs++
	js.w.add(Warning{Code: WarnJournalPageInvalid, File: FileJournal, Page: page, Offset: off, Msg: fmt.Sprintf(format, a...)})
}

func (js *jscan) scan() error {
	in := &js.s.Info
	if js.j == nil {
		in.NotAppliedReason = "not-hot"
		return nil
	}
	in.Present, in.Size = true, js.size
	if js.size == 0 {
		in.NotAppliedReason = "not-hot"
		return nil
	}
	var hdr [journalHeaderLen]byte
	n := min(js.size, journalHeaderLen)
	if err := readFull(js.j, hdr[:n], 0); err != nil {
		return err
	}
	in.Hot = hdr[0] != 0
	if !in.Hot {
		if js.size >= journalHeaderLen && hdr == [journalHeaderLen]byte{} {
			in.ZeroedHeader = true
			in.NotAppliedReason = "zeroed-header"
			return js.recoverZeroed()
		}
		in.NotAppliedReason = "not-hot"
		return nil
	}
	regionEnd, err := js.regionEnd()
	if err != nil {
		return err
	}
	if err := js.headerAndSegments(hdr, n, regionEnd); err != nil {
		return err
	}
	js.decide()
	js.hotWarning()
	return nil
}

// regionEnd is where the records region ends: before the super-journal
// trailer ([lock-byte page marker][name][len][sum][magic]) when there is one.
func (js *jscan) regionEnd() (int64, error) {
	in := &js.s.Info
	if js.size < 16 {
		return js.size, nil
	}
	var t [16]byte
	if err := readFull(js.j, t[:], js.size-16); err != nil {
		return 0, err
	}
	nameLen := int64(binary.BigEndian.Uint32(t[0:]))
	if string(t[8:]) != journalMagic || nameLen == 0 || nameLen > journalMaxSuperName || nameLen+16 > js.size {
		return js.size, nil
	}
	if err := js.l.alloc(nameLen); err != nil {
		return 0, err
	}
	defer js.l.free(nameLen)
	name := make([]byte, nameLen)
	if err := readFull(js.j, name, js.size-16-nameLen); err != nil {
		return 0, err
	}
	var sum uint32
	for _, b := range name {
		sum += uint32(b)
	}
	if sum != binary.BigEndian.Uint32(t[4:]) {
		return js.size, nil
	}
	in.HasSuperJournal = true
	total := 16 + nameLen
	if js.size-total >= 4 {
		total += 4 // the lock-byte page marker in front of the name
	}
	return js.size - total, nil
}

// headerAndSegments validates the first header and walks the segments.
func (js *jscan) headerAndSegments(hdr [journalHeaderLen]byte, n, regionEnd int64) error {
	in := &js.s.Info
	in.NotAppliedReason = "header-invalid"
	if n < journalHeaderLen {
		js.warn(WarnJournalHeaderInvalid, 0, "the file holds %d bytes, fewer than the %d of a header", js.size, journalHeaderLen)
		return nil
	}
	if string(hdr[:8]) != journalMagic {
		js.warn(WarnJournalHeaderInvalid, 0, "the first bytes are not the journal magic")
		return nil
	}
	be := binary.BigEndian
	in.Nonce = be.Uint32(hdr[12:])
	in.InitialPages = be.Uint32(hdr[16:])
	in.SectorSize = be.Uint32(hdr[20:])
	psField := be.Uint32(hdr[24:])
	if !validJournalSector(in.SectorSize) {
		js.warn(WarnJournalSectorInvalid, 20, "sector size %d is not a power of two in %d..%d; the header is not valid", in.SectorSize, journalMinSector, journalMaxSector)
		return nil
	}
	in.SectorValid = true
	switch {
	case psField == 0 && validJournalPageSize(uint32(max(js.dbps, 0))):
		// the engine reads a page size of 0 as the database's (TestEngineJournalRules)
		in.PageSize = uint32(js.dbps)
	case psField == 0:
		js.warn(WarnJournalNoPageSize, 24, "the header page size is 0 (the database's) and the database page size is unknown")
		return nil
	case validJournalPageSize(psField):
		in.PageSize = psField
	default:
		js.warn(WarnJournalHeaderInvalid, 24, "page size %d is not a power of two in 512..65536", psField)
		return nil
	}
	if js.size < journalMinFile {
		js.warn(WarnJournalHeaderInvalid, 0, "the file holds %d bytes, fewer than the %d of the engine first sector; the engine never plays such a journal back", js.size, journalMinFile)
		return nil
	}
	if int64(in.SectorSize) > js.size {
		js.warn(WarnJournalHeaderInvalid, 20, "the header (one sector of %d bytes) does not fit the %d bytes of the file", in.SectorSize, js.size)
		return nil
	}
	in.HeaderValid = true
	in.NotAppliedReason = ""
	in.PageSizeMatchesDB = js.dbps > 0 && int64(in.PageSize) == int64(js.dbps)
	return js.walk(hdr, regionEnd)
}

// walk reads the segments: each header at the next multiple of the sector size
// after the records before it; a later header is trusted for its record count
// and nonce only (the sector and page size of the first govern, as for the
// engine).
func (js *jscan) walk(first [journalHeaderLen]byte, regionEnd int64) error {
	s, in := js.s, &js.s.Info
	e := js.e
	sector := int64(in.SectorSize)
	ps := int64(in.PageSize)
	rsz := ps + 8
	limitRecs := e.opts.Limits.MaxJournalRecords
	limitSegs := e.opts.Limits.MaxJournalSegments
	hdr := first
	off := int64(0)
	consumed := int64(0)
	for off+sector <= js.size {
		if off > 0 {
			if err := readFull(js.j, hdr[:], off); err != nil {
				return err
			}
			if string(hdr[:8]) != journalMagic {
				break
			}
		}
		if len(in.Segments) >= limitSegs {
			js.warn(WarnLimitReached, off, "more than %d segments; the rest are not read, so the journal is not applied", limitSegs)
			s.capped = true
			break
		}
		if err := js.l.alloc(journalSegmentCost); err != nil {
			return fmt.Errorf("journal segments: %w", err)
		}
		nRec := binary.BigEndian.Uint32(hdr[8:])
		nonce := binary.BigEndian.Uint32(hdr[12:])
		recStart := off + sector
		fit := max(regionEnd-recStart, 0) / rsz
		declared := int64(nRec)
		if nRec == math.MaxUint32 {
			declared = min(fit, math.MaxUint32)
		}
		count := min(declared, fit)
		if rem := limitRecs - int64(len(s.Records)); count > rem {
			count = rem
			s.capped = true
		}
		seg := len(in.Segments)
		in.Segments = append(in.Segments, JournalSegment{Offset: off, DeclaredRecords: uint32(declared), Records: uint32(count), HeaderOK: true})
		if err := js.readRecords(recStart, count, nonce, true, seg); err != nil {
			return err
		}
		consumed = recStart + count*rsz
		if s.capped {
			js.warn(WarnLimitReached, off, "more than %d records; the rest are not read, so the journal is not applied", limitRecs)
			break
		}
		if nRec == math.MaxUint32 || count < declared {
			break
		}
		off = (consumed + sector - 1) / sector * sector
	}
	in.TrailingBytes = max(regionEnd-consumed, 0)
	return nil
}

// readRecords appends count whole records starting at start, in chunks. With
// trust the checksums are verified against nonce; without it every record is
// unverified (ChecksumOK false).
func (js *jscan) readRecords(start, count int64, nonce uint32, trust bool, seg int) error {
	ps := int64(js.s.Info.PageSize)
	rsz := ps + 8
	per := max(1, journalChunk/rsz)
	for done := int64(0); done < count; {
		cnt := min(per, count-done)
		if err := js.l.alloc(cnt * journalRecordCost); err != nil {
			return fmt.Errorf("journal records: %w", err)
		}
		bufSize := cnt * rsz
		if err := js.l.alloc(bufSize); err != nil {
			return fmt.Errorf("journal read buffer: %w", err)
		}
		buf := make([]byte, bufSize)
		err := readFull(js.j, buf, start+done*rsz)
		js.l.free(bufSize)
		if err != nil {
			return err
		}
		for i := range cnt {
			raw := buf[i*rsz : (i+1)*rsz]
			ok := trust && JournalChecksum(nonce, raw[4:4+ps]) == binary.BigEndian.Uint32(raw[4+ps:])
			js.s.Records = append(js.s.Records, JournalRecord{
				Index: len(js.s.Records), Segment: seg,
				Page: binary.BigEndian.Uint32(raw), Offset: start + (done+i)*rsz + 4, ChecksumOK: ok,
			})
		}
		done += cnt
	}
	return nil
}

// decide settles the counts and the verdict of a journal with a valid header.
func (js *jscan) decide() {
	s, in := js.s, &js.s.Info
	js.count()
	switch {
	case !in.HeaderValid:
		return
	case !in.PageSizeMatchesDB:
		in.NotAppliedReason = "page-size-mismatch"
		js.warn(WarnJournalPageSizeMismatch, 24, "journal page size %d differs from the database's %d (0: unknown); the journal is not applied", in.PageSize, js.dbps)
	case in.HasSuperJournal && !js.opts.SuperJournalPresent:
		in.NotAppliedReason = "super-journal-unknown"
		js.warn(WarnJournalSuperUnknown, js.size, "the journal names a super-journal file, which is not in evidence; the engine skips the rollback when it is absent, so the journal is not applied")
	case s.capped:
		in.NotAppliedReason = "limit-reached"
	default:
		in.Applied = true
	}
	js.playback()
}

// count fills the record counts.
func (js *jscan) count() {
	in := &js.s.Info
	in.RecordsTotal = uint32(len(js.s.Records))
	for i, r := range js.s.Records {
		if r.ChecksumOK {
			in.RecordsValid++
		} else {
			in.RecordsBadChecksum++
			if in.FirstBadRecord < 0 {
				in.FirstBadRecord = i
			}
		}
	}
}

// playback replays the records as the engine does: in file order, a bad
// checksum, page 0 or the lock-byte page ends it, a page above the initial
// size is skipped, and the last record of a page wins. Problems are warned
// about whether or not the journal is applied.
func (js *jscan) playback() {
	s, in := js.s, &js.s.Info
	lock := LockBytePage(int(in.PageSize))
	winner := map[uint32]int{}
	for i := range s.Records {
		r := &s.Records[i]
		stop := false
		switch {
		case !r.ChecksumOK:
			js.problem(r.Page, r.Offset, "record %d (page %d) fails its checksum; playback ends here, as the engine's does", i, r.Page)
			stop = true
		case r.Page == 0 || r.Page == lock:
			js.problem(r.Page, r.Offset, "record %d is for page %d, which can never be restored; playback ends here", i, r.Page)
			stop = true
		case r.Page > in.InitialPages:
			js.problem(r.Page, r.Offset, "record %d is for page %d, above the initial size of %d pages; skipped", i, r.Page, in.InitialPages)
		default:
			if _, dup := winner[r.Page]; dup {
				js.w.add(Warning{Code: WarnJournalDuplicatePage, File: FileJournal, Page: r.Page, Offset: r.Offset, Msg: fmt.Sprintf("page %d is journaled more than once; the last record wins", r.Page)})
			}
			winner[r.Page] = i
		}
		if stop {
			break
		}
	}
	if !in.Applied {
		return
	}
	if err := js.l.alloc(int64(len(winner)) * journalMapCost); err != nil {
		// not enough budget for the map: the journal is not applied, never half applied
		in.Applied, in.NotAppliedReason = false, "limit-reached"
		js.warn(WarnLimitReached, 0, "the page map of the journal does not fit the budget; the journal is not applied")
		return
	}
	s.winner = winner
	for pg, i := range winner {
		s.Records[i].Applied = true
		s.maxApplied = max(s.maxApplied, pg)
	}
	in.AppliedRecords = uint32(len(winner))
}

func (js *jscan) hotWarning() {
	in := &js.s.Info
	if !in.Hot {
		return
	}
	if in.Applied {
		js.warn(WarnJournalHot, 0, "a hot journal is present: Live() presents the database rolled back to its state before the interrupted transaction (%d of %d records applied); AsFound() shows the file as found", in.AppliedRecords, in.RecordsTotal)
		return
	}
	js.warn(WarnJournalHot, 0, "a hot journal is present but is not applied (%s)", in.NotAppliedReason)
}

// recoverZeroed reads the records left behind by a journal whose header was
// zeroed (PERSIST mode). The page size is the database's, the sector size is
// the candidate that makes the most records agree on a nonce, and the nonce is
// the consensus of (stored checksum - sampled bytes) over at least two records;
// without a consensus the records are listed unverified. Nothing is applied.
func (js *jscan) recoverZeroed() error {
	s, in := js.s, &js.s.Info
	js.warn(WarnJournalHeaderInvalid, 0, "the header is zeroed (a PERSIST journal after commit); the journal is not hot and is not applied; records left behind are listed")
	if !validJournalPageSize(uint32(max(js.dbps, 0))) {
		js.warn(WarnJournalNoPageSize, 0, "the header is zeroed and the database page size is unknown; no record can be cut")
		return nil
	}
	regionEnd, err := js.regionEnd()
	if err != nil {
		return err
	}
	ps := int64(js.dbps)
	rsz := ps + 8
	in.PageSize = uint32(ps)
	in.PageSizeMatchesDB = true
	var bestS int64
	var bestCount int
	var bestNonce uint32
	fallbackS := int64(0)
	for sec := int64(journalMinSector * 16); sec <= journalMaxSector; sec *= 2 { // 512..65536
		if sec+rsz > regionEnd {
			break
		}
		n := min(journalSampleRecs, (regionEnd-sec)/rsz)
		if err := js.l.alloc(n * rsz); err != nil {
			return fmt.Errorf("journal sample: %w", err)
		}
		buf := make([]byte, n*rsz)
		err := readFull(js.j, buf, sec)
		js.l.free(n * rsz)
		if err != nil {
			return err
		}
		votes := map[uint32]int{}
		for i := range n {
			raw := buf[i*rsz : (i+1)*rsz]
			if binary.BigEndian.Uint32(raw) == 0 {
				continue // zero padding or a page-0 record never votes for a nonce
			}
			votes[binary.BigEndian.Uint32(raw[4+ps:])-JournalChecksum(0, raw[4:4+ps])]++
		}
		for nonce, c := range votes {
			if c > bestCount || (c == bestCount && sec < bestS) || (c == bestCount && sec == bestS && nonce < bestNonce) {
				bestCount, bestS, bestNonce = c, sec, nonce
			}
		}
		if pg := binary.BigEndian.Uint32(buf); fallbackS == 0 && pg != 0 && int64(pg) <= js.e.opts.Limits.MaxPages {
			fallbackS = sec
		}
	}
	trust := bestCount >= 2
	start := bestS
	if !trust {
		start = fallbackS
	}
	if start == 0 {
		return nil
	}
	count := max(regionEnd-start, 0) / rsz
	if rem := js.e.opts.Limits.MaxJournalRecords; count > rem {
		count = rem
		s.capped = true
		js.warn(WarnLimitReached, 0, "more than %d records; the rest are not read", rem)
	}
	in.SectorSize = uint32(start)
	if trust {
		in.NonceDerived, in.Nonce = true, bestNonce
	}
	in.Segments = []JournalSegment{{Offset: 0, DeclaredRecords: uint32(count), Records: uint32(count)}}
	if err := js.l.alloc(journalSegmentCost); err != nil {
		return err
	}
	if err := js.readRecords(start, count, bestNonce, trust, 0); err != nil {
		return err
	}
	in.TrailingBytes = max(regionEnd-(start+count*rsz), 0)
	js.count()
	return nil
}

// attachedJournal is a journal attached to a DB.
type attachedJournal struct {
	r    io.ReaderAt
	size int64
	scan *JournalScan
}

// AttachJournal scans the rollback journal j (size bytes) of this database and
// attaches it: a view taken by Live afterwards presents the database rolled
// back when the journal is hot and applies (JournalInfo.Applied); AsFound never
// does. A second call is ErrAlreadyAttached. Views taken before the call do not
// change.
func (d *DB) AttachJournal(j io.ReaderAt, size int64) (info *JournalInfo, err error) {
	defer guard(&err)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.jr != nil {
		return nil, ErrAlreadyAttached
	}
	scan, err := ScanJournal(j, size, d.info.PageSize, d.env.opts)
	if err != nil {
		return nil, err
	}
	for _, x := range scan.Warnings {
		d.warns.add(x)
	}
	d.jr = &attachedJournal{r: j, size: size, scan: scan}
	cp := scan.Info
	cp.Segments = append([]JournalSegment(nil), scan.Info.Segments...)
	return &cp, nil
}

// Journal returns the scan of the attached journal, or nil when none is
// attached. The result is shared and must not be modified.
func (d *DB) Journal() *JournalScan {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.jr == nil {
		return nil
	}
	return d.jr.scan
}

// AsFound returns the raw view: the database file as found, plus the WAL
// overlay when a WAL is attached, and never a journal rollback. With no applied
// journal it equals Live(). Nothing in the library uses it by default.
func (d *DB) AsFound() *View { return d.view(false) }

// journalOverlay is a pageSource that serves, for each page a hot journal
// restores, the before-image of the record that wins (the last playable one),
// and every other page from the lower source. The database is rolled back to
// its initial size: pages above it are unavailable.
type journalOverlay struct {
	lower   pageSource
	j       io.ReaderAt
	size    int64
	ps      int
	initial uint32
	recs    []JournalRecord
	winner  map[uint32]int
}

func (o *journalOverlay) has(pgno uint32) bool {
	if pgno == 0 || pgno > o.initial {
		return false
	}
	if _, ok := o.winner[pgno]; ok {
		return true
	}
	return o.lower.has(pgno)
}

func (o *journalOverlay) read(pgno uint32) ([]byte, PageLoc, error) {
	if pgno == 0 || pgno > o.initial {
		return nil, PageLoc{}, fmt.Errorf("%w: page %d is beyond the %d pages the journal's transaction started with", ErrPageUnavailable, pgno, o.initial)
	}
	idx, ok := o.winner[pgno]
	if !ok {
		return o.lower.read(pgno)
	}
	off := o.recs[idx].Offset
	p := make([]byte, o.ps)
	if err := readFull(o.j, p, off); err != nil {
		return nil, PageLoc{}, err
	}
	return p, PageLoc{File: FileJournal, Offset: off, Record: idx}, nil
}

func (o *journalOverlay) fileOf(pgno uint32) FileKind {
	if _, ok := o.winner[pgno]; ok && pgno != 0 && pgno <= o.initial {
		return FileJournal
	}
	return sourceFile(o.lower, pgno)
}

// applyJournal layers the applied journal over v's sources (the database file):
// v.src, v.info and the warnings follow the rolled-back state.
func (v *View) applyJournal(a *attachedJournal) {
	s := a.scan
	d := v.d
	in := &s.Info
	ov := &journalOverlay{lower: v.src, j: a.r, size: a.size, ps: d.info.PageSize, initial: in.InitialPages, recs: s.Records, winner: s.winner}
	v.src = ov
	// Pages the journal's initial size claims beyond the file that no applied
	// record supplies are unavailable, never zero-filled (the engine zero-fills).
	if in.InitialPages > v.info.FilePages {
		gap := int64(in.InitialPages - v.info.FilePages)
		for pg := range s.winner {
			if pg > v.info.FilePages && pg <= in.InitialPages {
				gap--
			}
		}
		first := v.info.FilePages + 1
		for hasKey(s.winner, first) {
			first++
		}
		if gap > 0 {
			v.warns.add(Warning{Code: WarnLivePagesUnavailable, File: FileDB, Page: first, Msg: fmt.Sprintf(
				"%d pages below the journal's initial size are in neither the database file nor an applied journal record (first is page %d); they are unavailable, never zero-filled", gap, first)})
		}
	}
	phys := max(v.info.FilePages, s.maxApplied)
	info := v.info
	if _, ok := s.winner[1]; ok {
		if hdr, _, err := ov.read(1); err == nil && len(hdr) >= headerSize {
			sz := min(int64(phys)*int64(d.info.PageSize), 1<<62)
			scratch := newWarnings(v.e.opts.Limits.MaxWarnings)
			if pi, perr := parseHeader(hdr[:headerSize], sz, scratch); perr != nil || pi.PageSize != d.info.PageSize || pi.Reserved != d.info.Reserved {
				v.warns.add(Warning{Code: WarnJournalPageInvalid, File: FileJournal, Page: 1, Msg: "page 1 in the journal has a header that does not fit the database (page size, reserved bytes or structure); the database's header is kept"})
			} else {
				for _, x := range scratch.snapshot() {
					x.File = FileJournal
					v.warns.add(x)
				}
				info = pi
			}
		}
	}
	info.FileSize = d.size
	info.FilePages = phys
	info.PageCount = in.InitialPages
	v.info = info
}

func hasKey(m map[uint32]int, k uint32) bool { _, ok := m[k]; return ok }
