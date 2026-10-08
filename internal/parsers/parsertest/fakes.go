package parsertest

import (
	"context"
	"io"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

// FakeMeta is the Meta every fake parser declares: platform android, primary
// role "primary" with the glob android:**/parsertest/<name>.dat and the optional
// companion "-wal", emitting message v1.
func FakeMeta(name, version string) parse.Meta {
	return parse.Meta{
		Name: name, Version: version, Title: name + " (test parser)",
		Platforms: []string{parse.PlatformAndroid},
		Emits:     []parse.Emit{{Type: message.Type, PayloadVersion: message.PayloadVersion}},
		Inputs: []parse.InputSpec{{
			Role: "primary", Globs: []string{"android:**/parsertest/" + name + ".dat"},
			Companions: []string{"-wal"}, Required: true,
		}},
	}
}

// probeRead is what the fakes' Probe does: a small read of the primary.
func probeApplicable(in *parse.Input) (parse.Applicability, error) {
	var b [16]byte
	if _, err := in.Primary.R.ReadAt(b[:], 0); err != nil && err != io.EOF {
		return parse.Applicability{}, err
	}
	return parse.Applicability{Status: parse.Applicable}, nil
}

// messageRecord is a valid message record for artifact id. line is 1-based and
// puts "text:line=N" in the locator; off and length are the byte range of text.
func messageRecord(artifactID string, line int, text string, off, length int64) records.Record {
	m := message.Message{
		Channel: message.ChannelSMS, Direction: message.DirUnknown, Kind: message.KindText,
		ParticipantsUnknown: true,
	}
	r := records.Record{
		Type: message.Type, ArtifactID: artifactID,
		Locator: "text:line=" + strconv.Itoa(line),
		Summary: message.Summary(message.ChannelSMS, message.DirUnknown, "", text),
		Body:    text,
		Payload: m.Payload(),
	}
	if length > 0 {
		r.Range = &records.Range{Offset: off, Length: length}
	}
	return r
}

// validRecord is a valid record of the primary artifact, for the fakes that do
// not care about content.
func validRecord(in *parse.Input, n int) records.Record {
	return messageRecord(in.Primary.ID, n, "record "+strconv.Itoa(n), 0, 0)
}

// WellBehaved emits one message per line of the primary (a valid record with a
// byte range and the locator text:line=N), reports a note and progress, and
// warns once per blank line. Lines, when above 0, caps the lines it processes.
type WellBehaved struct {
	Name, Version string
	Lines         int
}

// Meta implements parse.Parser.
func (p WellBehaved) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p WellBehaved) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p WellBehaved) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	size := in.Primary.Size
	if err := in.Budget.Alloc(size); err != nil {
		return err
	}
	defer in.Budget.Free(size)
	data := make([]byte, size)
	if _, err := io.ReadFull(io.NewSectionReader(in.Primary.R, 0, size), data); err != nil {
		return err
	}
	var lines []string
	for rest := string(data); rest != ""; {
		line, after, _ := strings.Cut(rest, "\n")
		lines = append(lines, line)
		rest = after
	}
	if p.Lines > 0 && len(lines) > p.Lines {
		lines = lines[:p.Lines]
	}
	total := int64(len(lines))
	out.Note("lines", strconv.Itoa(len(lines)))
	off := int64(0)
	for i, line := range lines {
		if err := parse.Tick(ctx, i); err != nil {
			return err
		}
		n := i + 1
		if strings.TrimSpace(line) == "" {
			if err := out.Warn(ctx, "text:line="+strconv.Itoa(n), "blank line"); err != nil {
				return err
			}
		} else if err := out.Emit(ctx, messageRecord(in.Primary.ID, n, line, off, int64(len(line)))); err != nil {
			return err
		}
		off += int64(len(line)) + 1
		out.Progress(int64(n), total)
	}
	return nil
}

// PanicWhere says where a Panicking parser panics.
type PanicWhere int

// Where a Panicking parser panics.
const (
	PanicInProbe PanicWhere = iota
	PanicInParse
	PanicAfterEmits
)

