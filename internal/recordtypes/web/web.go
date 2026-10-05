// Package web defines the payloads of the "web_visit", "web_search" and
// "download" record types, version 1: the builders parsers use to produce them
// (Visit.Payload, Search.Payload, Download.Payload), the validators the records
// writer runs on every such record (ValidateVisit, ValidateSearch,
// ValidateDownload, installed at init), the typed readers for stored payloads
// (DecodeVisit, DecodeSearch, DecodeDownload) and the searchable VisitBody and
// VisitSummary. It is a pure package: it imports only internal/records
// (SetValidator, in init) and internal/recordtypes/common.
//
// The payloads follow the contract of the plan's F10 table. A field the source
// does not have is absent, never an empty string, object or list (C3). URLs are
// stored exactly as the source holds them (case, port, dot segments, query order
// and fragment untouched): a normalized form can only ever be an additional field
// (web_search has normalized_term; nothing else has one). Ids that sources store
// as numbers are written as strings by the builders and accepted either way by
// the validators; Decode returns them as strings. Unknown fields in a stored
// payload are ignored by Decode; the provenance objects recovery, snapshot and
// deleted are never ignored.
package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The record type names and the payload version these packages write and read.
const (
	VisitType      = "web_visit"
	SearchType     = "web_search"
	DownloadType   = "download"
	PayloadVersion = 1
)

// maxPath bounds the local path of a download in bytes (the records source path
// limit).
const maxPath = 4096

// ErrUnsupportedPayloadVersion is returned by the Decode functions for a payload
// version this package does not read.
var ErrUnsupportedPayloadVersion = errors.New("web: unsupported payload version")

// Referrer is the visit (and its URL) a visit came from. The visit id is a string
// (builders convert numeric ids, for example with strconv).
type Referrer struct{ VisitID, URL string }

// Visit is the typed form of a web_visit payload. A zero value (empty string, nil
// pointer or slice) means "not known" and is absent from the payload. URL and
// Referrer.URL are verbatim. Deleted, Recovery and Snapshot are provenance: a
// typed reader must not present a visit that carries them as a live one (Live).
type Visit struct {
	URL, Browser, Title, VisitID, Profile, Transition string
	TransitionQualifiers                              []string
	Referrer                                          *Referrer
	RedirectSource, RedirectDestination               string
	DurationS                                         *float64
	URLVisitCount, TypedCount, StatusCode             *int64
	Hidden, LoadSuccessful                            *bool
	Deleted, Recovery, Snapshot                       map[string]any
	Raw                                               map[string]any
}

// Search is the typed form of a web_search payload. Term is the term as typed;
// NormalizedTerm is the source's own normalized form, an additional field.
type Search struct {
	Term, NormalizedTerm, Engine, EngineID, URL, VisitID, TimeSource, Browser string
	Deleted, Recovery, Snapshot                                               map[string]any
	Raw                                                                       map[string]any
}

