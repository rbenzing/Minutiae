package dbrecover

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// Test seams (export_test.go): the function Run (and Plan) calls between its last flush and
// the fresh hash of the inputs, a change to each row before it is mapped, and
// the number of rows between two Progress calls.
var (
	afterFlushHook func()
	rowHook        func(sqlitefile.RecoveredRow) sqlitefile.RecoveredRow
	progressEvery  int64 = 1000
)

// maxOutsideListed bounds the rows held back for the outside-range warnings;
// the writer caps the warnings it audits at the same number.
const maxOutsideListed = 10_000

// resolveHook is a test seam (export_test.go): resolve calls it once the
// database input is known.
var resolveHook func()

// schemaReadHook is a test seam (export_test.go): the engine-refusal gate calls
// it before reading the schema and takes its error as the read error.
var schemaReadHook func() error

const planNote = "assessment only: nothing was written"

// pipeline is the work Run and Plan share: the inputs, the library handles and
// the facts read from them.
type pipeline struct {
	c        *evidence.Case
	in       Inputs
	o        Options
	narrowed []string

	man                        []evidence.ManifestRecord
	dbRec                      evidence.ManifestRecord
	rep                        SiblingReport
	set                        *sealedSet
	pairingWAL, pairingJournal string // "explicit" | "by-name" | "none", per companion

	db     *sqlitefile.DB
	hist   *sqlitefile.Hist
	hsum   sqlitefile.HistSummary
	facts  map[string]map[string]any // role -> facts read after attaching
	unused map[string]string         // role -> why a discovered companion is not applied
}

func (p *pipeline) release() {
	if p.hist != nil {
		p.hist.Release()
	}
	if p.set != nil {
		p.set.close()
	}
}

// resolve reads the manifest, finds the database and its companions and opens
// (and hashes) every input.
func (p *pipeline) resolve(ctx context.Context) error {
	if p.in.NoSiblings && (p.in.WAL != "" || p.in.Journal != "") {
		return badOption("the sibling search is turned off, yet a WAL or journal was named: choose one")
	}
	man, err := p.c.Manifest()
	if err != nil {
		return fmt.Errorf("read the manifest: %w", err)
	}
	p.man = man
	rec, err := p.c.FindArtifact(p.in.DB)
	if err != nil {
		return fmt.Errorf("database input: %w", err)
	}
	ix := slices.IndexFunc(man, func(r evidence.ManifestRecord) bool { return r.ID == rec.ID })
	if ix < 0 {
		return fmt.Errorf("database input: %w: %q", evidence.ErrUnknownArtifact, p.in.DB)
	}
	p.dbRec = man[ix]
	if resolveHook != nil {
		resolveHook()
	}

	var found SiblingReport
	if !p.in.NoSiblings && (p.in.WAL == "" || p.in.Journal == "") {
		found = FindSiblings(man, p.dbRec)
	}
	pick := func(role, explicit string, res SiblingResult) (SiblingResult, string, error) {
		switch {
		case explicit != "":
			return SiblingResult{Status: SiblingExplicit, ArtifactID: explicit}, explicit, nil
		case p.in.NoSiblings:
			return SiblingResult{Status: SiblingDisabled}, "", nil
		}
		switch res.Status {
		case SiblingFound:
			return res, res.ArtifactID, nil
		case SiblingAmbiguous:
			return res, "", &RefusalError{Reason: ReasonAmbiguousSibling, Err: res.Err(role)}
		case SiblingError:
			return res, "", &RefusalError{Reason: ReasonSiblingUnusable, Err: res.Err(role)}
		}
		return res, "", nil
	}
	var walID, jrID string
	var werr, jerr error
	p.rep.WAL, walID, werr = pick("wal", p.in.WAL, found.WAL)
	p.rep.Journal, jrID, jerr = pick("journal", p.in.Journal, found.Journal)
	if err := errors.Join(werr, jerr); err != nil {
		return err
	}
	// how each companion came to be used, apart: naming one and finding the other by
	// name is two different truths (J119)
	pairingOf := func(explicit, id string) string {
		switch {
		case explicit != "":
			return "explicit"
		case id != "":
			return "by-name"
		}
		return "none"
	}
	p.pairingWAL, p.pairingJournal = pairingOf(p.in.WAL, walID), pairingOf(p.in.Journal, jrID)
	set, err := openInputs(ctx, p.c, man, p.dbRec.ID, walID, jrID)
	if err != nil {
		return err
	}
	p.set = set
	return nil
}

