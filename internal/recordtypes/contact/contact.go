// Package contact defines the payload of the "contact" record type, version 1:
// the builder parsers use to produce it (Contact.Payload), the validator the
// records writer runs on every contact (Validate, installed at init), the typed
// reader for stored payloads (Decode) and the searchable Body and Summary. It is
// a pure package: it imports only internal/records (SetValidator, in init) and
// internal/recordtypes/common.
//
// The payload follows the contract of the plan's F10 table: at least one of a
// display name, a phone, an e-mail address or an organization (a tombstone, the
// deleted marker with a non-empty source id, needs none of them: spec 9.3);
// everything else optional and absent (never an empty string, object or list) when the parser
// does not know it (C3). Phone numbers, e-mail addresses and names are the source
// strings, never normalized (C5). Unknown fields in a stored payload are ignored
// by Decode; the provenance objects recovery, snapshot and deleted are never
// ignored.
//
// A parser that has to sanitise text the contract refuses (invalid UTF-8, NUL)
// keeps the original bytes in raw (base64): the payload stores only what the
// writer accepts, and the typed value does not flag the replacement.
package contact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// Type is the record type name and PayloadVersion the payload version this
// package writes and reads.
const (
	Type           = "contact"
	PayloadVersion = 1
)

// ErrUnsupportedPayloadVersion is returned by Decode for a payload version this
// package does not read.
var ErrUnsupportedPayloadVersion = errors.New("contact: unsupported payload version")

// Names are the parts of a person's name as the source stores them.
type Names struct{ Given, Family, Middle, Prefix, Suffix, Nickname, Phonetic string }

// Organization is the employer data of a contact.
type Organization struct{ Name, Title, Department string }

// Value is one phone number, e-mail address, URL or instant-messaging handle:
// the source string, its label and kind as stored, and whether it is the primary.
type Value struct {
	Value, Label, Kind string
	Primary            *bool
}

// Address is one postal address.
type Address struct{ Formatted, Street, City, Region, Postcode, Country, Label string }

// Account is the account a contact row belongs to (Android raw_contacts).
type Account struct{ Type, Name string }

// Contact is the typed form of a contact payload. A zero value (empty string, nil
// pointer or slice) means "not known" and is absent from the payload. Deleted
// (the application's own deleted marker), Recovery and Snapshot are provenance:
// a typed reader must not present a contact that carries them as a live one.
type Contact struct {
	DisplayName, Birthday, SourceID string
	Names                           *Names
	Organization                    *Organization
	Phones, Emails, URLs, IMs       []Value
	Addresses                       []Address
	Accounts                        []Account
	Starred                         *bool
	PhotoPresent                    *bool
	PhotoArtifactID                 string
	SourceIDs                       []string
	Deleted, Recovery, Snapshot     map[string]any
	Raw                             map[string]any
}

func put(m map[string]any, key, v string) {
	if v != "" {
		m[key] = v
	}
}

// putObject writes obj under key unless it holds nothing (C3: no empty objects).
func putObject(m map[string]any, key string, obj map[string]any) {
	if len(obj) > 0 {
		m[key] = obj
	}
}

// valueList writes the non-empty elements of a list of values; a list with no
// such element is not written. An element that carries a label but no value is
// written as it is (the validator refuses it: that is a parser bug, not an absent
// value).
func valueList(m map[string]any, key string, vs []Value) {
	var out []any
	for _, v := range vs {
		e := map[string]any{}
		put(e, "value", v.Value)
		put(e, "label", v.Label)
		put(e, "kind", v.Kind)
		if v.Primary != nil {
			e["primary"] = *v.Primary
		}
		if len(e) > 0 {
			out = append(out, e)
		}
	}
	if len(out) > 0 {
		m[key] = out
	}
}