// Download is the typed form of a download payload. TargetPath and CurrentPath
// are the local paths as stored (at most 4096 bytes); the sizes are bytes.
type Download struct {
	TargetPath, CurrentPath, URL, Referrer, TabURL, Mime, State, DangerType, InterruptReason, Browser string
	URLChain                                                                                          []string
	TotalBytes, ReceivedBytes                                                                         *int64
	Opened                                                                                            *bool
	Deleted, Recovery, Snapshot                                                                       map[string]any
	Raw                                                                                               map[string]any
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

func putStrings(m map[string]any, key string, vs []string) {
	if len(vs) == 0 {
		return
	}
	out := make([]any, len(vs))
	for i, s := range vs {
		out[i] = s
	}
	m[key] = out
}

// putProvenance writes the provenance and raw objects, deep-copied.
func putProvenance(p map[string]any, deleted, recovery, snapshot, raw map[string]any) {
	for _, e := range []struct {
		key string
		m   map[string]any
	}{{"deleted", deleted}, {"recovery", recovery}, {"snapshot", snapshot}, {"raw", raw}} {
		if e.m != nil {
			p[e.key] = common.CopyMap(e.m)
		}
	}
}

// Payload returns the payload map in the canonical value types of the records
// package (string, bool, int64, float64, []any, map[string]any). Empty strings,
// nil pointers, empty objects and lists are omitted; a *bool, *int64 or *float64
// that is set is written even when false or 0. The free-form objects are
// deep-copied, so the payload never aliases the Visit.
func (v Visit) Payload() map[string]any {
	p := map[string]any{}
	put(p, "url", v.URL)
	put(p, "browser", v.Browser)
	put(p, "title", v.Title)
	put(p, "visit_id", v.VisitID)
	put(p, "profile", v.Profile)
	put(p, "transition", v.Transition)
	putStrings(p, "transition_qualifiers", v.TransitionQualifiers)
	if r := v.Referrer; r != nil {
		e := map[string]any{}
		put(e, "visit_id", r.VisitID)
		put(e, "url", r.URL)
		putObject(p, "referrer", e)
	}
	redirect := map[string]any{}
	put(redirect, "source_visit_id", v.RedirectSource)
	put(redirect, "destination_visit_id", v.RedirectDestination)
	putObject(p, "redirect", redirect)
	if v.DurationS != nil {
		p["duration_s"] = *v.DurationS
	}
	if v.URLVisitCount != nil {
		p["url_visit_count"] = *v.URLVisitCount
	}
	if v.TypedCount != nil {
		p["typed_count"] = *v.TypedCount
	}
	if v.StatusCode != nil {
		p["status_code"] = *v.StatusCode
	}
	if v.Hidden != nil {
		p["hidden"] = *v.Hidden
	}
	if v.LoadSuccessful != nil {
		p["load_successful"] = *v.LoadSuccessful
	}
	putProvenance(p, v.Deleted, v.Recovery, v.Snapshot, v.Raw)
	return p
}

// Payload returns the payload map of a search (see Visit.Payload for the rules).
func (s Search) Payload() map[string]any {
	p := map[string]any{}
	put(p, "term", s.Term)
	put(p, "normalized_term", s.NormalizedTerm)
	put(p, "engine", s.Engine)
	put(p, "engine_id", s.EngineID)
	put(p, "url", s.URL)
	put(p, "visit_id", s.VisitID)
	put(p, "time_source", s.TimeSource)
	put(p, "browser", s.Browser)
	putProvenance(p, s.Deleted, s.Recovery, s.Snapshot, s.Raw)
	return p
}

// Payload returns the payload map of a download (see Visit.Payload for the rules).
func (d Download) Payload() map[string]any {
	p := map[string]any{}
	put(p, "target_path", d.TargetPath)
	put(p, "current_path", d.CurrentPath)
	put(p, "url", d.URL)
	putStrings(p, "url_chain", d.URLChain)
	put(p, "referrer", d.Referrer)
	put(p, "tab_url", d.TabURL)
	put(p, "mime", d.Mime)
	if d.TotalBytes != nil {
		p["total_bytes"] = *d.TotalBytes
	}
	if d.ReceivedBytes != nil {
		p["received_bytes"] = *d.ReceivedBytes
	}
	put(p, "state", d.State)
	put(p, "danger_type", d.DangerType)
	put(p, "interrupt_reason", d.InterruptReason)
	if d.Opened != nil {
		p["opened"] = *d.Opened
	}
	put(p, "browser", d.Browser)
	putProvenance(p, d.Deleted, d.Recovery, d.Snapshot, d.Raw)
	return p
}

// browsers is the vocabulary of the browser field (and "unknown", always accepted);
// a function, so no package-level slice exists.
func browsers() []string {
	return []string{"chrome", "chromium", "webview", "edge", "brave", "opera", "safari", "unknown-chromium"}
}

func transitions() []string {
	return []string{
		"link", "typed", "auto_bookmark", "auto_subframe", "manual_subframe", "generated", "auto_toplevel", "form_submit", "reload", "keyword",
		"keyword_generated",
	}
}

// The payload contracts. The schemas are unexported package variables used only as
// the receivers of Validate (the purity rules accept nothing else).
var (
	visitSchema = common.Schema{Fields: append([]common.Field{
		{Name: "url", Kind: common.KString, Required: true, NonEmpty: true},
		{Name: "browser", Kind: common.KEnum, Required: true, Enum: browsers()},
		{Name: "title", Kind: common.KString},
		{Name: "visit_id", Kind: common.KIDString},
		{Name: "profile", Kind: common.KString},
		{Name: "transition", Kind: common.KEnum, Enum: transitions()},
		{Name: "transition_qualifiers", Kind: common.KStringArray},
		{Name: "referrer", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "visit_id", Kind: common.KIDString},
			{Name: "url", Kind: common.KString, NonEmpty: true},
		}}},
		{Name: "redirect", Kind: common.KObject, Obj: &common.Schema{Fields: []common.Field{
			{Name: "source_visit_id", Kind: common.KIDString},
			{Name: "destination_visit_id", Kind: common.KIDString},
		}}},
		{Name: "duration_s", Kind: common.KNumber, Min: new(int64)},
		{Name: "url_visit_count", Kind: common.KInt, Min: new(int64)},
		{Name: "typed_count", Kind: common.KInt, Min: new(int64)},
		{Name: "status_code", Kind: common.KInt},
		{Name: "hidden", Kind: common.KBool},
		{Name: "load_successful", Kind: common.KBool},
	}, common.CommonFields()...)}

	searchSchema = common.Schema{Fields: append([]common.Field{
		{Name: "term", Kind: common.KString, Required: true, NonEmpty: true},
		{Name: "normalized_term", Kind: common.KString},
		{Name: "engine", Kind: common.KString},
		{Name: "engine_id", Kind: common.KIDString},
		{Name: "url", Kind: common.KString, NonEmpty: true},
		{Name: "visit_id", Kind: common.KIDString},
		{Name: "time_source", Kind: common.KString},
		{Name: "browser", Kind: common.KEnum, Enum: browsers()},
	}, common.CommonFields()...)}

	downloadSchema = common.Schema{Fields: append([]common.Field{
		{Name: "target_path", Kind: common.KString, NonEmpty: true, MaxLen: maxPath},
		{Name: "current_path", Kind: common.KString, NonEmpty: true, MaxLen: maxPath},
		{Name: "url", Kind: common.KString, NonEmpty: true},
		{Name: "url_chain", Kind: common.KStringArray},
		{Name: "referrer", Kind: common.KString, NonEmpty: true},
		{Name: "tab_url", Kind: common.KString, NonEmpty: true},
		{Name: "mime", Kind: common.KString},
		{Name: "total_bytes", Kind: common.KInt, Min: new(int64)},
		{Name: "received_bytes", Kind: common.KInt, Min: new(int64)},
		{Name: "state", Kind: common.KString},
		{Name: "danger_type", Kind: common.KString},
		{Name: "interrupt_reason", Kind: common.KString},
		{Name: "opened", Kind: common.KBool},
		{Name: "browser", Kind: common.KEnum, Enum: browsers()},
	}, common.CommonFields()...), AtLeastOne: []string{"target_path", "url"}}
)