func dbFacts(i sqlitefile.Info) map[string]any {
	return map[string]any{
		"page_size": int64(i.PageSize), "page_count": int64(i.PageCount), "file_size": i.FileSize,
		"encoding": i.Encoding.String(), "freelist_count": int64(i.FreelistCount),
	}
}

func walFacts(w *sqlitefile.WALInfo) map[string]any {
	return map[string]any{
		"present": w.Present, "header_valid": w.HeaderValid, "header_problem": w.HeaderProblem, "used_by_live": w.UsedByLive,
		"version_refused": w.VersionRefused, "size": w.Size, "frame_slots": int64(w.FrameSlots),
		"frames_committed": int64(w.FramesCommitted), "frames_uncommitted": int64(w.FramesUncommitted),
		"frames_stale": int64(w.FramesStale), "commits": int64(w.Commits),
	}
}

func journalFacts(j *sqlitefile.JournalInfo) map[string]any {
	return map[string]any{
		"present": j.Present, "header_valid": j.HeaderValid, "zeroed_header": j.ZeroedHeader, "hot": j.Hot, "applied": j.Applied, "truncated": j.Truncated,
		"not_applied_reason": j.NotAppliedReason, "has_super_journal": j.HasSuperJournal,
		"super_journal_present": "false (not modelled)", "size": j.Size,
		"records_total": int64(j.RecordsTotal), "records_valid": int64(j.RecordsValid), "records_bad_checksum": int64(j.RecordsBadChecksum),
	}
}

func walUnusable(w *sqlitefile.WALInfo) string {
	switch {
	case w.VersionRefused:
		return "the WAL version is not supported (the engine refuses it)"
	case !w.HeaderValid && w.HeaderProblem != "":
		return "the WAL header is not valid: " + w.HeaderProblem
	case !w.HeaderValid:
		return "the WAL header is not valid"
	}
	return ""
}

func journalUnusable(j *sqlitefile.JournalInfo) string {
	switch {
	case j.HeaderValid || j.ZeroedHeader:
		return ""
	case j.Size == 0:
		return "the journal file is empty"
	case !j.Hot:
		return "the file is not a hot journal and its header is not zeroed"
	}
	return "the journal header is not valid"
}

