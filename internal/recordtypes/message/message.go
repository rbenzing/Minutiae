// Package message defines the payload of the "message" record type, version 1:
// the builder parsers use to produce it (Message.Payload), the validator the
// records writer runs on every message (Validate, installed at init), and the
// typed reader for stored payloads (Decode). It is a pure package: it imports
// only internal/records (SetValidator, in init) and internal/recordtypes/common.
//
// The payload follows the contract of the plan's F10 table: required channel,
// direction, kind and participants; everything else optional and absent (never
// an empty string, a zero or false that means "not known") when the parser does
// not know it (C3). Unknown fields in a stored payload are ignored by Decode.
package message

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// Type is the record type name and PayloadVersion the payload version this
// package writes and reads.
const (
	Type           = "message"
	PayloadVersion = 1
)

// Channels of the core parsers. A channel is any token [a-z][a-z0-9_]{0,31}: an
// app parser adds its own without editing this package.
const (
	ChannelSMS      = "sms"
	ChannelMMS      = "mms"
	ChannelIMessage = "imessage"
)

// Directions.
const (
	DirIn      = "in"
	DirOut     = "out"
	DirUnknown = "unknown"
)

// Kinds of message.
const (
	KindText     = "text"
	KindMedia    = "media"
	KindReaction = "reaction"
	KindSystem   = "system"
	KindUnknown  = "unknown"
)

// Roles of a participant.
const (
	RoleFrom   = "from"
	RoleTo     = "to"
	RoleCC     = "cc"
	RoleBCC    = "bcc"
	RoleMember = "member"
)

// Delivery statuses.
const (
	StatusReceived  = "received"
	StatusSent      = "sent"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
	StatusQueued    = "queued"
	StatusDraft     = "draft"
	StatusUnknown   = "unknown"
)

// Where the body text came from.
const (
	TextSourceText           = "text"
	TextSourceAttributedBody = "attributedBody"
	TextSourcePart           = "part"
	TextSourceSummary        = "summary"
)

// ErrUnsupportedPayloadVersion is returned by Decode for a payload version this
// package does not read.
var ErrUnsupportedPayloadVersion = errors.New("message: unsupported payload version")

// Participant is one party of a message. Kind is the shape of Address
// (common.AddressKind), never an identity.
type Participant struct {
	Role, Address, Name, Kind string
}

// Thread is the conversation a message belongs to.
type Thread struct {
	ID, Title string
	Group     *bool
	Members   []string
}

// Attachment describes one attachment; ArtifactID and ArtifactSHA256 name the
// artifact that holds its bytes when the parser found it.
type Attachment struct {
	Name, Mime, SourceRef, ArtifactID, ArtifactSHA256 string
	Size                                              *int64
}

// Message is the typed form of a message payload. A zero value (empty string,
// nil pointer or slice) means "not known" and is absent from the payload.
type Message struct {
	Channel, Direction, Kind                               string
	Participants                                           []Participant
	ParticipantsUnknown                                    bool
	Thread                                                 *Thread
	Status, Subject, GUID, TargetGUID, Service, TextSource string
	Read                                                   *bool
	Subscription                                           *int64
	Attachments                                            []Attachment
	BodyFlags                                              []string
	BodyTruncated                                          *bool
	BodyTotalBytes                                         *int64
	BodyRawB64, BodyRawSHA256                              string
	BodyRawLen                                             *int64
	Deleted                                                map[string]any // {"source": ...}
	Raw                                                    map[string]any
	Recovery, Snapshot                                     map[string]any // provenance: a recovered or snapshot-derived message is never presented as live
}