// ValidateVisit checks a web_visit payload against the v1 contract. Its errors
// name field paths and never a payload value; unknown fields are allowed.
func ValidateVisit(payload map[string]any) error { return visitSchema.Validate(payload) }

// ValidateSearch checks a web_search payload against the v1 contract.
func ValidateSearch(payload map[string]any) error { return searchSchema.Validate(payload) }

// ValidateDownload checks a download payload against the v1 contract.
func ValidateDownload(payload map[string]any) error { return downloadSchema.Validate(payload) }

func init() {
	records.SetValidator(VisitType, ValidateVisit)
	records.SetValidator(SearchType, ValidateSearch)
	records.SetValidator(DownloadType, ValidateDownload)
}

// VisitBody is the searchable text of a visit: the URL, then the title on a line of
// its own, both exactly as stored (nothing trimmed or collapsed), so a fragment or
// a query value is found by the substring index. An absent part is skipped.
func VisitBody(v Visit) string {
	switch {
	case v.URL == "":
		return v.Title
	case v.Title == "":
		return v.URL
	}
	return v.URL + "\n" + v.Title
}

// VisitSummary builds the record summary: the title, else the URL, at most 80
// characters, one line, through common.SummarizeChars (format and bidi characters
// are shown as <U+XXXX>).
func VisitSummary(v Visit) string {
	if s := common.SummarizeChars(v.Title, 80); s != "" {
		return s
	}
	return common.SummarizeChars(v.URL, 80)
}

// decodeObject parses a stored payload: one JSON object with exact numbers,
// validated against the contract.
func decodeObject(kind string, payloadV int, payload []byte, validate func(map[string]any) error) (map[string]any, error) {
	if payloadV != PayloadVersion {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedPayloadVersion, payloadV)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: the payload is not a JSON object", kind)
	}
	if len(bytes.TrimSpace(payload[dec.InputOffset():])) != 0 {
		return nil, fmt.Errorf("%s: the payload holds more than one JSON value", kind)
	}
	if m == nil {
		return nil, fmt.Errorf("%s: the payload is not a JSON object", kind)
	}
	if err := validate(m); err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	return m, nil
}

