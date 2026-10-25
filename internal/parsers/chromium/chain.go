package chromium

import (
	"slices"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// The caps of a download's URL chain (4F-R11, 4F-R26).
const (
	maxList       = 1000    // entries of a list kept
	maxChainBytes = 2 << 20 // cumulative bytes of the kept chain entries (each already cut at maxURL)
	maxBodyTotal  = 1 << 20 // bytes of a download's searchable body, below the records cap
)

// chain is the URL chain of one download.
type chain struct {
	urls      []string // the kept entries in chain_index order (ties by rowid)
	final     string   // the last entry of the whole ordered chain, cleaned: the URL the download was fetched from
	undecided bool     // the engine could not answer: nothing is stated about the chain
}

// chainEntry is one downloads_url_chains row of the download.
type chainEntry struct {
	index, rowid int64
	url          []byte // aliases the reader's memory until the mapping returns
}

// chainOf reads the downloads_url_chains rows of download id through one Match (charged to the budget by the
// Context, so a hostile chain fails the job with parse.ErrBudget and never allocates unboundedly), orders them by
// chain_index and then rowid, and applies the caps after sorting: the first maxList entries and at most
// maxChainBytes of cleaned URL bytes are kept. raw gets url_chain_state ("no-chain-rows", "gaps", "duplicate-index",
// "unknown-index", "unknown-url") and the cap markers. A failed or short answer other than a budget or a context
// error leaves the chain undecided (4F-R22 as amended by 4F-R30).
func chainOf(m parse.MapContext, id int64, raw map[string]any) (chain, error) {
	rows, undecided, err := rowrun.Match(m, "downloads_url_chains", "id", id)
	switch {
	case err != nil && fatal(err):
		return chain{}, err
	case err != nil || undecided:
		return chain{undecided: true}, nil
	case len(rows) == 0:
		raw["url_chain_state"] = "no-chain-rows"
		return chain{}, nil
	}
	cols := rowrun.Resolve(m, "downloads_url_chains", "chain_index", "url")
	entries := make([]chainEntry, 0, len(rows))
	for _, r := range rows {
		idx, ik := rowrun.IntCell(r, cols.At("chain_index"))
		if ik != rowrun.CellValue {
			raw["url_chain_state"] = "unknown-index" // the order of the chain cannot be decided
			return chain{}, nil
		}
		u, uk := rowrun.TextCell(r, cols.At("url"))
		if uk != rowrun.CellValue || len(u) == 0 {
			raw["url_chain_state"] = "unknown-url" // an entry that states no URL: the chain cannot be listed whole
			return chain{}, nil
		}
		rid, _ := r.Rowid()
		entries = append(entries, chainEntry{index: idx, rowid: rid, url: u})
	}
	slices.SortStableFunc(entries, func(a, b chainEntry) int {
		switch {
		case a.index != b.index:
			return signOf(a.index < b.index)
		case a.rowid != b.rowid:
			return signOf(a.rowid < b.rowid)
		}
		return 0
	})
	switch {
	case hasRepeat(entries):
		raw["url_chain_state"] = "duplicate-index"
	case isGapped(entries):
		raw["url_chain_state"] = "gaps"
	}

	var out chain
	var total int64
	for _, e := range entries {
		total += int64(len(e.url))
	}
	keep := entries[:min(len(entries), maxList)]
	used, byteCut := 0, false
	for _, e := range keep {
		cl := common.CleanText(e.url, maxURL)
		if used+len(cl.Text) > maxChainBytes {
			byteCut = true
			break
		}
		used += len(cl.Text)
		out.urls = append(out.urls, noteCleaned(raw, "url_chain", cl))
	}
	last := entries[len(entries)-1]
	if len(out.urls) == len(entries) {
		out.final = out.urls[len(out.urls)-1]
	} else {
		out.final = capped(raw, "url_chain", last.url, maxURL)
	}
	if len(entries) > maxList {
		raw["url_chain_truncated"] = true
	}
	if byteCut {
		raw["url_chain_bytes_truncated"] = true
		raw["url_chain_bytes_total"] = total
	}
	if len(entries) > maxList || byteCut {
		raw["url_chain_total"] = int64(len(entries))
	}
	return out, nil
}

// signOf is the comparison result for a strict "less": -1 when less, else 1 (callers have excluded equality).
func signOf(less bool) int {
	if less {
		return -1
	}
	return 1
}

func hasRepeat(es []chainEntry) bool {
	for i := 1; i < len(es); i++ {
		if es[i].index == es[i-1].index {
			return true
		}
	}
	return false
}

// isGapped says the indexes are not exactly 0..n-1.
func isGapped(es []chainEntry) bool {
	for i, e := range es {
		if e.index != int64(i) {
			return true
		}
	}
	return false
}
