package artparse

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

const (
	maxNoteKey       = 64
	maxNoteValue     = 1 << 10
	maxReason        = 1 << 10
	progressInterval = 200 * time.Millisecond
	maxPayloadDepth  = 65      // the writer allows 64 levels; one more is refused here as well
	maxPayloadNodes  = 1 << 22 // values copied per record: a shared sub-tree cannot multiply the work
	relationFromRec  = "from-recovered-artifact"
)

var methodRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// errBadPayload wraps records.ErrInvalidRecord: a payload this host will not copy.
var errBadPayload = fmt.Errorf("%w: payload nests too deeply or holds too many values", records.ErrInvalidRecord)

// artInfo is what the emitter keeps of one artifact: copied once at newEmitter, never re-read from
// the parser's Input.
type artInfo struct {
	id       string
	platform string
	recovery *parse.RecoveryInfo
	snapshot *parse.SnapshotInfo
}

// emitter implements parse.Emitter for one invocation. It uses ONLY the host's context and the
// ingestWriter: the ctx argument of Emit and Warn is accepted and ignored (never called: a
// parser-defined Context whose Err or Done blocks cannot reach the host).
type emitter struct {
	w       ingestWriter
	hostCtx context.Context
	meta    parse.Meta // Emits and Platforms only; a private copy
	bundle  []parse.Artifact
	arts    map[string]artInfo
	lim     parse.Limits
	pump    chan<- func()
	report  func(done, total int64)

	sealed   atomic.Bool
	inflight atomic.Int64
	accepted atomic.Int64
	rejected atomic.Int64
	warnings atomic.Int64

	lastProgress atomic.Int64 // unix nanoseconds of the last forwarded progress

	notesMu sync.Mutex // a leaf: never held across a call
	noteMap map[string]string
}

func newEmitter(hostCtx context.Context, w ingestWriter, reg Registered, arts map[string]parse.Artifact, lim parse.Limits, pump chan<- func(), progress func(done, total int64)) (*emitter, error) {
	e := &emitter{
		w: w, hostCtx: hostCtx, lim: lim, pump: pump, report: progress,
		arts: make(map[string]artInfo, len(arts)), noteMap: map[string]string{},
	}
	m := reg.Meta()
	e.meta = parse.Meta{Name: m.Name, Platforms: m.Platforms, Emits: m.Emits}
	for _, a := range arts {
		info := artInfo{id: a.ID, platform: a.Platform}
		if a.Recovery != nil {
			ri := *a.Recovery
			if !methodRE.MatchString(ri.Method) {
				return nil, fmt.Errorf("artparse: artifact %s: recovery method %q is not [a-z][a-z0-9-]{1,31}", a.ID, ri.Method)
			}
			if ri.Confidence != nil {
				c := *ri.Confidence
				if c < 0 || c > 100 {
					return nil, fmt.Errorf("artparse: artifact %s: recovery confidence %d is outside 0..100", a.ID, c)
				}
				ri.Confidence = &c
			}
			info.recovery = &ri
		}
		if a.Source.Snapshot != nil {
			s := *a.Source.Snapshot
			info.snapshot = &s
		}
		e.arts[a.ID] = info
		e.bundle = append(e.bundle, parse.Artifact{ID: a.ID, Platform: a.Platform})
	}
	return e, nil
}

// begin enters a writer call: it refuses when sealed, and counts the call in flight so seal can wait
// for it. The sealed flag is re-checked after the increment, so a seal racing it wins.
func (e *emitter) begin() error {
	if e.sealed.Load() {
		return parse.ErrSealed
	}
	if err := e.hostCtx.Err(); err != nil {
		return err
	}
	e.inflight.Add(1)
	if e.sealed.Load() {
		e.inflight.Add(-1)
		return parse.ErrSealed
	}
	return nil
}

func (e *emitter) end() { e.inflight.Add(-1) }

// writerCtx is the context for the writer: the host's values, but not its cancellation, so a record
// that was accepted is audited even while the job is being cancelled.
func (e *emitter) writerCtx() context.Context { return context.WithoutCancel(e.hostCtx) }

func warnPath(r records.Record) string {
	switch {
	case r.Locator != "":
		return r.Locator
	case r.SourcePath != "":
		return r.SourcePath
	}
	return "artifact:" + r.ArtifactID
}

// reject audits and counts a rejection the emitter itself decided (the writer never saw the record).
func (e *emitter) reject(r records.Record, err error) error {
	if rerr := e.w.Reject(e.writerCtx(), cleanAuditText(warnPath(r), 4096), cleanAuditText(err.Error(), maxReason)); rerr != nil {
		return rerr
	}
	return e.countRejection()
}

// countRejection counts one rejection and returns ErrRejectedCap when the cap is reached.
func (e *emitter) countRejection() error {
	if n := e.rejected.Add(1); n >= int64(e.lim.MaxRejected) {
		return parse.ErrRejectedCap
	}
	return nil
}

// Emit hands one record to the writer. The ctx argument is ignored (see the type comment).
func (e *emitter) Emit(_ context.Context, r records.Record) error {
	if err := e.begin(); err != nil {
		return err
	}
	defer e.end()

	if err := parse.CheckRecord(e.meta, e.bundle, r); err != nil {
		return e.reject(r, err)
	}
	if e.accepted.Add(1) > e.lim.MaxRecords {
		e.accepted.Add(-1)
		return parse.ErrRecordCap
	}
	c, err := e.prepare(r)
	if err != nil {
		e.accepted.Add(-1)
		return e.reject(r, err)
	}
	if err := e.w.Add(e.writerCtx(), c); err != nil {
		e.accepted.Add(-1)
		if errors.Is(err, records.ErrInvalidRecord) {
			// the writer audited and counted this refusal itself: the emitter only counts it for the cap
			return e.countRejection()
		}
		return err
	}
	return nil
}

