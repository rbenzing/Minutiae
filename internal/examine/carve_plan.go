package examine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/carve"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/volume"
)

// CarveListCap is how many hits a CarvePlan keeps one by one; the rest are only counted.
const CarveListCap = 10_000

// maxTypesParam is the longest "types" parameter written as a list; a longer one is recorded by its digest.
const maxTypesParam = 256

// Skip reasons of an accepted hit that is not written (the output limits add max-files and max-bytes).
const (
	carveSkipBelowMin = "below-min-confidence"
	carveSkipNotSel   = "type-not-selected"
	carveSkipNoName   = "no-local-name"
)

// carveUnknownCap is the confidence of a carve whose recovered chain names no confidence (dbrecover's
// unknownCapConfidence).
const carveUnknownCap = 25

// carveScanFunc and carveValidateBudget are the seams of the tests: the scanner and the validator read budget
// (0 = the default of the carve package).
var (
	carveScanFunc       = carve.Scan
	carveValidateBudget int64
	errCarveLimit       = errors.New("carve output limit reached")
)

// CarveHit is one accepted hit of a carve with what the case would do with it.
type CarveHit struct {
	carve.Hit
	Alloc      evidence.AllocSummary
	region     allocRegion // zero outside raw scope
	Skip       string      // "" (would write / written), below-min-confidence, type-not-selected, max-files, max-bytes
	Confidence int         // after the 3E-R10 caps
	Name       string      // relative path below the analysis directory
}

// CarvePlan is what a carve would do, built without a single write or audit entry.
type CarvePlan struct {
	Scope           string
	ScopeRuns       int
	ScopeBytes      int64
	Align           int64
	Hits            []CarveHit // at most CarveListCap
	HitsNotListed   int
	Stats           carve.Stats
	SkippedBy       map[string]int
	LimitReached    string
	NotScannedBytes int64
	Sigset          string
	SigsetSHA256    string
	SigFileSHA256   string
	Notes           []string
}

// carveJob is one resolved carve: the scope, the signature set and the per-hit rules. PlanCarve and Carve share
// it, so a list shows exactly what a run does.
type carveJob struct {
	s          *Session
	o          CarveOptions
	sc         *carveScope
	set        *carve.Set
	filter     map[string][]string
	validators map[string]string // type name -> validator id
	types      []string          // the selected names, sorted
	sigSHA     string
	setSHA     string
	notes      []string // scope and set notes (audited as non-skip warnings)
	limitNote  string   // the scan-limit note (code scan-limit)
	uncopied   int64    // bytes of accepted hits that failed to copy
	scanRuns   []carve.Run
	cutBytes   int64 // scope bytes past Session.ScanLimit
	byID       map[string]evidence.ManifestRecord

	b       *budget
	stats   carve.Stats
	limit   string
	skipped map[string]int
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// prepareCarve resolves the options, the scope and the signature set. It reads the partition table and the
// filesystems, never the carved bytes, and writes nothing.
func (s *Session) prepareCarve(o CarveOptions) (*carveJob, error) {
	sc, err := s.buildCarveScope(o)
	if err != nil {
		return nil, err
	}
	b, err := newBudget(o.MaxFiles, o.MaxBytes)
	if err != nil {
		return nil, err
	}
	j := &carveJob{s: s, o: o, sc: sc, b: b, skipped: map[string]int{}, notes: slices.Clone(sc.Notes)}

	full := carve.Builtin()
	if o.SigFile != nil {
		if full, err = carve.ParseSigFile(o.SigFile, full); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCarveOption, err)
		}
		j.sigSHA = sha256Hex(o.SigFile)
	}
	j.setSHA = full.Digest()
	sel, filter, err := full.Select(o.Types)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCarveOption, err)
	}
	if o.MaxFileSize > 0 { // lowers each type's MaxLen, never raises it
		kept := sel.Types[:0]
		noted := map[string]bool{} // the rows of a family share MinLen: one note per family
		for _, t := range sel.Types {
			t.MaxLen = min(t.MaxLen, o.MaxFileSize)
			if t.MaxLen < t.MinLen {
				if !noted[t.Name] {
					noted[t.Name] = true
					j.notes = append(j.notes, fmt.Sprintf("type %s is not carved: --max-file-size %d is below its minimum length %d", t.Name, o.MaxFileSize, t.MinLen))
				}
				continue
			}
			kept = append(kept, t)
		}
		sel.Types = kept
		if len(sel.Types) == 0 {
			return nil, carveOptionErr("max file size %d leaves no type to carve", o.MaxFileSize)
		}
	}
	j.set, j.filter = sel, filter
	j.validators = map[string]string{}
	for _, t := range sel.Types {
		j.validators[t.Name] = t.Validate
	}
	j.types = slices.Clone(o.Types)
	slices.Sort(j.types)
	j.types = slices.Compact(j.types)

	j.scanRuns = sc.Runs
	if limit, why := s.ScanLimit(); limit > 0 {
		var kept []carve.Run
		var used int64
		for _, r := range sc.Runs {
			if used >= limit {
				j.cutBytes += r.Length
				continue
			}
			if room := limit - used; r.Length > room {
				j.cutBytes += r.Length - room
				r.Length = room
			}
			used += r.Length
			kept = append(kept, r)
		}
		j.scanRuns = kept
		if j.cutBytes > 0 {
			j.limitNote = fmt.Sprintf("scan-limit: %d byte(s) of the scope lie past the scan limit of %d bytes (%s) and are not carved", j.cutBytes, limit, why)
		}
	}
	if sc.Scope == CarveArtifact {
		recs, err := s.Case.Manifest()
		if err != nil {
			return nil, err
		}
		j.byID = make(map[string]evidence.ManifestRecord, len(recs))
		for _, r := range recs {
			j.byID[r.ID] = r
		}
	}
	return j, nil
}