// attach opens the database through the library, attaches the companions and
// reads the facts of each. A database the library cannot present is a refusal
// (D11); so is an explicitly named companion that cannot be used (D9).
func (p *pipeline) attach(ctx context.Context) error {
	p.facts = map[string]map[string]any{}
	p.unused = map[string]string{}
	db, err := sqlitefile.Open(p.set.DB.f, p.set.DB.Ref.Size, sqlitefile.Options{Limits: sqlitefile.Limits{MaxFitEqualityRows: p.o.MaxFitEqualityRows}})
	if err != nil {
		if errors.Is(err, sqlitefile.ErrNotSQLite) {
			return &RefusalError{Reason: ReasonNotSQLite, Err: fmt.Errorf("%w: %w", ErrNotSQLite, err)}
		}
		return fmt.Errorf("open the database: %w", err)
	}
	p.db = db
	info := db.Info()
	p.facts["db"] = dbFacts(info)
	if len(info.EngineRefuses) > 0 {
		return &RefusalError{Reason: ReasonEngineRefuses, Detail: strings.Join(info.EngineRefuses, "; "), Err: sqlitefile.ErrEngineRefuses}
	}
	var refuse error
	if in := p.set.WAL; in != nil {
		wi, err := db.AttachWAL(in.f, in.Ref.Size)
		if err != nil {
			return fmt.Errorf("attach the WAL %s: %w", in.Ref.ArtifactID, err)
		}
		p.facts["wal"] = walFacts(wi)
		if why := walUnusable(wi); why != "" {
			refuse = errors.Join(refuse, p.companionUnusable("wal", in, p.in.WAL != "", why))
		}
	}
	if in := p.set.Journal; in != nil {
		ji, err := db.AttachJournal(in.f, in.Ref.Size)
		if err != nil {
			return fmt.Errorf("attach the journal %s: %w", in.Ref.ArtifactID, err)
		}
		p.facts["journal"] = journalFacts(ji)
		if why := journalUnusable(ji); why != "" {
			refuse = errors.Join(refuse, p.companionUnusable("journal", in, p.in.Journal != "", why))
		}
	}
	if refuse != nil {
		return refuse
	}
	// Some reasons show only once the schema is read (a schema tree deeper than
	// the engine follows, a row whose name disagrees with its statement, an
	// index on no table). The header-only Info above misses them. A schema that
	// cannot be read here is a refusal too (never a pass: Schema caches only
	// successes, so a later read could succeed with the check skipped); only a
	// cancelled context is reported as itself.
	live := db.Live() // after the companions: the view the run reads
	var serr error
	if schemaReadHook != nil {
		serr = schemaReadHook()
	}
	if serr == nil {
		_, serr = live.Schema(ctx)
	}
	if serr != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if errors.Is(serr, sqlitefile.ErrLiveUnavailable) {
			return &RefusalError{Reason: ReasonLiveUnavailable, Err: fmt.Errorf("%w: %w", ErrLiveUnavailable, serr)}
		}
		return &RefusalError{Reason: ReasonEngineRefuses, Detail: "the schema cannot be read: " + serr.Error(), Err: fmt.Errorf("%w: the schema cannot be read: %w", sqlitefile.ErrEngineRefuses, serr)}
	}
	if why := live.Info().EngineRefuses; len(why) > 0 {
		return &RefusalError{Reason: ReasonEngineRefuses, Detail: strings.Join(why, "; "), Err: sqlitefile.ErrEngineRefuses}
	}
	return nil
}

// companionUnusable records that a companion is not applied. A named one is a
// refusal; a discovered one stays in the run, marked on every row (D9).
func (p *pipeline) companionUnusable(role string, in *sealedInput, explicit bool, why string) error {
	p.facts[role]["unused_reason"] = why
	if explicit {
		return &RefusalError{Reason: ReasonSiblingUnusable, Detail: fmt.Sprintf("%s artifact %s: %s", role, in.Ref.ArtifactID, why)}
	}
	p.unused[role] = why
	in.Ref.Unused = why
	if role == "wal" {
		p.rep.WAL.Unused = why
	} else {
		p.rep.Journal.Unused = why
	}
	return nil
}

// summarize starts the history and reads its summary. A database whose live
// state the library cannot present is a refusal (D11).
func (p *pipeline) summarize(ctx context.Context) error {
	p.hist = p.db.History()
	s, err := p.hist.Summary(ctx)
	if err != nil {
		if errors.Is(err, sqlitefile.ErrLiveUnavailable) {
			return &RefusalError{Reason: ReasonLiveUnavailable, Err: fmt.Errorf("%w: %w", ErrLiveUnavailable, err)}
		}
		return fmt.Errorf("summary of the history: %w", err)
	}
	p.hsum = s
	return nil
}

// applied lists the inputs whose bytes are examined: the database and the
// companions the library applies.
func (p *pipeline) applied() []string {
	ids := []string{p.set.DB.Ref.ArtifactID}
	if in := p.set.WAL; in != nil && p.unused["wal"] == "" {
		ids = append(ids, in.Ref.ArtifactID)
	}
	if in := p.set.Journal; in != nil && p.unused["journal"] == "" {
		ids = append(ids, in.Ref.ArtifactID)
	}
	return ids
}

