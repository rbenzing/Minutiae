// Package call defines the payload of the "call" record type, version 1: the
// builder parsers use to produce it (Call.Payload), the validator the records
// writer runs on every call (Validate, installed at init), and the typed reader
// for stored payloads (Decode). It is a pure package: it imports only
// internal/records (SetValidator, in init) and internal/recordtypes/common.
//
// The payload follows the contract of the plan's F10 table: required direction
// and outcome; everything else optional and absent (never an empty string, a
// zero or false that means "not known") when the parser does not know it (C3).
// The call's instants live in Record.Time/TimeEnd, not in the payload. Unknown
// fields in a stored payload are ignored by Decode; the provenance objects
// recovery, snapshot and deleted are never ignored.
//
// A parser that has to sanitise text the contract refuses (invalid UTF-8, NUL)
// keeps the original bytes in raw (base64): the payload stores only what the
// writer accepts, and the typed value does not flag the replacement.
package call

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// Type is the record type name and PayloadVersion the payload version this
// package writes and reads.
const (
	Type           = "call"
	PayloadVersion = 1
)

// Directions.
const (
	DirIn      = "in"
	DirOut     = "out"
	DirUnknown = "unknown"
)

// Outcomes.
const (
	OutcomeAnswered  = "answered"
	OutcomeMissed    = "missed"
	OutcomeRejected  = "rejected"
	OutcomeBlocked   = "blocked"
	OutcomeVoicemail = "voicemail"
	OutcomeCancelled = "cancelled"
	OutcomeUnknown   = "unknown"
)

// How the caller's number was presented.
const (
	PresentationAllowed    = "allowed"
	PresentationRestricted = "restricted"
	PresentationUnknown    = "unknown"
	PresentationPayphone   = "payphone"
)

// Kinds of call.
const (
	KindVoice         = "voice"
	KindVideo         = "video"
	KindFaceTimeAudio = "facetime_audio"
	KindFaceTimeVideo = "facetime_video"
	KindUnknown       = "unknown"
)

// ErrUnsupportedPayloadVersion is returned by Decode for a payload version this
// package does not read.
var ErrUnsupportedPayloadVersion = errors.New("call: unsupported payload version")

// Call is the typed form of a call payload. A zero value (empty string or nil
// pointer) means "not known" and is absent from the payload. Address is the
// source string, never normalized (C5); NameCached is the name the source cached
// with the call, not a contact lookup. DurationS is seconds as stored.
// Deleted, Recovery and Snapshot are provenance: a typed reader must not present
// a call that carries them as a live one.
// DurationS is a float64 view; the stored payload keeps the exact number.
type Call struct {
	Direction, Outcome, Address, AddressPresentation, NameCached, Kind, Service, CountryISO, GeoDescription string
	DurationS                                                                                               *float64
	Subscription                                                                                            *int64
	Read, New                                                                                               *bool
	Deleted, Recovery, Snapshot                                                                             map[string]any
	Raw                                                                                                     map[string]any
}