// Payload returns the payload map in the canonical value types of the records
// package (string, bool, int64, []any, map[string]any). Empty strings, nil
// pointers, empty objects and lists, and list elements that hold nothing are
// omitted; a *bool that is set is written even when false. The free-form objects
// are deep-copied, so the payload never aliases the Contact.
func (c Contact) Payload() map[string]any {
	p := map[string]any{}
	put(p, "display_name", c.DisplayName)
	if n := c.Names; n != nil {
		e := map[string]any{}
		put(e, "given", n.Given)
		put(e, "family", n.Family)
		put(e, "middle", n.Middle)
		put(e, "prefix", n.Prefix)
		put(e, "suffix", n.Suffix)
		put(e, "nickname", n.Nickname)
		put(e, "phonetic", n.Phonetic)
		putObject(p, "names", e)
	}
	if o := c.Organization; o != nil {
		e := map[string]any{}
		put(e, "name", o.Name)
		put(e, "title", o.Title)
		put(e, "department", o.Department)
		putObject(p, "organization", e)
	}
	valueList(p, "phones", c.Phones)
	valueList(p, "emails", c.Emails)
	valueList(p, "urls", c.URLs)
	valueList(p, "ims", c.IMs)
	var addrs []any
	for _, a := range c.Addresses {
		e := map[string]any{}
		put(e, "formatted", a.Formatted)
		put(e, "street", a.Street)
		put(e, "city", a.City)
		put(e, "region", a.Region)
		put(e, "postcode", a.Postcode)
		put(e, "country", a.Country)
		put(e, "label", a.Label)
		if len(e) > 0 {
			addrs = append(addrs, e)
		}
	}
	if len(addrs) > 0 {
		p["addresses"] = addrs
	}
	put(p, "birthday", c.Birthday)
	var accts []any
	for _, a := range c.Accounts {
		e := map[string]any{}
		put(e, "type", a.Type)
		put(e, "name", a.Name)
		if len(e) > 0 {
			accts = append(accts, e)
		}
	}
	if len(accts) > 0 {
		p["accounts"] = accts
	}
	if c.Starred != nil {
		p["starred"] = *c.Starred
	}
	photo := map[string]any{}
	if c.PhotoPresent != nil {
		photo["present"] = *c.PhotoPresent
	}
	put(photo, "artifact_id", c.PhotoArtifactID)
	putObject(p, "photo", photo)
	put(p, "source_id", c.SourceID)
	if len(c.SourceIDs) > 0 {
		ids := make([]any, len(c.SourceIDs))
		for i, s := range c.SourceIDs {
			ids[i] = s
		}
		p["source_ids"] = ids
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

// valueSchema is the contract of one element of phones, emails, urls and ims.
func valueSchema() *common.Schema {
	return &common.Schema{Fields: []common.Field{
		{Name: "value", Kind: common.KString, Required: true, NonEmpty: true},
		{Name: "label", Kind: common.KString, NonEmpty: true},
		{Name: "kind", Kind: common.KString, NonEmpty: true},
		{Name: "primary", Kind: common.KBool},
	}}
}

// The payload contract. The schema is an unexported package variable used only as
// the receiver of Validate (the purity rules accept nothing else).
var schema = common.Schema{
	Fields: append([]common.Field{
		{Name: "display_name", Kind: common.KString},
		{Name: "names", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "given", Kind: common.KString, NonEmpty: true},
			{Name: "family", Kind: common.KString, NonEmpty: true},
			{Name: "middle", Kind: common.KString, NonEmpty: true},
			{Name: "prefix", Kind: common.KString, NonEmpty: true},
			{Name: "suffix", Kind: common.KString, NonEmpty: true},
			{Name: "nickname", Kind: common.KString, NonEmpty: true},
			{Name: "phonetic", Kind: common.KString, NonEmpty: true},
		}}},
		{Name: "organization", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "name", Kind: common.KString},
			{Name: "title", Kind: common.KString},
			{Name: "department", Kind: common.KString},
		}}},
		{Name: "phones", Kind: common.KArray, Obj: valueSchema()},
		{Name: "emails", Kind: common.KArray, Obj: valueSchema()},
		{Name: "urls", Kind: common.KArray, Obj: valueSchema()},
		{Name: "ims", Kind: common.KArray, Obj: valueSchema()},
		{Name: "addresses", Kind: common.KArray, Obj: &common.Schema{Fields: []common.Field{
			{Name: "formatted", Kind: common.KString, NonEmpty: true},
			{Name: "street", Kind: common.KString, NonEmpty: true},
			{Name: "city", Kind: common.KString, NonEmpty: true},
			{Name: "region", Kind: common.KString, NonEmpty: true},
			{Name: "postcode", Kind: common.KString, NonEmpty: true},
			{Name: "country", Kind: common.KString, NonEmpty: true},
			{Name: "label", Kind: common.KString, NonEmpty: true},
		}}},
		{Name: "birthday", Kind: common.KDate},
		{Name: "accounts", Kind: common.KArray, Obj: &common.Schema{Fields: []common.Field{
			{Name: "type", Kind: common.KString, NonEmpty: true},
			{Name: "name", Kind: common.KString, NonEmpty: true},
		}}},
		{Name: "starred", Kind: common.KBool},
		{Name: "photo", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "present", Kind: common.KBool},
			{Name: "artifact_id", Kind: common.KString, NonEmpty: true},
		}}},
		{Name: "source_id", Kind: common.KString, NonEmpty: true},
		{Name: "source_ids", Kind: common.KStringArray},
	}, common.CommonFields()...),
	Cross: crossRules,
}