// prepare deep-copies r and applies what the host knows from the manifest: recovered-ness and the
// snapshot a record came from.
func (e *emitter) prepare(r records.Record) (records.Record, error) {
	c := r
	if r.Range != nil {
		v := *r.Range
		c.Range = &v
	}
	if r.Time != nil {
		v := *r.Time
		c.Time = &v
	}
	if r.TimeEnd != nil {
		v := *r.TimeEnd
		c.TimeEnd = &v
	}
	if r.Times != nil {
		c.Times = append([]records.NamedTime(nil), r.Times...)
	}
	if r.Confidence != nil {
		v := *r.Confidence
		c.Confidence = &v
	}
	nodes := 0
	if r.Payload != nil {
		p, err := copyValue(r.Payload, 0, &nodes)
		if err != nil {
			return records.Record{}, err
		}
		c.Payload, _ = p.(map[string]any)
	}
	info, ok := e.arts[r.ArtifactID]
	if !ok {
		return c, nil
	}
	if info.recovery != nil {
		c.Deleted = true
		c.Recovery = info.recovery.Method
		c.Confidence = lower(c.Confidence, info.recovery.Confidence)
		if c.Payload == nil {
			c.Payload = map[string]any{}
		}
		rec, _ := c.Payload["recovery"].(map[string]any)
		if rec == nil {
			rec = map[string]any{}
		}
		rec["artifact_method"] = info.recovery.Method
		if info.recovery.Confidence != nil {
			rec["artifact_confidence"] = *info.recovery.Confidence
		}
		if _, own := rec["relation"]; own {
			rec["artifact_relation"] = relationFromRec
		} else {
			rec["relation"] = relationFromRec
		}
		c.Payload["recovery"] = rec
	}
	if info.snapshot != nil {
		if c.Payload == nil {
			c.Payload = map[string]any{}
		}
		c.Payload["snapshot"] = map[string]any{"name": info.snapshot.Name, "xid": info.snapshot.Xid}
	}
	return c, nil
}

// lower returns a pointer to the lower of a and b; a nil one is ignored.
func lower(a, b *int) *int {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil || *a <= *b:
		v := *a
		return &v
	}
	v := *b
	return &v
}

// copyValue deep-copies the value types a payload may hold; any other value is passed on as it is
// (the writer refuses it). Depth and the number of values are bounded, so a cyclic or heavily shared
// structure is refused instead of exhausting the stack.
func copyValue(v any, depth int, nodes *int) (any, error) {
	if depth >= maxPayloadDepth {
		return nil, errBadPayload
	}
	if *nodes++; *nodes > maxPayloadNodes {
		return nil, errBadPayload
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			c, err := copyValue(val, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			out[k] = c
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			c, err := copyValue(val, depth+1, nodes)
			if err != nil {
				return nil, err
			}
			out[i] = c
		}
		return out, nil
	}
	return v, nil
}

// Warn reports something the parser could not extract. The ctx argument is ignored.
func (e *emitter) Warn(_ context.Context, locator, reason string) error {
	if err := e.begin(); err != nil {
		return err
	}
	defer e.end()
	if err := e.w.Warn(e.writerCtx(), cleanAuditText(locator, 4096), cleanAuditText(reason, maxReason)); err != nil {
		return err
	}
	e.warnings.Add(1)
	return nil
}

// Note keeps a bounded key/value fact about the job: at most MaxNotes entries, the first write of a
// key wins.
func (e *emitter) Note(key, value string) {
	if e.sealed.Load() {
		return
	}
	key, value = cleanAuditText(key, maxNoteKey), cleanAuditText(value, maxNoteValue)
	e.notesMu.Lock()
	defer e.notesMu.Unlock()
	if _, dup := e.noteMap[key]; dup || len(e.noteMap) >= e.lim.MaxNotes {
		return
	}
	e.noteMap[key] = value
}

// Progress forwards at most every 200 ms, and always the final done == total, by queueing a closure
// on the pump without blocking (dropped when the pump is full or the emitter is sealed).
func (e *emitter) Progress(done, total int64) {
	if e.sealed.Load() || e.pump == nil || e.report == nil {
		return
	}
	now := time.Now().UnixNano()
	last := e.lastProgress.Load()
	final := total > 0 && done >= total
	if !final && last != 0 && now-last < int64(progressInterval) {
		return
	}
	select {
	case e.pump <- func() { e.report(done, total) }:
		e.lastProgress.Store(now)
	default:
	}
}

// seal refuses every later call, then waits until the writer calls already in flight have returned.
// It takes no lock a parser call can hold.
func (e *emitter) seal() {
	e.sealed.Store(true)
	for e.inflight.Load() > 0 {
		time.Sleep(time.Millisecond)
	}
}

// counts reports the emitter's own counters; they serve only the MaxRecords and MaxRejected caps (the
// writer's counts are authoritative for the audit).
func (e *emitter) counts() (accepted, rejected, warnings int) {
	return int(e.accepted.Load()), int(e.rejected.Load()), int(e.warnings.Load())
}

// notes returns a copy of the notes.
func (e *emitter) notes() map[string]string {
	e.notesMu.Lock()
	defer e.notesMu.Unlock()
	out := make(map[string]string, len(e.noteMap))
	for k, v := range e.noteMap {
		out[k] = v
	}
	return out
}