func put(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

func putBool(m map[string]any, key string, v *bool) {
	if v != nil {
		m[key] = *v
	}
}

func putInt(m map[string]any, key string, v *int64) {
	if v != nil {
		m[key] = *v
	}
}

func strings2any(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Payload returns the payload map in the canonical value types of the records
// package (string, bool, int64, []any, map[string]any). Empty strings, nil
// pointers, empty lists and a false ParticipantsUnknown are omitted; a *bool or
// *int64 that is set is written even when false or zero. participants is always
// present (an empty list when the parties are unknown).
func (m Message) Payload() map[string]any {
	p := map[string]any{}
	put(p, "channel", m.Channel)
	put(p, "direction", m.Direction)
	put(p, "kind", m.Kind)
	parts := make([]any, len(m.Participants))
	for i, pt := range m.Participants {
		e := map[string]any{}
		put(e, "role", pt.Role)
		put(e, "address", pt.Address)
		put(e, "name", pt.Name)
		put(e, "kind", pt.Kind)
		parts[i] = e
	}
	p["participants"] = parts
	if m.ParticipantsUnknown {
		p["participants_unknown"] = true
	}
	if t := m.Thread; t != nil {
		e := map[string]any{}
		put(e, "id", t.ID)
		put(e, "title", t.Title)
		putBool(e, "group", t.Group)
		if len(t.Members) > 0 {
			e["members"] = strings2any(t.Members)
		}
		p["thread"] = e
	}
	put(p, "status", m.Status)
	putBool(p, "read", m.Read)
	put(p, "subject", m.Subject)
	if len(m.Attachments) > 0 {
		atts := make([]any, len(m.Attachments))
		for i, a := range m.Attachments {
			e := map[string]any{}
			put(e, "name", a.Name)
			put(e, "mime", a.Mime)
			putInt(e, "size", a.Size)
			put(e, "source_ref", a.SourceRef)
			put(e, "artifact_id", a.ArtifactID)
			put(e, "artifact_sha256", a.ArtifactSHA256)
			atts[i] = e
		}
		p["attachments"] = atts
	}
	put(p, "guid", m.GUID)
	put(p, "target_guid", m.TargetGUID)
	put(p, "service", m.Service)
	putInt(p, "subscription", m.Subscription)
	put(p, "text_source", m.TextSource)
	if len(m.BodyFlags) > 0 {
		p["body_flags"] = strings2any(m.BodyFlags)
	}
	putBool(p, "body_truncated", m.BodyTruncated)
	putInt(p, "body_total_bytes", m.BodyTotalBytes)
	put(p, "body_raw_b64", m.BodyRawB64)
	put(p, "body_raw_sha256", m.BodyRawSHA256)
	putInt(p, "body_raw_len", m.BodyRawLen)
	if m.Deleted != nil {
		p["deleted"] = common.CopyMap(m.Deleted)
	}
	if m.Recovery != nil {
		p["recovery"] = common.CopyMap(m.Recovery)
	}
	if m.Snapshot != nil {
		p["snapshot"] = common.CopyMap(m.Snapshot)
	}
	if m.Raw != nil {
		p["raw"] = common.CopyMap(m.Raw)
	}
	return p
}

// The payload contract. The schema is an unexported package variable used only as
// the receiver of Validate (the purity rules accept nothing else).
var schema = common.Schema{
	Fields: append([]common.Field{
		{Name: "channel", Kind: common.KToken, Required: true},
		{Name: "direction", Kind: common.KEnum, Required: true, Enum: []string{"in", "out", "unknown"}},
		{Name: "kind", Kind: common.KEnum, Required: true, Enum: []string{"text", "media", "reaction", "system", "unknown"}},
		{Name: "participants", Kind: common.KArray, Required: true, Obj: &common.Schema{Fields: []common.Field{
			{Name: "role", Kind: common.KEnum, Required: true, Enum: []string{"from", "to", "cc", "bcc", "member"}},
			{Name: "address", Kind: common.KString, Required: true, NonEmpty: true, MaxLen: 1024},
			{Name: "name", Kind: common.KString},
			{Name: "kind", Kind: common.KEnum, Enum: []string{"phone", "email", "shortcode", "alnum", "unknown"}},
		}}},
		{Name: "participants_unknown", Kind: common.KBool},
		{Name: "thread", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "id", Kind: common.KIDString},
			{Name: "title", Kind: common.KString},
			{Name: "group", Kind: common.KBool},
			{Name: "members", Kind: common.KStringArray},
		}}},
		{Name: "status", Kind: common.KEnum, Enum: []string{"received", "sent", "delivered", "failed", "queued", "draft", "unknown"}},
		{Name: "read", Kind: common.KBool},
		{Name: "subject", Kind: common.KString},
		{Name: "attachments", Kind: common.KArray, Obj: &common.Schema{
			Fields: []common.Field{
				{Name: "name", Kind: common.KString},
				{Name: "mime", Kind: common.KString},
				{Name: "size", Kind: common.KInt, Min: new(int64)},
				{Name: "source_ref", Kind: common.KString},
				{Name: "artifact_id", Kind: common.KString, NonEmpty: true},
				{Name: "artifact_sha256", Kind: common.KHex64},
			},
			Cross: func(m map[string]any) error {
				if _, ok := m["artifact_sha256"]; ok {
					if _, ok := m["artifact_id"]; !ok {
						return errors.New("artifact_sha256 requires artifact_id")
					}
				}
				return nil
			},
		}},
		{Name: "guid", Kind: common.KString},
		{Name: "target_guid", Kind: common.KString},
		{Name: "service", Kind: common.KString},
		{Name: "subscription", Kind: common.KInt},
		{Name: "text_source", Kind: common.KEnum, Enum: []string{"text", "attributedBody", "part", "summary"}},
		{Name: "body_flags", Kind: common.KStringArray},
		{Name: "body_truncated", Kind: common.KBool},
		{Name: "body_total_bytes", Kind: common.KInt, Min: new(int64)},
		{Name: "body_raw_b64", Kind: common.KString},
		{Name: "body_raw_sha256", Kind: common.KHex64},
		{Name: "body_raw_len", Kind: common.KInt, Min: new(int64)},
	}, common.CommonFields()...),
	Cross: crossRules,
}