// crossRules are the rules that span fields of one payload: the contact must say
// who it is. An empty display name, an empty list or an organization whose fields
// are all empty identify nobody. The one exception is a tombstone (spec 9.3, the
// deleted_contacts table): a payload with the deleted marker and at least one
// non-empty source id says which source row was deleted, and nothing else.
func crossRules(m map[string]any) error {
	if _, ok := m["deleted"].(map[string]any); ok {
		if ids, _ := m["source_ids"].([]any); slices.ContainsFunc(ids, func(id any) bool { s, _ := id.(string); return s != "" }) {
			return nil
		}
	}
	if s, _ := m["display_name"].(string); s != "" {
		return nil
	}
	for _, k := range []string{"phones", "emails"} {
		if l, _ := m[k].([]any); len(l) > 0 {
			return nil
		}
	}
	if o, ok := m["organization"].(map[string]any); ok {
		for _, k := range []string{"name", "title", "department"} {
			if s, _ := o[k].(string); s != "" {
				return nil
			}
		}
	}
	return errors.New("at least one of display_name,phones,emails,organization must be present and not empty")
}

// Validate checks a contact payload against the v1 contract. Its errors name
// field paths and never a payload value; unknown fields are allowed.
func Validate(payload map[string]any) error { return schema.Validate(payload) }

func init() { records.SetValidator(Type, Validate) }

// Body is the searchable text of the contact: the display name, then one line for
// every phone, every e-mail address, every postal address and the organization,
// in that order, so a partial number or address is found by the substring index.
// The values are exactly as stored (the caller has made them valid text; nothing
// is trimmed or collapsed), empty ones are skipped, and no list is cut. A postal
// address is its formatted form, or else its street, city, region, postcode and
// country joined by ", "; the organization is its name, title and department
// joined the same way. URLs and instant-messaging handles are not listed.
func Body(c Contact) string {
	var lines []string
	add := func(s string) {
		if s != "" {
			lines = append(lines, s)
		}
	}
	add(c.DisplayName)
	for _, v := range c.Phones {
		add(v.Value)
	}
	for _, v := range c.Emails {
		add(v.Value)
	}
	for _, a := range c.Addresses {
		if a.Formatted != "" {
			add(a.Formatted)
			continue
		}
		add(joinNonEmpty(a.Street, a.City, a.Region, a.Postcode, a.Country))
	}
	if o := c.Organization; o != nil {
		add(joinNonEmpty(o.Name, o.Title, o.Department))
	}
	return strings.Join(lines, "\n")
}

func joinNonEmpty(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}

// Summary builds the record summary: the display name (at most 80 characters) and
// the first phone (at most 32 bytes), one line, through common.Summarize (format
// and bidi characters are shown as <U+XXXX>). A contact with neither shows its
// first e-mail address, then its organization (name, else title, else department).
func Summary(c Contact) string {
	name := common.SummarizeChars(c.DisplayName, 80)
	phone := ""
	for _, v := range c.Phones {
		if phone = common.Summarize(v.Value, 32); phone != "" {
			break
		}
	}
	if name != "" || phone != "" {
		return joinSpace(name, phone)
	}
	for _, v := range c.Emails {
		if s := common.SummarizeChars(v.Value, 80); s != "" {
			return s
		}
	}
	if o := c.Organization; o != nil {
		for _, v := range []string{o.Name, o.Title, o.Department} {
			if s := common.SummarizeChars(v, 80); s != "" {
				return s
			}
		}
	}
	if c.Deleted != nil { // a tombstone: the source row that was deleted
		for _, id := range c.SourceIDs {
			if s := common.Summarize(id, 64); s != "" {
				return "deleted contact " + s
			}
		}
	}
	return ""
}