// Panicking panics with Value in Probe, at the start of Parse, or in Parse
// after it emitted After valid records. Value can be a string, an error, a
// runtime error (RuntimePanicValue) or a value whose Error method panics
// (ErrorPanics).
type Panicking struct {
	Name, Version string
	Where         PanicWhere
	Value         any
	After         int
}

// Meta implements parse.Parser.
func (p Panicking) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Panicking) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	if p.Where == PanicInProbe {
		panic(p.Value)
	}
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Panicking) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	switch p.Where {
	case PanicInParse:
		panic(p.Value)
	case PanicAfterEmits:
		for i := 1; i <= p.After; i++ {
			if err := out.Emit(ctx, validRecord(in, i)); err != nil {
				return err
			}
		}
		panic(p.Value)
	}
	return nil
}

// RuntimePanicValue returns the value of a real runtime error: the panic of a
// write to a nil map.
func RuntimePanicValue() (v any) {
	defer func() { v = recover() }()
	var m map[string]int
	m["x"] = 1 //nolint:staticcheck // the write to a nil map is the point: it panics with a real runtime error
	return nil
}

// ErrorPanics is an error whose Error method panics, so code that formats a
// recovered panic value can itself panic.
type ErrorPanics struct{}

// Error panics.
func (ErrorPanics) Error() string { panic("parsertest: Error method panics") }

// InvalidMode says what is wrong with the records an Invalid parser emits.
type InvalidMode int

// What is wrong with the records an Invalid parser emits.
const (
	InvalidNulInSummary InvalidMode = iota
	InvalidMissingRequiredField
	InvalidUnknownType
	InvalidOversizeBody
	InvalidBadLocator
)

// Invalid emits N records the writer refuses for the reason Mode names, and
// stops at the first emit error.
type Invalid struct {
	Name, Version string
	Mode          InvalidMode
	N             int
}

// Meta implements parse.Parser.
func (p Invalid) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Invalid) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Invalid) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	var body string
	if p.Mode == InvalidOversizeBody {
		body = strings.Repeat("x", 4<<20+1)
	}
	for i := 1; i <= p.N; i++ {
		r := validRecord(in, i)
		switch p.Mode {
		case InvalidNulInSummary:
			r.Summary = "a\x00b"
		case InvalidMissingRequiredField:
			r.Payload = map[string]any{"direction": message.DirIn}
		case InvalidUnknownType:
			r.Type = "nonesuch_type"
		case InvalidOversizeBody:
			r.Body = body
		case InvalidBadLocator:
			r.Locator = "BAD LOCATOR"
		}
		if err := out.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// LieMode says how a Liar lies.
type LieMode int

// How a Liar lies.
const (
	LieOutsideBundle LieMode = iota
	LieUndeclaredType
	LieRangeBeyondArtifact
	LieForeignPlatform
)

// Liar emits one record that its declaration or its bundle does not allow:
// for the artifact Other, which is not in the bundle (OutsideBundle); of type
// call, which its Meta does not declare (UndeclaredType); with a byte range
// beyond the artifact (RangeBeyondArtifact); or, declared as an iOS parser
// (Meta), a valid record for the Android artifact it was given (ForeignPlatform).
type Liar struct {
	Name, Version string
	Mode          LieMode
	Other         string
}

// Meta implements parse.Parser.
func (p Liar) Meta() parse.Meta {
	m := FakeMeta(p.Name, p.Version)
	if p.Mode == LieForeignPlatform {
		m.Platforms = []string{parse.PlatformIOS}
		m.Inputs[0].Globs = []string{"ios:**/parsertest/" + p.Name + ".dat"}
	}
	return m
}

// Probe implements parse.Parser.
func (p Liar) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Liar) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	r := validRecord(in, 1)
	switch p.Mode {
	case LieOutsideBundle:
		r.ArtifactID = p.Other
	case LieUndeclaredType:
		r.Type = call.Type
		r.Payload = map[string]any{"direction": "unknown", "outcome": "unknown"}
	case LieRangeBeyondArtifact:
		r.Range = &records.Range{Offset: 0, Length: in.Primary.Size + 1}
	}
	return out.Emit(ctx, r)
}