func (p *pipeline) inputInfos() []InputInfo {
	var out []InputInfo
	for _, in := range p.set.inputs() {
		r := in.Ref
		out = append(out, InputInfo{
			Role: r.Role, ArtifactID: r.ArtifactID, SHA256: r.SHA256, Size: r.Size,
			Recovered: r.Recovered, Incomplete: r.Incomplete, Cap: r.Cap, Facts: p.facts[r.Role],
		})
	}
	return out
}

func (p *pipeline) mapper() *mapper {
	m := &mapper{
		byFile: map[sqlitefile.FileKind]inputRef{sqlitefile.FileDB: p.set.DB.Ref},
		cap:    p.set.cap(), mixed: p.set.mixed(), capUnknown: p.set.capUnknown(), incomplete: p.set.incomplete(),
		pairingWAL: p.pairingWAL, pairingJournal: p.pairingJournal, maxPayload: p.o.MaxPayloadBytes, maxBody: p.o.MaxBodyBytes,
	}
	if in := p.set.WAL; in != nil {
		m.byFile[sqlitefile.FileWAL] = in.Ref
	}
	if in := p.set.Journal; in != nil {
		m.byFile[sqlitefile.FileJournal] = in.Ref
	}
	return m
}

// outsideRow is a row whose cell range lies outside its artifact (J40).
type outsideRow struct{ path, reason string }

// scanState collects what one pass over the recovered rows finds.
type scanState struct {
	counts     Counts
	bytes      int64
	added      int64
	skipped    int64
	outside    []outsideRow
	slackWarns []string // the slack carve's warnings, sent with the library's
	err        error
}

// recordBytes is the size a record adds to the run's byte total, or an error
// when its payload cannot be measured: the caller fails closed (J113).
func recordBytes(r records.Record) (int64, error) {
	n, err := encodedSize(r.Payload)
	if err != nil {
		return 0, err
	}
	return int64(len(r.Summary)+len(r.Body)+len(r.Locator)) + n, nil
}

func (s *scanState) limit(name string) {
	if !slices.Contains(s.counts.LimitsHit, name) {
		s.counts.LimitsHit = append(s.counts.LimitsHit, name)
	}
}

func (s *scanState) skip(reason string) {
	s.skipped++
	s.counts.SkippedByReason[reason]++
}