// typesParam is the "types" recovery parameter (or its digest form).
func (j *carveJob) typesParam() (key, val string) {
	if len(j.types) == 0 {
		return "types", "all"
	}
	list := strings.Join(j.types, ",")
	if len(list) > maxTypesParam {
		return "types_sha256", sha256Hex([]byte(list))
	}
	return "types", list
}

// carveTrailCap reads the recovery trail of an artifact exactly as dbrecover.trailCap does (its twin in
// internal/dbrecover/input.go, which this package does not import and plan 4G edits): recovered says the
// artifact or an ancestor is recovered; ceiling is the lowest confidence on the chain; unknown is true when the
// chain names no confidence at all or was cut by a missing parent, a cycle or the depth bound (a lower
// confidence may lie beyond the cut). A cut chain whose lowest confidence is above 25 is capped at 25; one at or
// below 25 keeps it.
func carveTrailCap(byID map[string]evidence.ManifestRecord, id string) (recovered bool, ceiling *int, unknown bool) {
	tr, found := evidence.RecoveryTrailOf(byID, id)
	if !found {
		return false, nil, false
	}
	ceiling = tr.MinConfidence
	unknown = tr.MinConfidence == nil || tr.Cut
	if tr.Cut && ceiling != nil && *ceiling > carveUnknownCap {
		v := carveUnknownCap
		ceiling = &v
	}
	return true, ceiling, unknown
}

// allocOf counts the bytes [off, off+n) of the scope by allocation state, with the region they sit in.
func (j *carveJob) allocOf(off, n int64) (evidence.AllocSummary, allocRegion, error) {
	switch j.sc.Scope {
	case CarveRaw:
		a, reg, ok := j.sc.Alloc.classify(evidence.Run{Offset: off, Length: n})
		if !ok && n > 0 {
			return a, reg, fmt.Errorf("internal error: the carved range %d+%d crosses a region of the allocation classifier", off, n)
		}
		return a, reg, nil
	case CarveArtifact:
		return evidence.AllocSummary{Unknown: n}, allocRegion{}, nil
	}
	return evidence.AllocSummary{Free: n}, allocRegion{}, nil
}

