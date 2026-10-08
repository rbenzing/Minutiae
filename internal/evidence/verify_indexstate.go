package evidence

import (
	"context"
	"fmt"
	"slices"
)

// verifyIndexState reports the state of the full-text index (schema v3 and newer), per the plan's
// index-state table. A current index is silent here (the comparison of its content is a separate
// check); an unbuilt, interrupted or foreign-version index is a NOTICE that names the remedy,
// because the case is intact and only unsearchable; an unreadable or invalid state is a problem,
// because the case cannot say what its index is. A records reindex that the audit log announced and
// never concluded is a NOTICE too, whatever the state says: the index may be partial.
func (c *Case) verifyIndexState(ctx context.Context, rep *VerifyReport, ps *problemSet, entries []AuditEntry, auditReadable bool) (current bool) {
	remedy := "run: minutiae records reindex --case " + c.Dir
	var derived derivedIndex
	dangling := false
	if auditReadable {
		derived = deriveIndexState(entries, ps)
		for _, d := range danglingReindexes(entries, rep) {
			dangling = true
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
	// the content is compared whenever the audit log says this build built the index, whatever
	// records_meta says; the state itself is compared with what the log derives
	// an announced reindex that dropped rows leaves a partial index (state "building", explained by the log): nothing to compare
	compare := !dangling && (st.Kind != IndexBuilding || !derived.explains(st.Value))
	if derived.known {
		compare = compare && derived.current(st.Current)
	} else {
		compare = compare && st.Kind == IndexCurrent
	}
	if st.Kind != IndexInvalid && !derived.explains(st.Value) {
		rep.problemf("%s: the index state differs from the audit log: records_meta %s holds %q, the audit log derives %q; %s", FTSWordTable, MetaFTSNormVersion, st.Value, derived.allowed[0], remedy)
		return compare
	}
	switch st.Kind {
	case IndexCurrent:
		return compare
	case IndexUnbuilt:
		rep.noticef("%s: the full-text index is not built: %d records are not searchable and the index content is not verified; %s", FTSWordTable, records, remedy)
	case IndexBuilding:
		rep.noticef("%s: a records reindex was interrupted (the index is %q): the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, remedy)
	case IndexStale:
		rep.noticef("%s: the full-text index was built by %q and this build is %q: the records are not searchable and the index content is not verified; %s", FTSWordTable, st.Value, st.Current, remedy)
	default:
		if st.Value == "" {
			rep.problemf("%s: records_meta %s is missing (or is not text): the state of the full-text index is unknown%s; %s", FTSWordTable, MetaFTSNormVersion, derived.differs(), remedy)
			return compare
		}
		rep.problemf("%s: records_meta %s holds %q, which is not a version of the full-text index%s; %s", FTSWordTable, MetaFTSNormVersion, st.Value, derived.differs(), remedy)
	}
	return compare
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

// derivedIndex is the index state the audit log accounts for (R57): verify recomputes it, it does not
// trust records_meta. known is false while the log says nothing about the version the index was built
// with (a case created at v3 or upgraded, with no ingest and no reindex since): then records_meta is
// unconstrained. Otherwise allowed[0] is the version the last records.reindex.done or ingest stated,
// and the rest are the values an announced, unconcluded reindex explains ("building", the version it
// was building).
type derivedIndex struct {
	known   bool
	allowed []string
}

func (d derivedIndex) explains(v string) bool { return !d.known || slices.Contains(d.allowed, v) }

// current reports whether the audit log says the index was built by this build's pipeline version.
func (d derivedIndex) current(version string) bool { return d.known && d.allowed[0] == version }

// deriveIndexState replays the audit log: a records.reindex.done states the version, a schema upgrade
// to v3 forgets it (the migration leaves the index unbuilt, or current when there was nothing to
// index), a records.ingest.start states the version it indexed with and must agree with what the log
// derived so far (a mismatch is a problem, named by audit seq), and an announced reindex allows the
// states it passes through.
func deriveIndexState(entries []AuditEntry, ps *problemSet) derivedIndex {
	var d derivedIndex
	for _, e := range entries {
		switch e.Action {
		case ActionCaseUpgrade:
			from, okF := auditInt(e.Details, "from")
			to, okT := auditInt(e.Details, "to")
			if okF && okT && from < 3 && to >= 3 {
				d = derivedIndex{}
			}
		case ActionReindex:
			if s, err := DecodeDetails[ReindexStart](e.Details); err == nil && d.known {
				d.allowed = append(d.allowed, indexBuildingValue, s.NormVersion)
			}
		case ActionReindexDone:
			if r, err := DecodeDetails[ReindexDone](e.Details); err == nil {
				d = derivedIndex{known: true, allowed: []string{r.NormVersion}}
			}
		case ActionIngestStart:
			s, err := DecodeDetails[IngestStart](e.Details)
			if err != nil || s.NormVersion == "" { // unreadable details are reported elsewhere; a v2 ingest has no index
				continue
			}
			if d.known && !slices.Contains(d.allowed, s.NormVersion) {
				ps.add("index-state", "%s: audit seq %d: records.ingest.start %q norm_version %q differs from the index version the audit log derives (%q)",
					FTSWordTable, e.Seq, s.IngestID, s.NormVersion, d.allowed[0])
				continue
			}
			d = derivedIndex{known: true, allowed: []string{s.NormVersion}}
		}
	}
	return d
}

// differs is the clause an unreadable or invalid state adds when the audit log says what it should be.
func (d derivedIndex) differs() string {
	if !d.known {
		return ""
	}
	return fmt.Sprintf(": the index state differs from the audit log, which derives %q", d.allowed[0])
}