// scan maps every recovered row, applies the filter and the run limits and
// hands the record to add. A record the writer refuses as invalid is counted
// and skipped (D14); anything else ends the pass.
func (p *pipeline) scan(ctx context.Context, add func(records.Record) error, track func(*scanState)) (*scanState, error) {
	s := &scanState{counts: Counts{ByMethod: map[string]int64{}, SkippedByReason: map[string]int64{}, ConfidenceRange: map[string]ConfRange{}}}
	if track != nil {
		track(s)
	}
	m := p.mapper()
	flt := newFilter(p.o)
	visit := func(r sqlitefile.RecoveredRow) (more bool) {
		// A panic in the mapping or in a callback ends the pass with a PanicError of
		// this package, before the library can turn it into its own.
		defer func() {
			if x := recover(); x != nil {
				s.err, more = &PanicError{Value: x}, false
			}
		}()
		if err := ctx.Err(); err != nil {
			s.err = err
			return false
		}
		if rowHook != nil {
			r = rowHook(r)
		}
		rec, err := m.record(r)
		if err != nil {
			s.err = err
			return false
		}
		conf := *rec.Confidence
		if keep, why := flt.keep(r, conf); !keep {
			s.skip(why)
			return true
		}
		if s.added >= p.o.MaxRecords {
			s.limit("max-records")
			return false
		}
		sz, err := recordBytes(rec)
		if err != nil {
			s.err = err
			return false
		}
		if s.bytes+sz > p.o.MaxBytes {
			s.limit("max-bytes")
			return false
		}
		if err := add(rec); err != nil {
			switch {
			case errors.Is(err, records.ErrRecoveredArtifactLiveRecord), errors.Is(err, records.ErrConfidenceAboveArtifact):
				s.err = fmt.Errorf("mapper defect: %w", err)
				return false
			case errors.Is(err, records.ErrInvalidRecord):
				s.skip("writer-rejected")
				return true
			}
			s.err = err
			return false
		}
		s.added++
		s.bytes += sz
		c := &s.counts
		c.ByMethod[r.Method]++
		if m.capUnknown {
			c.RecoveryCapUnknown++
		}
		if r.Relation == sqlitefile.RelUnknown {
			c.RelationUnknown++
		}
		if slices.Contains(r.Notes, sqlitefile.NoteIdentityByFitOnly) {
			c.FitOnly++
		}
		cr, seen := c.ConfidenceRange[r.Method]
		if !seen || conf < cr.Min {
			cr.Min = conf
		}
		if !seen || conf > cr.Max {
			cr.Max = conf
		}
		c.ConfidenceRange[r.Method] = cr
		if rec.Range == nil {
			c.RangeOutside++
			if len(s.outside) < maxOutsideListed {
				size := m.byFile[r.Loc.File].Size
				s.outside = append(s.outside, outsideRow{
					path: rec.ArtifactID + " " + rec.Locator,
					reason: fmt.Sprintf("cell range lies outside the artifact (size %d, claimed offset %d length %d): the claim is kept in loc_offset and loc_length and the record has no byte range",
						size, r.Loc.Offset, r.Loc.Length),
				})
			}
		}
		if p.o.Progress != nil && progressEvery > 0 && (s.added+s.skipped)%progressEvery == 0 {
			p.o.Progress(Progress{Rows: s.added, Skipped: s.skipped})
		}
		return true
	}
	st, err := p.hist.Rows(ctx, visit)
	c := &s.counts
	c.DuplicateOfLive, c.CellsRejected, c.InteriorSkipped = st.DuplicateOfLive, st.CellsRejected, st.InteriorSkipped
	c.FitEqualityCapped = st.FitEqualityCapped // an annotation limit: counted and warned, never a narrowing
	for _, l := range st.LimitsHit {
		s.limit(l)
	}
	// The second pass (slack, J5) reads the history again after Rows and never
	// touches what Rows found: it only adds its own rows and counters.
	if err == nil && s.err == nil && slackCarving && len(s.counts.LimitsHit) == 0 {
		ss, serr := carveSpans(ctx, p.db, p.hist, visit)
		c.SlackCopiesOfLive, c.SlackUnparsedBytes, c.SlackDroppedCrossed = ss.DroppedLiveCopy, ss.UnparsedBytes, ss.DroppedCrossed
		c.SlackDroppedOverlap, c.SlackCrossPageEqual, c.SlackWorkCapped, c.SlackRowCapped = ss.DroppedOverlap, ss.CrossPageEqual, ss.WorkCapped, ss.RowCapped
		s.slackWarns = ss.warnings
		if err = serr; err == nil {
			err = s.err
		}
	}
	c.FreePages, c.FreePagesZeroed = p.hsum.FreePages, p.hsum.FreePagesZeroed
	c.PagesByOrigin = map[string]int64{}
	for o, n := range p.hsum.PagesByOrigin {
		c.PagesByOrigin[o.String()] = n
	}
	c.Narrowed = slices.Clone(p.narrowed)
	// A run that lost rows is narrowed (J112): the writer refused some, or the slack
	// pass stopped at a cap. It is kept and concluded incomplete, never superseding.
	if c.SkippedByReason["writer-rejected"] > 0 {
		c.Narrowed = append(c.Narrowed, "writer-rejected")
	}
	if c.SlackWorkCapped > 0 {
		c.Narrowed = append(c.Narrowed, "slack-work-cap")
	}
	if c.SlackRowCapped {
		c.Narrowed = append(c.Narrowed, "slack-row-cap")
	}
	if p.o.Progress != nil {
		p.o.Progress(Progress{Rows: s.added, Skipped: s.skipped})
	}
	if s.err == nil {
		s.err = err
	}
	return s, s.err
}