// decorate turns a scanner hit into the hit the case would write: allocation state, provenance lists and
// confidence caps. The skip reasons that need no budget are decided here.
func (j *carveJob) decorate(h carve.Hit) (CarveHit, error) {
	ch := CarveHit{Hit: h, Confidence: h.Confidence}
	var err error
	if ch.Alloc, ch.region, err = j.allocOf(h.Offset, h.Length); err != nil {
		return ch, err
	}
	validator := j.validators[h.Family]
	if validator == "" {
		validator = "none"
	}
	basis := []string{"sig=" + h.Family, "validator=" + validator}
	basis = append(basis, h.Basis...)
	ch.Basis = append(basis, "scope-run="+strconv.Itoa(h.Run))
	assumptions := append([]string{"contiguous-object"}, h.Assumptions...)
	if j.sc.Scope == CarveArtifact {
		if recovered, ceiling, unknown := carveTrailCap(j.byID, j.sc.ArtifactID); recovered {
			assumptions = append(assumptions, "parent-recovered="+j.sc.ArtifactID)
			if unknown {
				assumptions = append(assumptions, "recovery-cap-unknown")
				if ceiling == nil {
					v := carveUnknownCap
					ceiling = &v
				}
			}
			if ceiling != nil {
				ch.Confidence = min(ch.Confidence, *ceiling)
			}
		}
	}
	ch.Assumptions = assumptions
	switch {
	case h.Filtered:
		ch.Skip = carveSkipNotSel
	case ch.Confidence < j.o.MinConfidence:
		ch.Skip = carveSkipBelowMin
	}
	return ch, nil
}

// content is the Recovery.Content of a hit (3E-R14).
func (h *CarveHit) content() string {
	if h.Outcome == carve.Validated {
		return "type-match"
	}
	return "ok"
}

// scan runs the scanner over the scope and hands every accepted hit to onHit with its skip reason decided
// (budgets included). The first output-limit refusal is handed on too and then ends the run: the rest of the
// scope is not scanned, and that is reported, never silent. unreadable sees each newly found unreadable range.
func (j *carveJob) scan(ctx context.Context, onHit func(*CarveHit) error, unreadable func(off, n int64, err error) error) error {
	o := carve.Options{
		Align: j.sc.Align, Nested: j.o.Nested, TypeFilter: j.filter, ValidateBudget: carveValidateBudget,
		OnReadError: unreadable,
	}
	if p := j.o.Progress; p != nil {
		o.Progress = func(done int64) { p(done, j.sc.Bytes) }
	}
	stats, err := carveScanFunc(ctx, j.s.Image, j.scanRuns, j.set, o, func(h carve.Hit) error {
		ch, err := j.decorate(h)
		if err != nil {
			return err
		}
		if ch.Skip == "" {
			if reason := j.b.admit(h.Length); reason != "" {
				if reason == reasonInvalidSize {
					return fmt.Errorf("internal error: a carved hit of %d bytes", h.Length)
				}
				ch.Skip, j.limit = reason, reason
			}
		}
		if ch.Skip != "" {
			j.skipped[ch.Skip]++
		}
		if err := onHit(&ch); err != nil {
			return err
		}
		if j.limit != "" {
			return errCarveLimit
		}
		return nil
	})
	j.stats = stats
	if errors.Is(err, errCarveLimit) {
		return nil
	}
	return err
}

// finishLimits settles LimitReached after a scan that ended without error: an output limit wins over the
// validator budget, which wins over the scan limit.
func (j *carveJob) finishLimits() {
	switch {
	case j.limit != "":
	case j.stats.BudgetExhausted:
		j.limit = "validate-budget"
	case j.cutBytes > 0:
		j.limit = "scan-limit"
	}
}

// notScanned is the scope bytes the scanner did not cover and could not read.
func (j *carveJob) notScanned() int64 {
	return max(0, j.sc.Bytes-j.stats.Scanned-j.stats.UnreadableBytes)
}

