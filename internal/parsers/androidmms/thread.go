package androidmms

import (
	"errors"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

func asciiSpace(c byte) bool { return c == ' ' || (c >= '\t' && c <= '\r') }

// recipientIDs streams the tokens of a recipient_ids cell (ASCII white space separates them)
// without allocating per token: at most maxList are parsed, the rest are only counted in
// total, a token of more than 20 bytes is unparsed without conversion. A hostile cell costs
// time, never memory in proportion to its size (ruling T20).
func recipientIDs(b []byte) (ids []int64, total int, unparsed int64) {
	for i := 0; i < len(b); {
		for i < len(b) && asciiSpace(b[i]) {
			i++
		}
		j := i
		for j < len(b) && !asciiSpace(b[j]) {
			j++
		}
		if j > i {
			if total < maxList {
				if id, ok := parseID(b[i:j]); ok {
					ids = append(ids, id)
				} else {
					unparsed++
				}
			}
			total++
		}
		i = j
	}
	return ids, total, unparsed
}

func parseID(tok []byte) (int64, bool) {
	if len(tok) > 20 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(tok), 10, 64)
	return n, err == nil
}

// threadFor builds the thread of a message: its id as stored and, when the provider's
// threads and canonical_addresses tables say so, its members. Every gap is recorded in
// raw, never filled: a recipient id with no canonical address is listed as missing, a
// thread with no row is "no-threads-row". A table that is absent from the variant
// leaves the members unknown without a marker (the Probe declared it optional).
func threadFor(m parse.MapContext, id int64, raw map[string]any) (*message.Thread, error) {
	th := &message.Thread{ID: strconv.FormatInt(id, 10)}
	trow, found, err := m.Get("threads", id)
	switch {
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return th, nil
	case err != nil:
		return nil, err
	case !found:
		raw["thread_state"] = "no-threads-row"
		return th, nil
	}
	rid, rk := rowrun.TextCell(trow, m.Column("threads", "recipient_ids"))
	if rk != rowrun.CellValue {
		raw["thread_state"] = "recipients-unknown"
		return th, nil
	}
	ids, total, unparsed := recipientIDs(rid)
	if total > 0 {
		group := total > 1
		th.Group = &group
	}
	if total > maxList {
		raw["thread_recipients_total"], raw["thread_recipients_truncated"] = int64(total), true
	}
	acol := m.Column("canonical_addresses", "address")
	var members []string
	var missing []any
	var skipped int64
	for _, cid := range ids {
		arow, ok, err := m.Get("canonical_addresses", cid)
		if errors.Is(err, sqlitedb.ErrNoSuchTable) {
			raw["thread_state"] = "no-canonical-addresses"
			return th, nil
		}
		if err != nil {
			return nil, err
		}
		var ab []byte
		ak := rowrun.CellMissing
		if ok {
			ab, ak = rowrun.TextCell(arow, acol)
		}
		if ak != rowrun.CellValue {
			missing = append(missing, cid)
			continue
		}
		cl := common.CleanText(ab, maxBody)
		if len(cl.Flags) > 0 {
			markText(raw, "thread.members")
		}
		if len(cl.Text) > maxAddr {
			skipped++
			continue
		}
		members = append(members, cl.Text)
	}
	th.Members = members
	if len(missing) > 0 {
		raw["thread_recipients_missing"] = missing
	}
	if unparsed > 0 {
		raw["thread_recipients_unparsed"] = unparsed
	}
	if skipped > 0 {
		raw["thread_members_skipped"] = skipped
	}
	return th, nil
}