// libraryWarnings collects the warnings of the database and of the history
// after the last pass, without repeats.
func (p *pipeline) libraryWarnings() []sqlitefile.Warning {
	type key struct {
		code   string
		file   sqlitefile.FileKind
		page   uint32
		offset int64
		msg    string
	}
	seen := map[key]bool{}
	var out []sqlitefile.Warning
	for _, w := range append(p.db.Warnings(), p.hist.Warnings()...) {
		k := key{w.Code, w.File, w.Page, w.Offset, w.Msg}
		if !seen[k] {
			seen[k] = true
			out = append(out, w)
		}
	}
	return out
}

// sendWarnings audits what the pass found as analysis.warning entries through
// the writer, in a fixed order: rows outside their artifact (J40), the mixed or
// incomplete inputs (D10), the run limits, then the library's warnings.
func (p *pipeline) sendWarnings(ctx context.Context, w *records.Writer, s *scanState) error {
	for _, o := range s.outside {
		if err := w.Warn(ctx, o.path, o.reason); err != nil {
			return err
		}
	}
	if more := s.counts.RangeOutside - int64(len(s.outside)); more > 0 {
		if err := w.Warn(ctx, "", fmt.Sprintf("%d further rows lie outside their artifact and are not listed one by one", more)); err != nil {
			return err
		}
	}
	var why []string
	if p.set.mixed() {
		why = append(why, "mixed inputs: some inputs are recovered and others are live; the rows carry mixed_inputs and the lowest confidence ceiling of all inputs")
	}
	if p.set.incomplete() {
		why = append(why, "an input is flagged incomplete: the rows carry input_incomplete")
	}
	if p.set.capUnknown() {
		why = append(why, fmt.Sprintf("an input is recovered and its confidence cap is unknown: its rows are capped at %d and carry the note recovery-cap-unknown", unknownCapConfidence))
	}
	if len(why) > 0 {
		if err := w.Warn(ctx, "", strings.Join(why, "; ")); err != nil {
			return err
		}
	}
	for _, l := range s.counts.LimitsHit {
		if err := w.Warn(ctx, "", fmt.Sprintf("run limit %s reached: the rows after it were not examined or written", l)); err != nil {
			return err
		}
	}
	for _, m := range s.slackWarns {
		if err := w.Warn(ctx, "", "slack carving: "+m); err != nil {
			return err
		}
	}
	lw := p.libraryWarnings()
	s.counts.LibraryWarnings = len(lw)
	for _, x := range lw {
		path, reason := warningText(x)
		if err := w.Warn(ctx, path, reason); err != nil {
			return err
		}
	}
	return nil
}

// endDetails is what analysis.end and analysis.error carry.
func (p *pipeline) endDetails(c Counts, ir records.IngestResult) map[string]any {
	d := countsDetails(c)
	comp := map[string]any{}
	for _, r := range []string{"wal", "journal"} {
		if f, ok := p.facts[r]; ok {
			comp[r] = f
		}
	}
	d["companions"] = comp
	if f, ok := p.facts["db"]; ok {
		d["db"] = f
	}
	d["files"], d["skipped"] = c.Written, sumCounts(c.SkippedByReason)
	if ir.IngestID != "" {
		d["ingest_id"] = ir.IngestID
		d["outcome"] = ir.Outcome
		d["warnings_suppressed"] = int64(ir.WarningsSuppressed)
		d["rejected"] = int64(ir.Rejected)
	}
	return d
}