func joinSpace(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + " " + b
}

// Decode reads a stored contact payload of the given payload version. Only
// version 1 exists; any other value is ErrUnsupportedPayloadVersion. Numbers are
// read exactly (json.Number): the numbers inside the free-form objects become
// int64 where integral, so the result equals what Payload produced. Unknown
// fields are ignored. A payload that does not satisfy the v1 contract is an error
// naming the path, never a value.
func Decode(payloadV int, payload []byte) (Contact, error) {
	if payloadV != PayloadVersion {
		return Contact{}, fmt.Errorf("%w: %d", ErrUnsupportedPayloadVersion, payloadV)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return Contact{}, errors.New("contact: the payload is not a JSON object")
	}
	if len(bytes.TrimSpace(payload[dec.InputOffset():])) != 0 {
		return Contact{}, errors.New("contact: the payload holds more than one JSON value")
	}
	if m == nil {
		return Contact{}, errors.New("contact: the payload is not a JSON object")
	}
	if err := Validate(m); err != nil {
		return Contact{}, fmt.Errorf("contact: %w", err)
	}
	return fromMap(m), nil
}

// fromMap extracts a validated payload. Every access is by comma-ok type
// assertion, so a value of an unexpected type is skipped rather than trusted.
func fromMap(m map[string]any) Contact {
	var out Contact
	out.DisplayName = str(m, "display_name")
	if nm, ok := m["names"].(map[string]any); ok {
		n := Names{
			Given: str(nm, "given"), Family: str(nm, "family"), Middle: str(nm, "middle"), Prefix: str(nm, "prefix"),
			Suffix: str(nm, "suffix"), Nickname: str(nm, "nickname"), Phonetic: str(nm, "phonetic"),
		}
		if n != (Names{}) {
			out.Names = &n
		}
	}
	if om, ok := m["organization"].(map[string]any); ok {
		o := Organization{Name: str(om, "name"), Title: str(om, "title"), Department: str(om, "department")}
		if o != (Organization{}) {
			out.Organization = &o
		}
	}
	out.Phones = values(m["phones"])
	out.Emails = values(m["emails"])
	out.URLs = values(m["urls"])
	out.IMs = values(m["ims"])
	if arr, ok := m["addresses"].([]any); ok {
		for _, e := range arr {
			em, _ := e.(map[string]any)
			out.Addresses = append(out.Addresses, Address{
				Formatted: str(em, "formatted"), Street: str(em, "street"), City: str(em, "city"), Region: str(em, "region"),
				Postcode: str(em, "postcode"), Country: str(em, "country"), Label: str(em, "label"),
			})
		}
	}
	out.Birthday = str(m, "birthday")
	if arr, ok := m["accounts"].([]any); ok {
		for _, e := range arr {
			em, _ := e.(map[string]any)
			out.Accounts = append(out.Accounts, Account{Type: str(em, "type"), Name: str(em, "name")})
		}
	}
	out.Starred = boolPtr(m, "starred")
	if pm, ok := m["photo"].(map[string]any); ok {
		out.PhotoPresent = boolPtr(pm, "present")
		out.PhotoArtifactID = str(pm, "artifact_id")
	}
	out.SourceID = str(m, "source_id")
	if arr, ok := m["source_ids"].([]any); ok {
		for _, e := range arr {
			s, _ := e.(string)
			out.SourceIDs = append(out.SourceIDs, s)
		}
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

func values(v any) []Value {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []Value
	for _, e := range arr {
		em, _ := e.(map[string]any)
		out = append(out, Value{Value: str(em, "value"), Label: str(em, "label"), Kind: str(em, "kind"), Primary: boolPtr(em, "primary")})
	}
	return out
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func boolPtr(m map[string]any, key string) *bool {
	if b, ok := m[key].(bool); ok {
		return &b
	}
	return nil
}

// Live reports whether the contact is a plain live one: it carries no recovery,
// snapshot or deleted provenance (see common.IsLive).
func (c Contact) Live() bool { return common.IsLive(c.Recovery, c.Snapshot, c.Deleted) }