// crossRules are the rules that span fields of one payload.
func crossRules(m map[string]any) error {
	if parts, ok := m["participants"].([]any); ok && len(parts) == 0 && m["participants_unknown"] != true {
		return errors.New("participants is empty without participants_unknown")
	}
	if t, ok := m["body_truncated"].(bool); ok && t {
		if _, has := m["body_total_bytes"]; !has {
			return errors.New("body_truncated requires body_total_bytes")
		}
	}
	if flags, ok := m["body_flags"].([]any); ok {
		for _, f := range flags {
			switch f {
			case common.FlagInvalidUTF8, common.FlagNUL, common.FlagTruncated:
			default:
				return errors.New("body_flags: an entry is not one of invalid_utf8,nul,truncated")
			}
		}
	}
	return nil
}

// Validate checks a message payload against the v1 contract. Its errors name
// field paths and never a payload value; unknown fields are allowed.
func Validate(payload map[string]any) error { return schema.Validate(payload) }

func init() { records.SetValidator(Type, Validate) }

// Summary builds the record summary "<channel> <in|out> <counterparty>: <first
// 80 characters of text>", one line, through common.Summarize (the text counts
// characters, not bytes; format and bidi characters are shown as <U+XXXX>, see
// common.Summarize). The other parts are bounded in bytes (32, 16, 64) so the
// whole stays under the 512-byte summary cap even for 4-byte characters.
func Summary(channel, direction, counterparty, text string) string {
	head := strings.Join([]string{
		common.Summarize(channel, 32),
		common.Summarize(direction, 16),
		common.Summarize(counterparty, 64),
	}, " ")
	return strings.TrimRight(head+": "+common.SummarizeChars(text, 80), " ")
}