func sumCounts(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// Run recovers the deleted rows of one database and its companions and writes
// them as db_row records, audited from analysis.start to analysis.end. The
// order is pinned: the inputs are hashed before anything is parsed, the rows
// are flushed and the warnings sent before the fresh hash of every input, and
// the ingest is ended only after that hash matched. An input that changed is an
// integrity error that no cancel and no other error hides. A panic is recovered
// into a PanicError: the writer is flushed and aborted, never ended.
func Run(ctx context.Context, c *evidence.Case, in Inputs, opts Options) (res Result, err error) {
	a := &analysis{c: c, id: evidence.NewAcquisitionID(time.Now())}
	res = Result{AnalysisID: a.id, RulesHash: RulesHash()}
	o, narrowed, err := opts.normalized()
	if err != nil {
		return res, a.finish(err, nil)
	}
	p := &pipeline{c: c, in: in, o: o, narrowed: narrowed}
	defer p.release()
	var (
		w    *records.Writer
		open bool       // the writer has started and has not concluded
		cur  *scanState // the pass in progress
	)
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		perr := &PanicError{Value: r}
		if p.dbRec.ID != "" { // a panic before analysis.start still names the database it was about
			a.parentID, a.deviceID = p.dbRec.ID, p.dbRec.Source.DeviceID
		}
		var counts Counts
		if cur != nil {
			counts = cur.counts
		}
		var ir records.IngestResult
		cause := error(perr)
		if open {
			var aerr error
			ir, aerr = abortAfterPanic(context.WithoutCancel(ctx), w, perr)
			cause = errors.Join(perr, aerr)
			counts.Written = ir.Records
		}
		if p.set != nil {
			res.Siblings, res.Inputs, res.Counts, res.Ingest = p.rep, p.inputInfos(), counts, ir
		}
		err = a.finish(cause, p.endDetails(counts, ir))
	}()

	if err := p.resolve(ctx); err != nil {
		a.parentID, a.deviceID = p.dbRec.ID, p.dbRec.Source.DeviceID
		return res, a.finish(err, nil)
	}
	a.parentID, a.deviceID = p.dbRec.ID, p.dbRec.Source.DeviceID
	if err := a.start(startDetails(p.set, p.rep, o, narrowed)); err != nil {
		return res, err
	}
	fail := func(err error, counts Counts, ir records.IngestResult) (Result, error) {
		res.Siblings, res.Inputs, res.Counts, res.Ingest = p.rep, p.inputInfos(), counts, ir
		return res, a.finish(err, p.endDetails(counts, ir))
	}
	if err := p.attach(ctx); err != nil {
		return fail(err, Counts{}, records.IngestResult{})
	}
	if err := p.summarize(ctx); err != nil {
		return fail(err, Counts{}, records.IngestResult{})
	}
	w, err = records.NewWriter(c, ParserIdentity(), records.WriterOptions{BatchRows: o.BatchRows})
	if err != nil {
		return fail(err, Counts{}, records.IngestResult{})
	}
	if err := w.Start(ctx, records.StartOptions{AnalysisID: a.id, Artifacts: p.applied(), AllowReingest: o.AllowReingest}); err != nil {
		return fail(err, Counts{}, records.IngestResult{})
	}
	open = true
	res.WriterStarted = true

	s, runErr := p.scan(ctx, func(r records.Record) error { return w.Add(ctx, r) }, func(s *scanState) { cur = s })
	wc := context.WithoutCancel(ctx)
	flushErr := w.Flush(wc)
	var warnErr error
	if flushErr == nil {
		warnErr = p.sendWarnings(wc, w, s)
	}
	runErr = errors.Join(runErr, flushErr, warnErr)
	if afterFlushHook != nil {
		afterFlushHook()
	}
	// The fresh hash runs whatever happened before: a change of an input is an
	// integrity failure that a cancel or another error must not hide. A run that
	// is still going is cancelled in the hash; one that already ended is not.
	chk := ctx
	if runErr != nil {
		chk = wc
	}
	if cerr := p.set.checkUnchanged(chk); cerr != nil {
		runErr = errors.Join(runErr, cerr)
	}
	if runErr != nil {
		ir, aerr := w.Abort(wc, runErr)
		open = false
		s.counts.Written = ir.Records
		return fail(errors.Join(runErr, aerr), s.counts, ir)
	}
	if len(s.counts.Narrowed) > 0 || len(s.counts.LimitsHit) > 0 {
		ir, aerr := w.Abort(wc, ErrNarrowedRun)
		open = false
		s.counts.Written = ir.Records
		res.Siblings, res.Inputs, res.Counts, res.Ingest = p.rep, p.inputInfos(), s.counts, ir
		if aerr != nil {
			return res, a.finish(aerr, p.endDetails(s.counts, ir))
		}
		return res, a.finish(nil, p.endDetails(s.counts, ir))
	}
	ir, err := w.End(ctx)
	if err != nil {
		// End failed: conclude the writer, so its live-ingest slot is not held and the
		// ingest is recorded incomplete (J118). A writer End already closed answers
		// ErrWriterClosed, which is not a second failure.
		air, aerr := w.Abort(wc, err)
		open = false
		if aerr != nil && !errors.Is(aerr, records.ErrWriterClosed) {
			err = errors.Join(err, aerr)
		} else if aerr == nil {
			ir = air
		}
		s.counts.Written = ir.Records
		return fail(err, s.counts, ir)
	}
	open = false
	s.counts.Written = ir.Records
	res.Siblings, res.Inputs, res.Counts, res.Ingest = p.rep, p.inputInfos(), s.counts, ir
	return res, a.finish(nil, p.endDetails(s.counts, ir))
}

