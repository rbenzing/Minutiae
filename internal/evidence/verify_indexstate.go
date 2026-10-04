package evidence

import "context"

// verifyIndexState reports the state of the full-text index (schema v3 and newer), per the plan's
// index-state table. A current index is silent here (the comparison of its content is a separate
// check); an unbuilt, interrupted or foreign-version index is a NOTICE that names the remedy,
// because the case is intact and only unsearchable; an unreadable or invalid state is a problem,
// because the case cannot say what its index is.
func (c *Case) verifyIndexState(ctx context.Context, rep *VerifyReport) {
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
		return
	}
	remedy := "run: minutiae records reindex --case " + c.Dir
	switch st.Kind {
	case IndexCurrent:
	case IndexUnbuilt:
		rep.noticef("%s: the full-text index is not built: %d records are not searchable and the index content is not verified; %s", FTSWordTable, records, remedy)
	case IndexBuilding:
		rep.noticef("%s: a records reindex was interrupted (the index is %q): the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, remedy)
	case IndexStale:
		rep.noticef("%s: the full-text index was built by %q and this build is %q: the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, st.Current, remedy)
	default:
		if st.Value == "" {
			rep.problemf("%s: records_meta %s is missing (or is not text): the state of the full-text index is unknown; %s", FTSWordTable, MetaFTSNormVersion, remedy)
			return
		}
		rep.problemf("%s: records_meta %s holds %q, which is not a version of the full-text index; %s", FTSWordTable, MetaFTSNormVersion, st.Value, remedy)
	}
}
