package artparse

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

const (
	viaTypedRecovery  = "typed-recovery"
	noteRangeOutside  = "range-outside-artifact"
	maxFactNotes      = 32  // notes one row's facts may carry
	maxFactNoteBytes  = 128 // bytes of one such note
	unmappedWarnLabel = "recovered row not mapped: "
)

// maxUnmappedLocators bounds the locators a recovery emitter remembers (4G-R25); past it the overflow
// flag is set and the job honors no claim. A recovery emitter holds its own copy (maxLoc), so a test
// that lowers it never touches shared state.
const maxUnmappedLocators = 1 << 16

// bundleFacts is what the stamp knows about the members of a recovery job's bundle.
type bundleFacts struct {
	byRole     map[string]artInfo
	cap        *int // the lowest cap of any recovered member (25 when one cannot be read); nil when none
	capUnknown bool
	capFrom    string // the recovered member that bounds the cap
	ids        []string
	recovered  *bundleRecovery // nil when no member is recovered
}

func newBundleFacts(arts map[string]artInfo, br *bundleRecovery) bundleFacts {
	b := bundleFacts{byRole: make(map[string]artInfo, len(arts)), recovered: br}
	for id, a := range arts {
		b.byRole[a.role] = a
		b.ids = append(b.ids, id)
	}
	sort.Strings(b.ids)
	if br != nil {
		b.cap, b.capUnknown, b.capFrom = br.cap, br.capUnknown, br.owner
	}
	return b
}

// checkNotes bounds the notes of a row's facts: their number and size are a typed error, never a cut.
func checkNotes(notes []string) error {
	if len(notes) > maxFactNotes {
		return fmt.Errorf("%w: %d notes (at most %d)", parse.ErrBadRecoveredFacts, len(notes), maxFactNotes)
	}
	for _, n := range notes {
		if n == "" || len(n) > maxFactNoteBytes {
			return fmt.Errorf("%w: a note of %d bytes (1 to %d)", parse.ErrBadRecoveredFacts, len(n), maxFactNoteBytes)
		}
	}
	return nil
}

// stampRecovered returns r stamped with the facts of its row (4G-R7): Deleted, the row's method, the
// confidence capped by every member of the bundle, the artifact of the row's role, the cell range when it
// lies inside that artifact, the locator, the member's remote path and payload.recovery. The mapper's own
// Time, Times, Body, Summary and typed payload fields are kept. f must have passed Validate and
// checkNotes. r and f are not modified. A payload.recovery in r is refused: only the host writes it.
func stampRecovered(r records.Record, f parse.RecoveredFacts, b bundleFacts) (records.Record, []string, error) {
	info, ok := b.byRole[string(f.Role)]
	if !ok {
		return r, nil, fmt.Errorf("%w: the bundle has no %s member for the row at %s", records.ErrInvalidRecord, f.Role, f.Locator)
	}
	if _, set := r.Payload["recovery"]; set {
		return r, nil, fmt.Errorf("%w: payload.recovery is stamped by the host, the mapper set it", records.ErrInvalidRecord)
	}
	var warnings []string
	var notes []any
	addNote := func(n string) {
		if !slices.Contains(notes, any(n)) {
			notes = append(notes, n)
		}
	}
	for _, n := range f.Notes {
		addNote(n)
	}

	c := r
	c.ArtifactID, c.SourcePath, c.Locator = info.id, info.remotePath, f.Locator
	c.Deleted, c.Recovery = true, f.Method
	conf := f.Confidence
	capped := false
	if b.cap != nil && *b.cap < conf {
		conf, capped = *b.cap, true
	}
	c.Confidence = &conf
	c.Range = nil
	if g := f.Range; g.Offset >= 0 && g.Length > 0 && g.Offset <= info.size && g.Length <= info.size-g.Offset {
		c.Range = &records.Range{Offset: g.Offset, Length: g.Length}
	} else {
		addNote(noteRangeOutside)
		warnings = append(warnings, fmt.Sprintf("%s: the cell range %d+%d lies outside the %d-byte %s artifact", noteRangeOutside, g.Offset, g.Length, info.size, f.Role))
	}

	rec := map[string]any{
		"relation": f.Relation, "table_basis": f.Basis, "origin": f.Origin, "via": viaTypedRecovery,
		"page": int64(f.Page), "cell": int64(f.Cell), "file": string(f.Role),
	}
	if f.WAL != nil {
		rec["wal"] = map[string]any{"frame": int64(f.WAL.Frame), "salt1": int64(f.WAL.Salt1), "salt2": int64(f.WAL.Salt2), "committed": f.WAL.Committed}
	}
	if f.Journal != nil {
		rec["journal"] = map[string]any{"record": int64(f.Journal.Record), "segment": int64(f.Journal.Segment), "hot": f.Journal.Hot, "applied": f.Journal.Applied}
	}
	if f.Truncated {
		rec["truncated"] = true
	}
	siblings := []any{}
	for _, id := range b.ids {
		if id != info.id {
			siblings = append(siblings, id)
		}
	}
	rec["sibling_artifacts"] = siblings
	if capped {
		rec["cap_from"] = b.capFrom
	}
	if br := b.recovered; br != nil {
		src, relation := info.recovery, relationFromRec
		if src == nil {
			src, relation = br.members[br.owner].recovery, relationFromBundle
			rec["bundle_member"] = br.owner
			addNote(noteMixedBundle)
		}
		rec["artifact_method"] = src.Method
		if src.Confidence != nil {
			rec["artifact_confidence"] = *src.Confidence
		}
		rec["artifact_relation"] = relation
		if b.capUnknown {
			addNote(noteCapUnknown)
		}
	}
	rec["notes"] = notes

	p := make(map[string]any, len(r.Payload)+1)
	for k, v := range r.Payload {
		p[k] = v
	}
	p["recovery"] = rec
	c.Payload = p
	return c, warnings, nil
}