// abortAfterPanic flushes what the writer holds and aborts it, so the rows
// written so far stay and the ingest is concluded incomplete. A panic in either
// step is itself recovered.
func abortAfterPanic(ctx context.Context, w *records.Writer, cause error) (ir records.IngestResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r}
		}
	}()
	ferr := w.Flush(ctx)
	ir, aerr := w.Abort(ctx, cause)
	return ir, errors.Join(ferr, aerr)
}

// Plan runs the same pipeline as Run up to and including the rows, with a
// counting visit: no writer, no audit entry, nothing written.
func Plan(ctx context.Context, c *evidence.Case, in Inputs, opts Options) (_ Assessment, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r}
		}
	}()
	o, narrowed, err := opts.normalized()
	if err != nil {
		return Assessment{}, err
	}
	p := &pipeline{c: c, in: in, o: o, narrowed: narrowed}
	defer p.release()
	if err := p.resolve(ctx); err != nil {
		return Assessment{}, err
	}
	if err := p.attach(ctx); err != nil {
		return Assessment{}, err
	}
	if err := p.summarize(ctx); err != nil {
		return Assessment{}, err
	}
	// the writer's own validator judges each mapped record, so the counts match a Run (J117)
	validate, err := records.NewValidator(c, p.applied())
	if err != nil {
		return Assessment{}, err
	}
	s, err := p.scan(ctx, func(r records.Record) error { return validate(r) }, nil)
	if afterFlushHook != nil {
		afterFlushHook()
	}
	// as Run: the inputs are hashed afresh whatever happened, and a cancel or another
	// error does not hide a change (exit 4)
	chk := ctx
	if err != nil {
		chk = context.WithoutCancel(ctx)
	}
	if cerr := p.set.checkUnchanged(chk); cerr != nil {
		return Assessment{}, errors.Join(err, cerr)
	}
	if err != nil {
		return Assessment{}, err
	}
	s.counts.Written = s.added
	s.counts.LibraryWarnings = len(p.libraryWarnings())
	return Assessment{Inputs: p.inputInfos(), Siblings: p.rep, Counts: s.counts, RulesHash: RulesHash(), Note: planNote}, nil
}