// DecodeVisit reads a stored web_visit payload of the given payload version. Only
// version 1 exists; any other value is ErrUnsupportedPayloadVersion. Numbers are
// read exactly (json.Number): integers are canonical JSON integers, numeric ids
// become their decimal string, durations become float64, and the numbers inside
// the free-form objects become int64 where integral, so the result equals what
// Payload produced. Unknown fields are ignored. A payload that does not satisfy
// the v1 contract is an error naming the path, never a value.
func DecodeVisit(payloadV int, payload []byte) (Visit, error) {
	m, err := decodeObject("web_visit", payloadV, payload, ValidateVisit)
	if err != nil {
		return Visit{}, err
	}
	out := Visit{
		URL: str(m, "url"), Browser: str(m, "browser"), Title: str(m, "title"), VisitID: idStr(m, "visit_id"), Profile: str(m, "profile"),
		Transition: str(m, "transition"), TransitionQualifiers: strs(m, "transition_qualifiers"),
		DurationS: floatPtr(m, "duration_s"), URLVisitCount: intPtr(m, "url_visit_count"), TypedCount: intPtr(m, "typed_count"),
		StatusCode: intPtr(m, "status_code"), Hidden: boolPtr(m, "hidden"), LoadSuccessful: boolPtr(m, "load_successful"),
	}
	if rm, ok := m["referrer"].(map[string]any); ok {
		if r := (Referrer{VisitID: idStr(rm, "visit_id"), URL: str(rm, "url")}); r != (Referrer{}) {
			out.Referrer = &r
		}
	}
	if rm, ok := m["redirect"].(map[string]any); ok {
		out.RedirectSource, out.RedirectDestination = idStr(rm, "source_visit_id"), idStr(rm, "destination_visit_id")
	}
	out.Deleted, out.Recovery, out.Snapshot, out.Raw = provenance(m)
	return out, nil
}

// DecodeSearch reads a stored web_search payload (see DecodeVisit for the rules).
func DecodeSearch(payloadV int, payload []byte) (Search, error) {
	m, err := decodeObject("web_search", payloadV, payload, ValidateSearch)
	if err != nil {
		return Search{}, err
	}
	out := Search{
		Term: str(m, "term"), NormalizedTerm: str(m, "normalized_term"), Engine: str(m, "engine"), EngineID: idStr(m, "engine_id"),
		URL: str(m, "url"), VisitID: idStr(m, "visit_id"), TimeSource: str(m, "time_source"), Browser: str(m, "browser"),
	}
	out.Deleted, out.Recovery, out.Snapshot, out.Raw = provenance(m)
	return out, nil
}

// DecodeDownload reads a stored download payload (see DecodeVisit for the rules).
func DecodeDownload(payloadV int, payload []byte) (Download, error) {
	m, err := decodeObject("download", payloadV, payload, ValidateDownload)
	if err != nil {
		return Download{}, err
	}
	out := Download{
		TargetPath: str(m, "target_path"), CurrentPath: str(m, "current_path"), URL: str(m, "url"), Referrer: str(m, "referrer"),
		TabURL: str(m, "tab_url"), Mime: str(m, "mime"), State: str(m, "state"), DangerType: str(m, "danger_type"),
		InterruptReason: str(m, "interrupt_reason"), Browser: str(m, "browser"), URLChain: strs(m, "url_chain"),
		TotalBytes: intPtr(m, "total_bytes"), ReceivedBytes: intPtr(m, "received_bytes"), Opened: boolPtr(m, "opened"),
	}
	out.Deleted, out.Recovery, out.Snapshot, out.Raw = provenance(m)
	return out, nil
}

// Live reports whether the visit is a plain live one: it carries no recovery,
// snapshot or deleted provenance (see common.IsLive).
func (v Visit) Live() bool { return common.IsLive(v.Recovery, v.Snapshot, v.Deleted) }

// Live reports whether the search is a plain live one (see common.IsLive).
func (s Search) Live() bool { return common.IsLive(s.Recovery, s.Snapshot, s.Deleted) }

// Live reports whether the download is a plain live one (see common.IsLive).
func (d Download) Live() bool { return common.IsLive(d.Recovery, d.Snapshot, d.Deleted) }

// provenance extracts the provenance objects and raw from a validated payload.
func provenance(m map[string]any) (deleted, recovery, snapshot, raw map[string]any) {
	obj := func(key string) map[string]any {
		if o, ok := m[key].(map[string]any); ok {
			return common.NormMap(o)
		}
		return nil
	}
	return obj("deleted"), obj("recovery"), obj("snapshot"), obj("raw")
}

// Every access below is by comma-ok type assertion, so a value of an unexpected
// type is skipped rather than trusted.

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// idStr reads an id that may be a string or an integer: an integer becomes its
// canonical decimal string.
func idStr(m map[string]any, key string) string {
	switch x := m[key].(type) {
	case string:
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return strconv.FormatInt(i, 10)
		}
	}
	return ""
}

func strs(m map[string]any, key string) []string {
	arr, _ := m[key].([]any)
	var out []string
	for _, e := range arr {
		s, _ := e.(string)
		out = append(out, s)
	}
	return out
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

func floatPtr(m map[string]any, key string) *float64 {
	if n, ok := m[key].(json.Number); ok {
		if f, err := n.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return &f
		}
	}
	return nil
}
