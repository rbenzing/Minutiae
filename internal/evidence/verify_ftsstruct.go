package evidence

import (
	"errors"
	"fmt"
)

// The FTS5 structure record (data row 10 of <fts>_data), pinned against the transpiled SQLite source
// of modernc.org/sqlite v1.60.1 (lib/sqlite_g_0000000000000003.go; the C names are in brackets):
//
// FTS5_AVERAGES_ROWID is 1 and FTS5_STRUCTURE_ROWID is 10 (lib/sqlite.go). A segment's rows have the
// rowid segid<<(FTS5_DATA_PAGE_B+FTS5_DATA_HEIGHT_B+FTS5_DATA_DLI_B) + height<<FTS5_DATA_PAGE_B + page,
// with 31, 5 and 1 bits, so the segment id is rowid>>37 (_fts5DataRead and its callers, e.g. line 29801).
// _fts5StructureDecode (line 29028) reads a 4-byte cookie, varint nLevel and varint nSegment (both at
// most FTS5_MAX_SEGMENT = 2000), a varint write counter, then per level a varint nMerge and a varint
// nSeg (nSeg >= nMerge) and per segment the varints segid, pgnoFirst and pgnoLast (pgnoLast >= pgnoFirst);
// the segments of all levels add up to nSegment. SQLite does not check for trailing bytes; this decoder
// does (_fts5StructureWrite, line 29341, writes exactly this and nothing after it).
//
// A "version 2" record (FTS5_STRUCTURE_V2, "\xff\x00\x00\x01" after the cookie, five more varints per
// segment) is written only by a table created with contentless_delete=1 (it sets nOriginCntr, which is
// what makes _fts5StructureWrite emit V2; such tables also keep tombstone rows under segid+65536). No
// Minutiae table uses that option (schema_v3.go: an empty content option only), so a V2 record can only come from
// tampering or a foreign table. It is not decoded: it is reported as unreadable, the right answer for
// an index this build did not create (TestFTSStructureAccountsForEveryGenuineRow runs five genuine
// workloads and finds only V1 records).
const (
	ftsAveragesRowID  = 1
	ftsStructureRowID = 10
	ftsSegidShift     = 37 // FTS5_DATA_PAGE_B + FTS5_DATA_HEIGHT_B + FTS5_DATA_DLI_B
	ftsMaxSegment     = 2000
)

var errFTSStructure = errors.New("malformed FTS5 structure record")

// decodeFTSStructure returns the segment ids the structure record names. It reads only what the bytes
// hold. A level needs at least 2 bytes (merge count, segment count) and a segment at least 3 (id and
// two page numbers), so a declared count the remaining bytes cannot hold is refused before any
// allocation, and the result map is never sized from a declared count.
func decodeFTSStructure(b []byte) (map[int64]struct{}, error) {
	segs, err := decodeFTSStructureSegments(b)
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]struct{}, len(segs)) // len(segs) counts segments actually decoded, not a declared count
	for _, s := range segs {
		ids[s.ID] = struct{}{}
	}
	return ids, nil
}

// checkUnreferenced is called once per FTS table from verifyFTS, inside the fts.compare phase and
// inside the loop over FTSTables (before the phase ends and before the loop's early return). It
// streams the data ids and the idx segment ids through one read transaction and holds nothing but
// the structure's segment set.
func (r *ftsVerify) checkUnreferenced(table string) {
	shown, hidden := 0, 0
	report := func(format string, a ...any) {
		if shown < verifyMaxPerKind {
			r.rep.problemf(format, a...)
			shown++
		} else {
			hidden++
		}
	}
	err := r.c.ReadTx(r.ctx, func(h ReadHandle) error {
		var structure []byte
		if err := h.QueryRowContext(r.ctx, `SELECT block FROM `+table+`_data WHERE id = ?`, ftsStructureRowID).Scan(&structure); err != nil {
			return fmt.Errorf("read the structure record: %w", err)
		}
		segs, err := decodeFTSStructure(structure)
		if err != nil {
			r.rep.problemf("%s: the index structure record is unreadable: %q", table, err.Error())
			return nil
		}
		rows, err := h.QueryContext(r.ctx, `SELECT id FROM `+table+`_data WHERE id NOT IN (?, ?)`, ftsAveragesRowID, ftsStructureRowID)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			seg := id >> ftsSegidShift
			if id < 1<<ftsSegidShift {
				report("%s: data row %d is below the segment range (only rows 1 and 10 are), so no structure can reference it", table, id)
			} else if _, ok := segs[seg]; !ok {
				report("%s: data row %d (segment %d) is not referenced by the index structure", table, id, seg)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		sr, err := h.QueryContext(r.ctx, `SELECT DISTINCT segid FROM `+table+`_idx`)
		if err != nil {
			return err
		}
		defer func() { _ = sr.Close() }()
		for sr.Next() {
			var s int64
			if err := sr.Scan(&s); err != nil {
				return err
			}
			if _, ok := segs[s]; !ok {
				report("%s: index row of segment %d is not referenced by the index structure", table, s)
			}
		}
		return sr.Err()
	})
	if err != nil {
		r.fail(table, err) // the end of the verify context is marked as a cut there, not reported
	}
	if hidden > 0 {
		r.rep.problemf("%s: %d further unreferenced index rows are not listed", table, hidden)
	}
}