// Slow does not finish by itself. Cooperative: Parse returns ctx.Err() as soon
// as the context ends. Otherwise Parse ignores the context and blocks on
// Release; after it is released it tries Emit and a read of the primary, stores
// the errors it got in *Late, and only then closes Done (so a test that reads
// Late after <-Done is ordered by the channel close).
type Slow struct {
	Name, Version string
	Cooperative   bool
	Release, Done chan struct{}
	Late          *LateResults
}

// LateResults holds what a non-cooperative Slow parser got after it was released.
type LateResults struct{ EmitErr, ReadErr error }

// Meta implements parse.Parser.
func (p Slow) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Slow) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Slow) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	if p.Cooperative {
		<-ctx.Done()
		return ctx.Err()
	}
	<-p.Release
	emitErr := out.Emit(ctx, validRecord(in, 1))
	var b [1]byte
	_, readErr := in.Primary.R.ReadAt(b[:], 0)
	if readErr == io.EOF {
		readErr = nil
	}
	if p.Late != nil {
		p.Late.EmitErr, p.Late.ReadErr = emitErr, readErr
	}
	if p.Done != nil {
		close(p.Done)
	}
	return nil
}

// blockedCtx is a context whose Done and Err block forever: a parser-defined
// context a host must never wait on.
type blockedCtx struct{ block chan struct{} }

func (blockedCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c blockedCtx) Done() <-chan struct{} {
	<-c.block
	return nil
}

func (c blockedCtx) Err() error {
	<-c.block
	return nil
}
func (blockedCtx) Value(any) any { return nil }

// BlockingCtxParser calls Emit and Warn with a parser-defined context whose Err
// and Done block forever (its Block channel is never closed). It emits one valid
// record and one warning.
type BlockingCtxParser struct{ Name, Version string }

// Meta implements parse.Parser.
func (p BlockingCtxParser) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p BlockingCtxParser) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p BlockingCtxParser) Parse(_ context.Context, in *parse.Input, out parse.Emitter) error {
	ctx := blockedCtx{block: make(chan struct{})}
	if err := out.Emit(ctx, validRecord(in, 1)); err != nil {
		return err
	}
	return out.Warn(ctx, "text:line=1", "warned through a blocking context")
}

// MutateMode says what a Mutator changes.
type MutateMode int

// What a Mutator changes.
const (
	// MutateInput clears Recovery of its artifacts, rewrites Source and Snapshot and edits Primary.SHA256.
	MutateInput MutateMode = iota
	// MutateMeta changes the Meta its first Meta call returned (Emits, Platforms, a glob).
	MutateMeta
	// MutateBudget allocates a little and frees far more than that.
	MutateBudget
	// MutatePayloadAfterEmit changes a payload it already emitted, through slices and maps it kept.
	MutatePayloadAfterEmit
)

// Mutator mutates what it was given, for the host's tests of isolation: its
// Input copy, the Meta it returned, the BudgetView and a payload after Emit.
type Mutator struct {
	Name, Version string
	Mode          MutateMode
	last          *parse.Meta
}

// Meta implements parse.Parser. The first Meta returned is remembered, so
// MutateMeta can change what the caller of that first call holds.
func (p *Mutator) Meta() parse.Meta {
	m := FakeMeta(p.Name, p.Version)
	if p.last == nil {
		c := m
		p.last = &c
	}
	return m
}

