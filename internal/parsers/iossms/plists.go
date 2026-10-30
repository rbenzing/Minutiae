package iossms

import (
	"errors"
	"slices"

	"github.com/rbenzing/minutiae/internal/decode/plist"
	"github.com/rbenzing/minutiae/internal/parse"
)

const maxKeys = 64 // top-level keys of message_summary_info listed in raw.summary_info.keys (4E-R18)

// meter is a plist.Budget that charges the host's view and remembers what is still held, so a successful decode
// (whose charge the decoder never returns) is given back as soon as the facts are copied out.
type meter struct {
	b   *parse.BudgetView
	net int64
}

func (m *meter) Alloc(n int64) error {
	if err := m.b.Alloc(n); err != nil {
		return err
	}
	m.net += n
	return nil
}

func (m *meter) Free(n int64) {
	m.net -= n
	m.b.Free(n)
}

func (m *meter) release() { m.b.Free(m.net); m.net = 0 }

// plistFailure classifies a decode error: "budget" when the host refused the memory (a fact about this run, not
// about the data; no warning), otherwise "undecodable" (malformed, over a limit, unsupported).
func plistFailure(err error) string {
	if errors.Is(err, plist.ErrNoBudget) {
		return "budget"
	}
	return "undecodable"
}

// summaryInfo states what message_summary_info holds (4E-R18): raw.summary_info.keys (sorted top-level keys, at
// most 64), duplicate_keys when the lenient decode kept the last of repeated keys, and raw.edit_count (the summed
// array lengths under "ec" when it is a dictionary of arrays). Previous texts of edits are not extracted. A
// document that cannot be decoded is raw.message_summary_info_status "undecodable" (and a host warning); one that
// decodes to something other than a dictionary is "not-a-dictionary".
func summaryInfo(blob []byte, bud *parse.BudgetView, raw map[string]any) {
	m := &meter{b: bud}
	defer m.release()
	v, rep, err := plist.DecodeReport(blob, m)
	if err != nil {
		raw["message_summary_info_status"] = plistFailure(err)
		return
	}
	dict, ok := v.(map[string]any)
	if !ok {
		raw["message_summary_info_status"] = "not-a-dictionary"
		return
	}
	keys := make([]string, 0, len(dict))
	for k := range dict {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	info := map[string]any{}
	if len(keys) > maxKeys {
		info["keys_total"], info["keys_truncated"] = int64(len(keys)), true
		keys = keys[:maxKeys]
	}
	list := make([]any, len(keys))
	for i, k := range keys {
		list[i] = cleanField(raw, "summary_info.keys", []byte(k))
	}
	info["keys"] = list
	if rep.DuplicateKeys > 0 {
		info["duplicate_keys"] = int64(rep.DuplicateKeys)
	}
	raw["summary_info"] = info
	if ec, ok := dict["ec"].(map[string]any); ok {
		var n int64
		for _, e := range ec {
			a, isArr := e.([]any)
			if !isArr {
				return
			}
			n += int64(len(a))
		}
		raw["edit_count"] = n
	}
}

// payloadData states the class of the root object of the NSKeyedArchiver archive in payload_data (4E-R18), and
// nothing else of its content: raw.payload_data.root_class, read from the archive's own structure ($top root ->
// object -> class record), after the archive resolved completely. An archive that cannot be decoded is
// raw.payload_data_status "undecodable" (and a host warning); one whose root is not a class instance, or that has
// no root, is "no-root-class".
func payloadData(blob []byte, bud *parse.BudgetView, raw map[string]any) {
	m := &meter{b: bud}
	defer m.release()
	v, rep, err := plist.DecodeReport(blob, m)
	if err == nil {
		_, urep, uerr := plist.UnarchiveReport(v, m)
		rep, err = rep.Plus(urep), uerr
	}
	if err != nil {
		raw["payload_data_status"] = plistFailure(err)
		return
	}
	class, ok := rootClass(v)
	if !ok {
		raw["payload_data_status"] = "no-root-class"
		return
	}
	info := map[string]any{"root_class": cleanField(raw, "payload_data.root_class", []byte(class))}
	if rep.DuplicateKeys > 0 {
		info["duplicate_keys"] = int64(rep.DuplicateKeys)
	}
	raw["payload_data"] = info
}

// rootClass follows $top.root -> $objects[root] -> its $class -> $classname in a decoded archive that Unarchive
// has already accepted.
func rootClass(v any) (string, bool) {
	doc, _ := v.(map[string]any)
	top, _ := doc["$top"].(map[string]any)
	objs, _ := doc["$objects"].([]any)
	root, ok := top["root"].(plist.UID)
	if !ok || uint64(root) >= uint64(len(objs)) {
		return "", false
	}
	obj, ok := objs[root].(map[string]any)
	if !ok {
		return "", false
	}
	cu, ok := obj["$class"].(plist.UID)
	if !ok || uint64(cu) >= uint64(len(objs)) {
		return "", false
	}
	rec, _ := objs[cu].(map[string]any)
	name, ok := rec["$classname"].(string)
	return name, ok && name != ""
}