// Decode reads a stored message payload of the given payload version. Only
// version 1 exists; any other value is ErrUnsupportedPayloadVersion. Integers are
// canonical JSON integers (the validator refuses 1e3 and 2.0). Numbers are
// read exactly (json.Number) and become int64 where integral, so the result
// equals what Payload produced. Unknown fields are ignored. A payload that does
// not satisfy the v1 contract is an error naming the path, never a value.
func Decode(payloadV int, payload []byte) (Message, error) {
	if payloadV != PayloadVersion {
		return Message{}, fmt.Errorf("%w: %d", ErrUnsupportedPayloadVersion, payloadV)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Message{}, errors.New("message: the payload is not a JSON object")
	}
	if len(bytes.TrimSpace(payload[dec.InputOffset():])) != 0 {
		return Message{}, errors.New("message: the payload holds more than one JSON value")
	}
	if m == nil {
		return Message{}, errors.New("message: the payload is not a JSON object")
	}
	if err := Validate(m); err != nil {
		return Message{}, fmt.Errorf("message: %w", err)
	}
	return fromMap(m), nil
}

// fromMap extracts a validated payload. Every access is by comma-ok type
// assertion, so a value of an unexpected type is skipped rather than trusted.
func fromMap(m map[string]any) Message {
	var out Message
	out.Channel, _ = m["channel"].(string)
	out.Direction, _ = m["direction"].(string)
	out.Kind, _ = m["kind"].(string)
	if parts, ok := m["participants"].([]any); ok {
		for _, e := range parts {
			em, _ := e.(map[string]any)
			out.Participants = append(out.Participants, Participant{
				Role: str(em, "role"), Address: str(em, "address"), Name: str(em, "name"), Kind: str(em, "kind"),
			})
		}
	}
	out.ParticipantsUnknown, _ = m["participants_unknown"].(bool)
	if tm, ok := m["thread"].(map[string]any); ok {
		t := &Thread{ID: idString(tm["id"]), Title: str(tm, "title"), Group: boolPtr(tm, "group"), Members: strs(tm["members"])}
		out.Thread = t
	}
	out.Status = str(m, "status")
	out.Read = boolPtr(m, "read")
	out.Subject = str(m, "subject")
	if atts, ok := m["attachments"].([]any); ok {
		for _, e := range atts {
			am, _ := e.(map[string]any)
			out.Attachments = append(out.Attachments, Attachment{
				Name: str(am, "name"), Mime: str(am, "mime"), SourceRef: str(am, "source_ref"), ArtifactID: str(am, "artifact_id"),
				ArtifactSHA256: str(am, "artifact_sha256"), Size: intPtr(am, "size"),
			})
		}
	}
	out.GUID = str(m, "guid")
	out.TargetGUID = str(m, "target_guid")
	out.Service = str(m, "service")
	out.Subscription = intPtr(m, "subscription")
	out.TextSource = str(m, "text_source")
	out.BodyFlags = strs(m["body_flags"])
	out.BodyTruncated = boolPtr(m, "body_truncated")
	out.BodyTotalBytes = intPtr(m, "body_total_bytes")
	out.BodyRawB64 = str(m, "body_raw_b64")
	out.BodyRawSHA256 = str(m, "body_raw_sha256")
	out.BodyRawLen = intPtr(m, "body_raw_len")
	if d, ok := m["deleted"].(map[string]any); ok {
		out.Deleted = common.NormMap(d)
	}
	if r, ok := m["recovery"].(map[string]any); ok {
		out.Recovery = common.NormMap(r)
	}
	if sn, ok := m["snapshot"].(map[string]any); ok {
		out.Snapshot = common.NormMap(sn)
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

// idString renders a thread id that may be a string or an integer (KIDString).
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

func boolPtr(m map[string]any, key string) *bool {
	if b, ok := m[key].(bool); ok {
		return &b
	}
	return nil
}

func intPtr(m map[string]any, key string) *int64 {
	if n, ok := m[key].(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return &i
		}
	}
	return nil
}

func strs(v any) []string {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
}

// Live reports whether the message is a plain live one: it carries no recovery,
// snapshot or deleted provenance (see common.IsLive).
func (m Message) Live() bool { return common.IsLive(m.Recovery, m.Snapshot, m.Deleted) }