func put(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

// Payload returns the payload map in the canonical value types of the records
// package (string, bool, int64, float64, []any, map[string]any). Empty strings and
// nil pointers are omitted; a *bool, *int64 or *float64 that is set is written even
// when false or 0. The free-form objects are deep-copied, so the payload never
// aliases the Call.
func (c Call) Payload() map[string]any {
	p := map[string]any{}
	put(p, "direction", c.Direction)
	put(p, "outcome", c.Outcome)
	put(p, "address", c.Address)
	put(p, "address_presentation", c.AddressPresentation)
	put(p, "name_cached", c.NameCached)
	if c.DurationS != nil {
		p["duration_s"] = *c.DurationS
	}
	put(p, "kind", c.Kind)
	put(p, "service", c.Service)
	put(p, "country_iso", c.CountryISO)
	put(p, "geo_description", c.GeoDescription)
	if c.Subscription != nil {
		p["subscription"] = *c.Subscription
	}
	if c.Read != nil {
		p["read"] = *c.Read
	}
	if c.New != nil {
		p["new"] = *c.New
	}
	if c.Deleted != nil {
		p["deleted"] = common.CopyMap(c.Deleted)
	}
	if c.Recovery != nil {
		p["recovery"] = common.CopyMap(c.Recovery)
	}
	if c.Snapshot != nil {
		p["snapshot"] = common.CopyMap(c.Snapshot)
	}
	if c.Raw != nil {
		p["raw"] = common.CopyMap(c.Raw)
	}
	return p
}

// The payload contract. The schema is an unexported package variable used only as
// the receiver of Validate (the purity rules accept nothing else).
var schema = common.Schema{
	Fields: append([]common.Field{
		{Name: "direction", Kind: common.KEnum, Required: true, Enum: []string{"in", "out", "unknown"}},
		{Name: "outcome", Kind: common.KEnum, Required: true, Enum: []string{"answered", "missed", "rejected", "blocked", "voicemail", "cancelled", "unknown"}},
		{Name: "address", Kind: common.KString, NonEmpty: true},
		{Name: "address_presentation", Kind: common.KEnum, Enum: []string{"allowed", "restricted", "unknown", "payphone"}},
		{Name: "name_cached", Kind: common.KString, NonEmpty: true},
		{Name: "duration_s", Kind: common.KNumber, Min: new(int64)},
		{Name: "kind", Kind: common.KEnum, Enum: []string{"voice", "video", "facetime_audio", "facetime_video", "unknown"}},
		{Name: "service", Kind: common.KString, NonEmpty: true},
		{Name: "country_iso", Kind: common.KString, NonEmpty: true},
		{Name: "geo_description", Kind: common.KString, NonEmpty: true},
		{Name: "subscription", Kind: common.KInt},
		{Name: "read", Kind: common.KBool},
		{Name: "new", Kind: common.KBool},
	}, common.CommonFields()...),
}

// Validate checks a call payload against the v1 contract. Its errors name field
// paths and never a payload value; unknown fields are allowed.
func Validate(payload map[string]any) error { return schema.Validate(payload) }

func init() { records.SetValidator(Type, Validate) }

// Summary builds the record summary "call <direction> <outcome> <address>", one
// line, through common.Summarize (format and bidi characters are shown as
// <U+XXXX>). The address is absent for a hidden number. The parts are bounded
// separately (16, 16 and 64 bytes).
func Summary(direction, outcome, address string) string {
	parts := []string{"call"}
	for _, p := range []string{common.Summarize(direction, 16), common.Summarize(outcome, 16), common.Summarize(address, 64)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// Decode reads a stored call payload of the given payload version. Only version 1
// exists; any other value is ErrUnsupportedPayloadVersion. Numbers are read
// exactly (json.Number): integers are canonical JSON integers (the validator
// refuses 1e3 and 2.0), durations become float64, and the numbers inside the
// free-form objects stay json.Number (exact, never rounded), so the result equals what Payload
// produced. Unknown fields are ignored. A payload that does not satisfy the v1
// contract is an error naming the path, never a value.
func Decode(payloadV int, payload []byte) (Call, error) {
	if payloadV != PayloadVersion {
		return Call{}, fmt.Errorf("%w: %d", ErrUnsupportedPayloadVersion, payloadV)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Call{}, errors.New("call: the payload is not a JSON object")
	}
	if len(bytes.TrimSpace(payload[dec.InputOffset():])) != 0 {
		return Call{}, errors.New("call: the payload holds more than one JSON value")
	}
	if m == nil {
		return Call{}, errors.New("call: the payload is not a JSON object")
	}
	if err := Validate(m); err != nil {
		return Call{}, fmt.Errorf("call: %w", err)
	}
	return fromMap(m), nil
}

// fromMap extracts a validated payload. Every access is by comma-ok type
// assertion, so a value of an unexpected type is skipped rather than trusted.
func fromMap(m map[string]any) Call {
	var out Call
	out.Direction = str(m, "direction")
	out.Outcome = str(m, "outcome")
	out.Address = str(m, "address")
	out.AddressPresentation = str(m, "address_presentation")
	out.NameCached = str(m, "name_cached")
	out.DurationS = floatPtr(m, "duration_s")
	out.Kind = str(m, "kind")
	out.Service = str(m, "service")
	out.CountryISO = str(m, "country_iso")
	out.GeoDescription = str(m, "geo_description")
	out.Subscription = intPtr(m, "subscription")
	if b, ok := m["read"].(bool); ok {
		out.Read = &b
	}
	if b, ok := m["new"].(bool); ok {
		out.New = &b
	}
	if d, ok := m["deleted"].(map[string]any); ok {
		out.Deleted = common.NormMap(d)
	}
	if r, ok := m["recovery"].(map[string]any); ok {
		out.Recovery = common.NormMap(r)
	}
	if s, ok := m["snapshot"].(map[string]any); ok {
		out.Snapshot = common.NormMap(s)
	}
	if r, ok := m["raw"].(map[string]any); ok {
		out.Raw = common.NormMap(r)
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func intPtr(m map[string]any, key string) *int64 {
	if n, ok := m[key].(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return &i
		}
	}
	return nil
}

func floatPtr(m map[string]any, key string) *float64 {
	if n, ok := m[key].(json.Number); ok {
		if f, err := n.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return &f
		}
	}
	return nil
}

// Live reports whether the call is a plain live one: it carries no recovery,
// snapshot or deleted provenance (see common.IsLive).
func (c Call) Live() bool { return common.IsLive(c.Recovery, c.Snapshot, c.Deleted) }