func (j *carveJob) partitionOf() int {
	switch j.sc.Scope {
	case CarveUnallocated:
		return j.sc.Part.Index
	case CarveRaw:
		return j.o.Partition
	}
	return 0
}

// derivation returns the parent fields of a hit: the filesystem partition, the raw region, the image or the
// artifact.
func (j *carveJob) derivation(h *CarveHit) *evidence.Derivation {
	s := j.s
	switch j.sc.Scope {
	case CarveUnallocated:
		return s.baseDerivation(j.sc.Part, j.sc.FSType)
	case CarveRaw:
		return s.baseDerivation(volume.Partition{Index: h.region.Partition, Start: h.region.PartOffset}, h.region.FSType)
	}
	d := s.baseDerivation(volume.Partition{}, "")
	if j.sc.Scope == CarveArtifact {
		d.ParentSegments = nil
	}
	return d
}

// params are the recovery parameters of every hit of the run.
func (j *carveJob) params() map[string]string {
	k, v := j.typesParam()
	p := map[string]string{
		"sigset": j.set.ID, "sigset_sha256": j.setSHA, "min_confidence": strconv.Itoa(j.o.MinConfidence),
		"nested": strconv.FormatBool(j.o.Nested), "unaligned": strconv.FormatBool(j.o.Unaligned),
		"align": strconv.FormatInt(j.sc.Align, 10), k: v,
	}
	if j.sigSHA != "" {
		p["sig_file_sha256"] = j.sigSHA
	}
	if j.o.MaxFileSize > 0 {
		p["max_file_size"] = strconv.FormatInt(j.o.MaxFileSize, 10)
	}
	return p
}

// localName assigns the case-relative name of a hit: carved/<dir>/<offset>-<type>.<ext>.
func (j *carveJob) localName(lp *evidence.LocalPaths, h *CarveHit) (string, error) {
	dir, err := lp.Dir("/" + j.sc.Dir)
	if err != nil {
		return "", err
	}
	return lp.File(dir, fmt.Sprintf("%016x-%s.%s", h.Offset, h.Type, h.Ext))
}

// PlanCarve shows what Carve would do under the same options: the same scope, scan and budgets, with no audit
// entry and no write.
func (s *Session) PlanCarve(ctx context.Context, o CarveOptions) (*CarvePlan, error) {
	j, err := s.prepareCarve(o)
	if err != nil {
		return nil, err
	}
	p := &CarvePlan{
		Scope: j.sc.Scope, ScopeRuns: len(j.sc.Runs), ScopeBytes: j.sc.Bytes, Align: j.sc.Align,
		Sigset: j.set.ID, SigsetSHA256: j.setSHA, SigFileSHA256: j.sigSHA, Notes: slices.Clone(j.notes),
	}
	if j.limitNote != "" {
		p.Notes = append(p.Notes, j.limitNote)
	}
	lp := evidence.NewLocalPaths("carved")
	err = j.scan(ctx, func(h *CarveHit) error {
		if h.Skip == "" {
			name, nerr := j.localName(lp, h)
			if nerr != nil {
				h.Skip = carveSkipNoName
				j.b.release(h.Length)
				j.skipped[h.Skip]++
			}
			h.Name = name
		}
		if len(p.Hits) < CarveListCap {
			p.Hits = append(p.Hits, *h)
		} else {
			p.HitsNotListed++
		}
		return nil
	}, func(off, n int64, cause error) error {
		if len(p.Notes) < 100 {
			p.Notes = append(p.Notes, fmt.Sprintf("unreadable: %d byte(s) at offset %d (%v)", n, off, cause))
		}
		return nil
	})
	if err == nil {
		j.finishLimits()
	}
	p.Stats, p.SkippedBy, p.LimitReached, p.NotScannedBytes = j.stats, j.skipped, j.limit, j.notScanned()
	return p, err
}
