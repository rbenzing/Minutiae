package evidence

import (
	"context"
	"slices"
)

// verifyIndexState reports the state of the full-text index (schema v3 and newer), per the plan's
// index-state table. A current index is silent here (the comparison of its content is a separate
// check); an unbuilt, interrupted or foreign-version index is a NOTICE that names the remedy,
// because the case is intact and only unsearchable; an unreadable or invalid state is a problem,
// because the case cannot say what its index is. A records reindex that the audit log announced and
// never concluded is a NOTICE too, whatever the state says: the index may be partial.
func (c *Case) verifyIndexState(ctx context.Context, rep *VerifyReport, entries []AuditEntry, auditReadable bool) (current bool) {
	remedy := "run: minutiae records reindex --case " + c.Dir
	if auditReadable {
		for _, d := range danglingReindexes(entries, rep) {
			rep.noticef("%s: audit seq %d: records reindex %q was announced and never concluded (the index may be partial and its content is not verified); %s", FTSWordTable, d.seq, d.id, remedy)
		}
	}
	var st IndexState
	var records int64
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		var err error
		if st, err = ReadIndexState(ctx, h); err != nil {
			return err
		}
		if st.Kind == IndexUnbuilt {
			return h.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&records)
		}
		return nil
	})
	if err != nil {
		unreadable(rep, "full-text index state", err)
		return false
	}
	switch st.Kind {
	case IndexCurrent:
		return true
	case IndexUnbuilt:
		rep.noticef("%s: the full-text index is not built: %d records are not searchable and the index content is not verified; %s", FTSWordTable, records, remedy)
	case IndexBuilding:
		rep.noticef("%s: a records reindex was interrupted (the index is %q): the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, remedy)
	case IndexStale:
		rep.noticef("%s: the full-text index was built by %q and this build is %q: the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, st.Current, remedy)
	default:
		if st.Value == "" {
			rep.problemf("%s: records_meta %s is missing (or is not text): the state of the full-text index is unknown; %s", FTSWordTable, MetaFTSNormVersion, remedy)
			return false
		}
		rep.problemf("%s: records_meta %s holds %q, which is not a version of the full-text index; %s", FTSWordTable, MetaFTSNormVersion, st.Value, remedy)
	}
	return false
}

// danglingReindex is a records.reindex entry no later entry concluded.
type danglingReindex struct {
	id  string
	seq int64
}

// danglingReindexes returns the records.reindex entries the audit log announced and never
// concluded, in log order. A records.reindex.error with the same reindex id concludes one; a
// records.reindex.done concludes the reindex it names and every reindex announced before it, because
// a finished rebuild supersedes any earlier one (a reindex that was cut short is concluded by the next
// one that completes). A done or error that names no open announcement is a problem and concludes
// nothing, as is an entry whose details cannot be decoded.
func danglingReindexes(entries []AuditEntry, rep *VerifyReport) []danglingReindex {
	var open []danglingReindex
	for _, e := range entries {
		switch e.Action {
		case ActionReindex:
			s, err := DecodeDetails[ReindexStart](e.Details)
			if err != nil {
				rep.problemf("audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			open = append(open, danglingReindex{id: s.ReindexID, seq: e.Seq})
		case ActionReindexError:
			f, err := DecodeDetails[ReindexFailure](e.Details)
			if err != nil {
				rep.problemf("audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			i := slices.IndexFunc(open, func(d danglingReindex) bool { return d.id == f.ReindexID })
			if i < 0 {
				orphanReindexConclusion(rep, e, f.ReindexID)
				continue
			}
			open = slices.Delete(open, i, i+1)
		case ActionReindexDone:
			d, err := DecodeDetails[ReindexDone](e.Details)
			if err != nil {
				rep.problemf("audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			i := slices.IndexFunc(open, func(o danglingReindex) bool { return o.id == d.ReindexID })
			if i < 0 {
				orphanReindexConclusion(rep, e, d.ReindexID)
				continue
			}
			open = open[i+1:] // the named reindex and every one announced before it
		}
	}
	return open
}

// orphanReindexConclusion reports a records.reindex.done or .error that names no open announcement:
// the chain claims the conclusion of work it never announced (or concluded twice), which only a writer
// of the chain can produce. It concludes nothing.
func orphanReindexConclusion(rep *VerifyReport, e AuditEntry, id string) {
	rep.problemf("audit seq %d: %s names reindex %q, which no earlier records.reindex announced, or which was already concluded", e.Seq, e.Action, id)
}