// recoveryEmitter is the Emitter of a typed recovery job. Only EmitRecovered writes records: the plain
// Emit is refused (and the embedded emitter is marked recoveryOnly, so no promoted path writes a live
// record either).
type recoveryEmitter struct {
	*emitter
	b             bundleFacts
	minConfidence int
	belowMin      atomic.Int64
	unmapped      atomic.Int64

	// The unmapped rows are remembered as dbrecover.LocatorDigest of their locator (16 bytes each, at
	// most maxLoc: a fixed 1 MiB bound), the form the composed command hands the generic run (4G-R25).
	locMu   sync.Mutex // a leaf: never held across a call
	locSet  map[[16]byte]struct{}
	locOver bool
	maxLoc  int
}

func newRecoveryEmitter(hostCtx context.Context, w ingestWriter, reg Registered, arts map[string]parse.Artifact, lim parse.Limits, pump chan<- func(), progress func(done, total int64), minConfidence int) (*recoveryEmitter, error) {
	e, err := newEmitter(hostCtx, w, reg, arts, lim, pump, progress)
	if err != nil {
		return nil, err
	}
	e.recoveryOnly = true
	return &recoveryEmitter{emitter: e, b: newBundleFacts(e.arts, e.br), minConfidence: minConfidence, locSet: map[[16]byte]struct{}{}, maxLoc: maxUnmappedLocators}, nil
}

// refuseFacts audits and counts a row whose facts the host will not stamp.
func (r *recoveryEmitter) refuseFacts(rec records.Record, f parse.RecoveredFacts, err error) error {
	rec.Locator = f.Locator
	return r.reject(rec, fmt.Errorf("%w: %w", records.ErrInvalidRecord, err))
}

func checkFacts(f parse.RecoveredFacts) error {
	if err := f.Validate(); err != nil {
		return err
	}
	return checkNotes(f.Notes)
}

// Emit refuses: a recovery job writes only through EmitRecovered. It never delegates to the embedded
// emitter.
func (r *recoveryEmitter) Emit(_ context.Context, rec records.Record) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	return r.reject(rec, fmt.Errorf("%w: %w", records.ErrInvalidRecord, parse.ErrLiveEmitInRecovery))
}

// EmitRecovered stamps rec with the facts of its row and hands it to the writer. Bad facts, a record the
// mapper already marked as recovered and a record the writer refuses are counted rejections; a record
// below the minimum confidence (after every cap) is dropped and counted in belowMin.
func (r *recoveryEmitter) EmitRecovered(_ context.Context, rec records.Record, f parse.RecoveredFacts) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if err := checkFacts(f); err != nil {
		return r.refuseFacts(rec, f, err)
	}
	if rec.Recovery != "" || rec.Confidence != nil {
		return r.refuseFacts(rec, f, fmt.Errorf("%w: the mapper marked the record as recovered", parse.ErrRecoveryFieldsFromParser))
	}
	var warnings []string
	dropped := false
	err := r.accept(rec, func(in records.Record) (records.Record, error) {
		c, err := r.copyRecord(in)
		if err != nil {
			return records.Record{}, err
		}
		c, warnings, err = stampRecovered(c, f, r.b)
		if err != nil {
			return records.Record{}, err
		}
		if err := parse.CheckRecord(r.meta, r.bundle, c); err != nil {
			return records.Record{}, err
		}
		if r.minConfidence > 0 && *c.Confidence < r.minConfidence {
			dropped = true
			return records.Record{}, errDropped
		}
		stampSnapshot(&c, r.arts[c.ArtifactID])
		return c, nil
	})
	if dropped {
		r.belowMin.Add(1)
	}
	if err != nil || dropped {
		return err
	}
	for _, w := range warnings {
		if err := r.w.Warn(r.writerCtx(), cleanAuditText(f.Locator, 4096), cleanAuditText(w, maxReason)); err != nil {
			return err
		}
		r.warnings.Add(1)
	}
	return nil
}

// Unmapped reports a claimed row that produced no record: an analysis.warning at its locator, counted,
// and its locator remembered (bounded) for the composed command.
func (r *recoveryEmitter) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	if err := r.begin(); err != nil {
		return err
	}
	defer r.end()
	if err := checkFacts(f); err != nil {
		return r.refuseFacts(records.Record{}, f, err)
	}
	if err := r.w.Warn(r.writerCtx(), cleanAuditText(f.Locator, 4096), cleanAuditText(unmappedWarnLabel+reason, maxReason)); err != nil {
		return err
	}
	r.warnings.Add(1)
	r.unmapped.Add(1)
	d := dbrecover.LocatorDigest(f.Locator)
	r.locMu.Lock()
	defer r.locMu.Unlock()
	if _, dup := r.locSet[d]; dup || r.locOver {
		return nil
	}
	if len(r.locSet) >= r.maxLoc {
		r.locOver = true
		return nil
	}
	r.locSet[d] = struct{}{}
	return nil
}

// unmappedLocators returns the digests of the remembered locators of the unmapped rows (LocatorDigest),
// sorted, and whether more rows were unmapped than could be remembered.
func (r *recoveryEmitter) unmappedLocators() ([][16]byte, bool) {
	r.locMu.Lock()
	defer r.locMu.Unlock()
	out := make([][16]byte, 0, len(r.locSet))
	for d := range r.locSet {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b [16]byte) int { return bytes.Compare(a[:], b[:]) })
	return out, r.locOver
}