// Probe implements parse.Parser.
func (p *Mutator) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p *Mutator) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	switch p.Mode {
	case MutateInput:
		in.Primary.SHA256 = "mutated"
		in.Primary.Recovery = nil
		if sn := in.Primary.Source.Snapshot; sn != nil {
			sn.Xid = 99
		}
		in.Primary.Source = parse.SourceInfo{Kind: "mutated"}
		for role, a := range in.Artifacts {
			if a.Recovery != nil {
				a.Recovery.Class = "mutated"
			}
			if a.Source.Snapshot != nil {
				a.Source.Snapshot.Name, a.Source.Snapshot.Xid = "mutated", 99
			}
			a.Recovery = nil
			a.Source.Kind = "mutated"
			a.SHA256 = "mutated"
			in.Artifacts[role] = a
		}
	case MutateMeta:
		if p.last != nil {
			p.last.Emits[0].Type = "call" // in place: an append would reallocate and never reach the Meta the host holds
			p.last.Platforms[0] = parse.PlatformIOS
			p.last.Inputs[0].Globs[0] = "android:**/mutated.dat"
		}
	case MutateBudget:
		if err := in.Budget.Alloc(10); err != nil {
			return err
		}
		in.Budget.Free(1 << 40)
	case MutatePayloadAfterEmit:
		r := validRecord(in, 1)
		raw := map[string]any{"k": "orig"}
		r.Payload["raw"] = raw
		if err := out.Emit(ctx, r); err != nil {
			return err
		}
		raw["k"] = "mutated"
		r.Payload["kind"] = "mutated"
	}
	return nil
}

// Hog allocates Alloc bytes of the budget and returns the error, if any.
type Hog struct {
	Name, Version string
	Alloc         int64
}

// Meta implements parse.Parser.
func (p Hog) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Hog) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Hog) Parse(_ context.Context, in *parse.Input, _ parse.Emitter) error {
	return in.Budget.Alloc(p.Alloc)
}

// Spammer emits N valid records.
type Spammer struct {
	Name, Version string
	N             int
}

// Meta implements parse.Parser.
func (p Spammer) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Spammer) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Spammer) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	for i := 1; i <= p.N; i++ {
		if err := parse.Tick(ctx, i); err != nil {
			return err
		}
		if err := out.Emit(ctx, validRecord(in, i)); err != nil {
			return err
		}
	}
	return nil
}

// ProbeOnly answers Probe with Status, Reason and Err; Parse emits nothing.
type ProbeOnly struct {
	Name, Version, Status, Reason string
	Err                           error
}

// Meta implements parse.Parser.
func (p ProbeOnly) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p ProbeOnly) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: p.Status, Reason: p.Reason}, p.Err
}

// Parse implements parse.Parser.
func (p ProbeOnly) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// Nondeterministic puts math/rand output in its payload (only parsertest may:
// the purity rule bans it elsewhere).
type Nondeterministic struct{ Name, Version string }

// Meta implements parse.Parser.
func (p Nondeterministic) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p Nondeterministic) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p Nondeterministic) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	r := validRecord(in, 1)
	r.Payload["raw"] = map[string]any{"r": strconv.FormatUint(rand.Uint64(), 10)} //nolint:gosec // nondeterminism is the point of this fake
	return out.Emit(ctx, r)
}

// MapOrder emits one record per key of a map in iteration order, the mistake the repeated runs of
// AssertDeterministic exist to catch: Go randomises map iteration, so two runs disagree.
type MapOrder struct{ Name, Version string }

// Meta implements parse.Parser.
func (p MapOrder) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p MapOrder) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p MapOrder) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	for name, i := range map[string]int{"alpha": 1, "beta": 2} {
		r := validRecord(in, i)
		r.Summary = name
		if err := out.Emit(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

// Stateful keeps a counter in the parser value between jobs: its output depends on how many jobs
// ran before, which the registry (one instance per parser) would make real. The purity test refuses
// such a type in a real parser package; this fake proves AssertDeterministic catches it when one
// instance serves every run.
type Stateful struct {
	Name, Version string
	calls         int
}

// Meta implements parse.Parser.
func (p *Stateful) Meta() parse.Meta { return FakeMeta(p.Name, p.Version) }

// Probe implements parse.Parser.
func (p *Stateful) Probe(_ context.Context, in *parse.Input) (parse.Applicability, error) {
	return probeApplicable(in)
}

// Parse implements parse.Parser.
func (p *Stateful) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	p.calls++
	r := validRecord(in, 1)
	r.Summary = strings.Repeat("s", p.calls)
	return out.Emit(ctx, r)
}
