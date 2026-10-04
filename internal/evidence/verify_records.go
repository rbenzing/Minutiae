package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Record verification (schema v2): triggers, quick_check, the audited batches
// against the stored rows, and the row digests. The audit log is the authority:
// every records.batch entry commits to (first_id, count, digest) before its rows
// are written, and the digest is recomputed here from the stored rows, their
// times, the parser and the SHA-256 of the artifact the manifest holds.

const (
	// verifyChunkRows is the number of record rows read per ReadTx. The rows are
	// streamed in keyset chunks (WHERE id > ? ORDER BY id LIMIT n), never with a
	// cursor held across calls (the database has one connection).
	verifyChunkRows = 5000
	// verifyChunkBytes ends a chunk early when its rows are this large, so a
	// hostile database of huge rows cannot make one chunk unbounded.
	verifyChunkBytes = 64 << 20
	// verifyMaxPerKind is how many problems of one kind are listed; the rest are
	// counted in one line.
	verifyMaxPerKind = 50
)

// verifyChunkObserver is told about every chunk query of the record rows (table
// "records") with the number of rows it returned; only tests pass one (Case.verify).
type verifyChunkObserver func(table string, rows int)

// problemSet lists at most verifyMaxPerKind problems per kind and then counts
// the rest, so a damaged table of millions of rows cannot flood the report.
type problemSet struct {
	rep    *VerifyReport
	counts map[string]int
}

func (p *problemSet) add(kind, format string, a ...any) {
	p.counts[kind]++
	if p.counts[kind] <= verifyMaxPerKind {
		p.rep.problemf(format, a...)
	}
}

func (p *problemSet) flush() {
	kinds := make([]string, 0, len(p.counts))
	for k, n := range p.counts {
		if n > verifyMaxPerKind {
			kinds = append(kinds, k)
		}
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		p.rep.problemf("%d further %s problems are not listed", p.counts[k]-verifyMaxPerKind, k)
	}
}

// batchKey identifies an announced batch.
type batchKey struct {
	ingest string
	no     int
}

// auditedBatch is a records.batch audit entry.
type auditedBatch struct {
	Seq int64
	BatchCommit
}

func (b auditedBatch) lastID() int64 { return b.FirstID + int64(b.Count) - 1 }

// recordAudit is what the audit log says about records.
type recordAudit struct {
	batches   []auditedBatch // every valid records.batch entry, in log order
	byKey     map[batchKey]int
	batchErr  map[batchKey]bool // announced batches explained by records.batch.error
	recovered map[batchKey]bool // announced batches a records.ingest.recover says never reached the database
	parsers   map[[3]string]bool

	// the ingest lifecycle entries (P6): every start (first one wins, a repeat is
	// flagged), the conclusions and the recovers per ingest, in log order
	starts     map[string]IngestStart
	startSeq   map[string]int64
	startOrder []string
	dupStart   map[string]bool
	ends       map[string][]auditedConclusion
	recovers   map[string][]auditedRecover
}

// readRecordAudit decodes the records.* entries; entries that cannot be decoded
// or hold an impossible range are problems.
func readRecordAudit(entries []AuditEntry, ps *problemSet) *recordAudit {
	a := &recordAudit{
		starts: map[string]IngestStart{}, byKey: map[batchKey]int{}, batchErr: map[batchKey]bool{},
		recovered: map[batchKey]bool{}, parsers: map[[3]string]bool{},
		startSeq: map[string]int64{}, dupStart: map[string]bool{},
		ends: map[string][]auditedConclusion{}, recovers: map[string][]auditedRecover{},
	}
	for _, e := range entries {
		switch e.Action {
		case ActionIngestStart:
			s, err := DecodeDetails[IngestStart](e.Details)
			if err != nil {
				ps.add("audit", "audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			if _, dup := a.starts[s.IngestID]; !dup {
				a.starts[s.IngestID] = s
				a.startSeq[s.IngestID] = e.Seq
				a.startOrder = append(a.startOrder, s.IngestID)
			} else {
				a.dupStart[s.IngestID] = true
			}
			a.parsers[[3]string{s.Parser, s.ParserVersion, s.ParserHash}] = true
		case ActionIngestEnd, ActionIngestError:
			cn, err := DecodeDetails[IngestConclusion](e.Details)
			if err != nil {
				ps.add("audit", "audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			a.ends[cn.IngestID] = append(a.ends[cn.IngestID], auditedConclusion{Seq: e.Seq, Action: e.Action, Time: e.Time, IngestConclusion: cn})
		case ActionBatch:
			b, err := DecodeDetails[BatchCommit](e.Details)
			if err != nil {
				ps.add("audit", "audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			if b.FirstID < 1 || b.Count < 1 || b.FirstID > math.MaxInt64-int64(b.Count) {
				ps.add("audit", "audit seq %d: batch %d of ingest %q announces the id range %d+%d, which is not valid", e.Seq, b.BatchNo, b.IngestID, b.FirstID, b.Count)
				continue
			}
			k := batchKey{b.IngestID, b.BatchNo}
			if _, dup := a.byKey[k]; dup {
				ps.add("audit", "audit seq %d: batch %d of ingest %q is announced more than once", e.Seq, b.BatchNo, b.IngestID)
				continue
			}
			a.byKey[k] = len(a.batches)
			a.batches = append(a.batches, auditedBatch{Seq: e.Seq, BatchCommit: b})
		case ActionBatchError:
			b, err := DecodeDetails[BatchFailure](e.Details)
			if err != nil {
				ps.add("audit", "audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			a.batchErr[batchKey{b.IngestID, b.BatchNo}] = true
		case ActionIngestRecover:
			r, err := DecodeDetails[IngestRecover](e.Details)
			if err != nil {
				ps.add("audit", "audit seq %d: %s details unreadable: %q", e.Seq, e.Action, err.Error())
				continue
			}
			a.recovers[r.IngestID] = append(a.recovers[r.IngestID], auditedRecover{Seq: e.Seq, Time: e.Time, IngestRecover: r})
			for _, n := range r.BatchNos {
				a.recovered[batchKey{r.IngestID, n}] = true
			}
		}
	}
	return a
}

// checkOverlaps reports audited id ranges that overlap (ids are never reused,
// so two batches, written or not, never share an id) and returns the batches
// sorted by first id.
func (a *recordAudit) checkOverlaps(ps *problemSet) []auditedBatch {
	sorted := append([]auditedBatch(nil), a.batches...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].FirstID < sorted[j].FirstID })
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if cur.FirstID <= prev.lastID() {
			ps.add("overlap", "audited batch ranges overlap: batch %d of ingest %q (ids %d..%d) overlaps batch %d of ingest %q (ids %d..%d)",
				prev.BatchNo, prev.IngestID, prev.FirstID, prev.lastID(), cur.BatchNo, cur.IngestID, cur.FirstID, cur.lastID())
		}
	}
	return sorted
}

// find returns the index in sorted of the audited batch whose range holds id.
func findBatch(sorted []auditedBatch, id int64) (int, bool) {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i].FirstID > id })
	if i == 0 {
		return 0, false
	}
	if id <= sorted[i-1].lastID() {
		return i - 1, true
	}
	return 0, false
}

// storedBatch is a record_batches row.
type storedBatch struct {
	id      int64
	ingest  string
	no      int
	first   int64
	count   int64
	digest  string
	created string
}

// batchAcc accumulates the digest of one audited batch from its stored rows.
type batchAcc struct {
	digest *BatchDigest
	n      int64
}

// recordRowMeta is what a row carries beside what the digest sees.
type recordRowMeta struct {
	batchID  int64
	parserID int64
	hasParse bool
	sig      string // typeof() of every column of the row, comma separated
}

func unreadable(rep *VerifyReport, what string, err error) {
	rep.problemf("records unreadable: %s: %q", what, err.Error())
}

// verifyRecords is the record part of Verify (P1 to P5, P10, P12 and the digests).
// It does nothing for a schema older than v2. Every failure to read part of the
// record tables is a problem; it never aborts. entries is nil when the audit log
// could not be read, in which case the checks that need it are skipped.
func (c *Case) verifyRecords(rep *VerifyReport, recs []ManifestRecord, entries []AuditEntry, auditReadable bool, observe verifyChunkObserver) {
	dbv, err := c.store.SchemaVersion()
	if err != nil {
		rep.problemf("records unreadable: schema version: %q", err.Error())
		return
	}
	ctx := context.Background()
	ps := &problemSet{rep: rep, counts: map[string]int{}}
	defer ps.flush()

	// the schema objects of every schema this build knows (a v1 case too: tables or
	// triggers planted in it are not harmless)
	if dbv >= 1 && dbv <= CurrentSchema {
		c.verifySchemaObjects(ctx, ps, dbv)
	}
	if dbv < 2 {
		return
	}
	c.verifyIntegrityCheck(ctx, rep)

	var audit *recordAudit
	var sorted []auditedBatch
	if auditReadable {
		audit = readRecordAudit(entries, ps)
		sorted = audit.checkOverlaps(ps)
	} else {
		rep.problemf("records: the audit log is unreadable, so the record batches cannot be checked against it")
	}

	manifest := make(map[string]ManifestRecord, len(recs))
	for _, r := range recs {
		if _, dup := manifest[r.ID]; !dup {
			manifest[r.ID] = r
		}
	}
	dbArtifacts, dbErr := c.store.ArtifactHashes()
	if dbErr != nil {
		unreadable(rep, "artifacts", dbErr)
	}

	batches, ok := c.readStoredBatches(ctx, rep, ps)
	if ok {
		for _, b := range batches {
			if b.id <= 0 {
				ps.add("nonpositive-id", "record batch %d of ingest %q: batch id %d is not positive", b.no, b.ingest, b.id)
			}
		}
		c.checkTableCount(ctx, rep, "record_batches", int64(len(batches)))
	}
	byBatchID := make(map[int64]storedBatch, len(batches))
	byKey := make(map[batchKey]storedBatch, len(batches))
	for _, b := range batches {
		byBatchID[b.id] = b
		k := batchKey{b.ingest, b.no}
		if _, dup := byKey[k]; !dup {
			byKey[k] = b
		}
	}

	parserOK := c.verifyParsers(ctx, rep, ps, audit)
	// the tables no scan below reads row by row for another purpose
	c.verifyClassOnly(ctx, ps, metaClasses, []string{"key"}, func(v []string) string { return fmt.Sprintf("key %q", v[0]) })
	c.verifyClassOnly(ctx, ps, supersededClasses, []string{"ingest_id", "artifact_id"},
		func(v []string) string { return fmt.Sprintf("ingest %q artifact %q", v[0], v[1]) })

	acc := make([]*batchAcc, len(sorted))
	seenArtifact := map[string]bool{}
	var maxID int64
	streamed := c.streamRecords(ctx, ps, observe, func(row RecordRow, m recordRowMeta) {
		rep.RecordsChecked++
		maxID = max(maxID, row.ID)
		if !seenArtifact[row.ArtifactID] {
			seenArtifact[row.ArtifactID] = true
			if _, ok := manifest[row.ArtifactID]; !ok {
				ps.add("artifact", "record %d: artifact %q is not in the manifest", row.ID, row.ArtifactID)
			}
			if dbErr == nil {
				if _, ok := dbArtifacts[row.ArtifactID]; !ok {
					ps.add("artifact", "record %d: artifact %q is not in artifacts.db", row.ID, row.ArtifactID)
				}
			}
		}
		if ok {
			if _, found := byBatchID[m.batchID]; !found {
				ps.add("batch-id", "record %d: batch id %d does not exist in record_batches", row.ID, m.batchID)
			}
		}
		if !m.hasParse && parserOK {
			ps.add("parser", "record %d: parser id %d does not exist in parsers", row.ID, m.parserID)
		}
		if mr, ok := manifest[row.ArtifactID]; ok && row.SrcOffset != nil && row.SrcLength != nil {
			// P9, with checked math: off+n is never computed
			if off, n := *row.SrcOffset, *row.SrcLength; off < 0 || n < 0 || off > mr.Size || n > mr.Size-off {
				ps.add("src-range", "record %d: range beyond artifact: bytes %d+%d of artifact %q, which holds %d bytes", row.ID, off, n, row.ArtifactID, mr.Size)
			}
		}
		if !auditReadable {
			return
		}
		i, in := findBatch(sorted, row.ID)
		if !in {
			ps.add("range", "record %d is outside every batch range the audit log announced", row.ID)
			return
		}
		ab := sorted[i]
		if sb, found := byKey[batchKey{ab.IngestID, ab.BatchNo}]; found && sb.id != m.batchID {
			ps.add("batch-id", "record %d: batch id %d is not the batch that announced its id (batch %d of ingest %q has batch id %d)",
				row.ID, m.batchID, ab.BatchNo, ab.IngestID, sb.id)
		}
		if acc[i] == nil {
			acc[i] = &batchAcc{digest: NewBatchDigest()}
		}
		sha := ""
		if mr, ok := manifest[row.ArtifactID]; ok {
			sha = mr.SHA256
		}
		acc[i].digest.Add(RowDigest(row, sha))
		acc[i].n++
	})

	if auditReadable && streamed {
		c.checkBatches(rep, ps, audit, sorted, acc, byKey, ok)
	}
	if auditReadable {
		c.verifyLifecycle(ctx, rep, ps, audit, manifest, byKey, ok)
	}
	if ok {
		announced := map[batchKey]bool{}
		if auditReadable {
			for _, b := range audit.batches {
				announced[batchKey{b.IngestID, b.BatchNo}] = true
			}
			for _, b := range batches {
				if !announced[batchKey{b.ingest, b.no}] {
					ps.add("announce", "record batch %d of ingest %q (batch id %d, ids %d+%d) is not announced by any records.batch audit entry",
						b.no, b.ingest, b.id, b.first, b.count)
				}
			}
		}
	}
	if streamed {
		c.checkNextID(ctx, rep, maxID)
	}
}

// checkBatches compares every audited batch with its stored row and its rows.
func (c *Case) checkBatches(rep *VerifyReport, ps *problemSet, audit *recordAudit, sorted []auditedBatch, acc []*batchAcc, byKey map[batchKey]storedBatch, batchesRead bool) {
	pending := map[string]bool{}
	if un, err := c.UnresolvedIngests(); err != nil {
		unreadable(rep, "unresolved ingests", err)
	} else {
		for _, u := range un {
			pending[u.Start.IngestID] = true
		}
	}
	for i, ab := range sorted {
		k := batchKey{ab.IngestID, ab.BatchNo}
		var stored int64
		if acc[i] != nil {
			stored = acc[i].n
		}
		sb, have := byKey[k]
		switch {
		case !batchesRead:
			// record_batches is unreadable: already reported
		case !have:
			explained := audit.batchErr[k] || audit.recovered[k]
			switch {
			case stored == 0 && explained:
				rep.noticef("batch %d of ingest %q (ids %d..%d) was audited but its rows were never written (a records.batch.error or records.ingest.recover entry explains it)",
					ab.BatchNo, ab.IngestID, ab.FirstID, ab.lastID())
				continue
			case stored == 0 && pending[ab.IngestID]:
				rep.noticef("batch %d of ingest %q (ids %d..%d) was audited but its rows are not in the database; the ingest never concluded and the next Start recovers it",
					ab.BatchNo, ab.IngestID, ab.FirstID, ab.lastID())
				continue
			case explained:
			default:
				ps.add("batch-row", "batch row missing: batch %d of ingest %q (ids %d..%d) is announced by the audit log but has no record_batches row",
					ab.BatchNo, ab.IngestID, ab.FirstID, ab.lastID())
				if stored == 0 {
					continue
				}
			}
		default:
			if sb.first != ab.FirstID || sb.count != int64(ab.Count) || sb.digest != ab.Digest || sb.created != ab.Created {
				ps.add("batch-row", "record_batches row of batch %d of ingest %q differs from the audit log: stored first_id %d count %d digest %q created %q, audited first_id %d count %d digest %q created %q",
					ab.BatchNo, ab.IngestID, sb.first, sb.count, sb.digest, sb.created, ab.FirstID, ab.Count, ab.Digest, ab.Created)
			}
		}
		if stored != int64(ab.Count) {
			ps.add("count", "records stored: batch %d of ingest %q (ids %d..%d) holds %d records, the audit log announced %d",
				ab.BatchNo, ab.IngestID, ab.FirstID, ab.lastID(), stored, ab.Count)
		}
		got := NewBatchDigest().Sum()
		if acc[i] != nil {
			got = acc[i].digest.Sum()
		}
		rep.RecordBatchesChecked++
		if got != ab.Digest {
			ps.add("digest", "digest mismatch: batch %d of ingest %q (ids %d..%d): the stored rows hash to %q, the audit log committed to %q",
				ab.BatchNo, ab.IngestID, ab.FirstID, ab.lastID(), got, ab.Digest)
		}
	}
}

// verifySchemaObjects (P12) requires the whole of sqlite_master to be the schema this
// build defines for the database's version: every table, index and trigger present
// with exactly the expected definition (whitespace aside; for the 16 immutability
// triggers that is the original P12), and no other table, index, trigger or view.
func (c *Case) verifySchemaObjects(ctx context.Context, ps *problemSet, version int) {
	expected, err := expectedSchema(version)
	if err != nil {
		unreadable(ps.rep, "expected schema", err)
		return
	}
	var found map[string]schemaObject
	err = c.ReadTx(ctx, func(h ReadHandle) error {
		var err error
		found, err = readSchemaObjects(ctx, h)
		return err
	})
	if err != nil {
		unreadable(ps.rep, "schema objects", err)
		return
	}
	for _, d := range schemaDifferences(expected, found) {
		ps.add("schema", "%s", d)
	}
}

func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// verifyIntegrityCheck (P10) runs SQLite's integrity_check; anything but "ok" is a
// problem. Unlike quick_check it verifies every index against its table, so a
// reader-used index that no longer agrees with the rows (page-level tampering that
// leaves the schema text intact) is found; on a STRICT table both report a value of
// the wrong storage class. SQLite stops after 100 errors and at most 20 messages
// are kept. Its cost, a full scan of every table and index, belongs to plan 5D.
func (c *Case) verifyIntegrityCheck(ctx context.Context, rep *VerifyReport) {
	var msgs []string
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		rows, err := h.QueryContext(ctx, `SELECT integrity_check FROM pragma_integrity_check`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				return err
			}
			if len(msgs) < 20 {
				msgs = append(msgs, m)
			}
		}
		return rows.Err()
	})
	if err != nil {
		unreadable(rep, "integrity_check", err)
		return
	}
	if len(msgs) == 1 && msgs[0] == "ok" {
		return
	}
	for _, m := range msgs {
		rep.problemf("integrity_check failed: %q", m)
	}
}

// readStoredBatches reads every record_batches row (keyset chunks).
func (c *Case) readStoredBatches(ctx context.Context, rep *VerifyReport, ps *problemSet) ([]storedBatch, bool) {
	var out []storedBatch
	after := int64(math.MinInt64) // keyset scans start below every possible id (ids may be 0 or negative in a tampered db)
	for {
		var chunk []storedBatch
		var sigs []string
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			chunk, sigs = nil, nil
			rows, err := h.QueryContext(ctx, batchesSelect, after, verifyChunkRows)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var b storedBatch
				var sig string
				if err := rows.Scan(&b.id, &b.ingest, &b.no, &b.first, &b.count, &b.digest, &b.created, &sig); err != nil {
					return err
				}
				chunk = append(chunk, b)
				sigs = append(sigs, sig)
			}
			return rows.Err()
		})
		if err != nil {
			unreadable(rep, "record_batches", err)
			return out, false
		}
		for i, b := range chunk {
			batchesClasses.check(ps, sigs[i], func() string { return fmt.Sprintf("ingest %q batch %d", b.ingest, b.no) })
		}
		out = append(out, chunk...)
		if len(chunk) < verifyChunkRows {
			return out, true
		}
		last := chunk[len(chunk)-1].id
		if last < after {
			ps.add("scan", "scan of record_batches cannot make progress: the chunk after batch id %d ended at %d", after, last)
			return out, false
		}
		if after = last; after == math.MaxInt64 {
			return out, true
		}
		after++
	}
}

var batchesSelect = `SELECT batch_id, ingest_id, batch_no, first_id, count, digest, created, ` + batchesClasses.signature("") + `
	FROM record_batches WHERE batch_id >= ? ORDER BY batch_id LIMIT ?`

// verifyParsers (P2) requires every parsers row to be named by a
// records.ingest.start entry. It returns whether the parsers table was readable.
func (c *Case) verifyParsers(ctx context.Context, rep *VerifyReport, ps *problemSet, audit *recordAudit) bool {
	type parserRow struct {
		id            int64
		name, version string
		hash          sql.NullString
		sig           string
	}
	var rows []parserRow
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		rs, err := h.QueryContext(ctx, `SELECT id, name, version, hash, `+parsersClasses.signature("")+` FROM parsers ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rs.Close() }()
		for rs.Next() {
			var p parserRow
			if err := rs.Scan(&p.id, &p.name, &p.version, &p.hash, &p.sig); err != nil {
				return err
			}
			rows = append(rows, p)
		}
		return rs.Err()
	})
	if err != nil {
		unreadable(rep, "parsers", err)
		return false
	}
	for _, p := range rows {
		parsersClasses.check(ps, p.sig, func() string { return fmt.Sprintf("id %d", p.id) })
		if p.id <= 0 {
			ps.add("nonpositive-id", "parser %q %q: id %d is not positive", p.name, p.version, p.id)
		}
	}
	if audit == nil {
		return true
	}
	for _, p := range rows {
		if !audit.parsers[[3]string{p.name, p.version, p.hash.String}] {
			ps.add("parser-named", "parser %q %q (id %d) is not named by any records.ingest.start audit entry", p.name, p.version, p.id)
		}
	}
	return true
}

// recordsSelect reads one chunk of rows; the parser is a LEFT JOIN so a row whose
// parser row is gone is seen (and reported), not silently dropped.
//
// The last column is the typeof() signature of the row's own columns, checked
// against the schema's storage classes (verify_class.go).
var recordsSelect = `SELECT r.id, r.batch_id, r.parser_id, r.type, r.payload_v, r.artifact_id, r.source_path, r.locator,
	r.src_offset, r.src_length, r.ts, r.ts_end, r.ts_basis, r.tz_offset_min, r.deleted, r.recovered, r.recovery_method,
	r.confidence, p.name, p.version, p.hash, r.summary, r.body, r.payload, ` + recordsClasses.signature("r.") + `
	FROM records r LEFT JOIN parsers p ON p.id = r.parser_id
	WHERE r.id >= ? ORDER BY r.id LIMIT ?`

var (
	timesSelectAll = `SELECT record_id, kind, ts, ts_basis, tz_offset_min, ` + timesClasses.signature("") +
		` FROM record_times WHERE record_id >= ? ORDER BY record_id, kind`
	timesSelectRange = `SELECT record_id, kind, ts, ts_basis, tz_offset_min, ` + timesClasses.signature("") +
		` FROM record_times WHERE record_id >= ? AND record_id <= ? ORDER BY record_id, kind`
)

// streamRecords reads every record row in keyset chunks, each in its own ReadTx
// together with the record_times of that id range, and calls fn for each row in
// id order. It reports whether every chunk could be read.
func (c *Case) streamRecords(ctx context.Context, ps *problemSet, observe verifyChunkObserver, fn func(RecordRow, recordRowMeta)) bool {
	rep := ps.rep
	after := int64(math.MinInt64) // below every possible id: a tampered database may hold ids <= 0
	var nRows, nTimes int64
	for {
		var rows []RecordRow
		var metas []recordRowMeta
		var times []timeRow
		more := false
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			rows, metas, times, more = nil, nil, nil, false
			rs, err := h.QueryContext(ctx, recordsSelect, after, verifyChunkRows)
			if err != nil {
				return err
			}
			defer func() { _ = rs.Close() }()
			size := 0
			for rs.Next() {
				var r RecordRow
				var m recordRowMeta
				var pname, pver, phash sql.NullString
				var deleted, recovered int64
				if err := rs.Scan(&r.ID, &m.batchID, &m.parserID, &r.Type, &r.PayloadV, &r.ArtifactID, &r.SourcePath, &r.Locator,
					&r.SrcOffset, &r.SrcLength, &r.TS, &r.TSEnd, &r.TSBasis, &r.TZOffsetMin, &deleted, &recovered, &r.RecoveryMethod,
					&r.Confidence, &pname, &pver, &phash, &r.Summary, &r.Body, &r.Payload, &m.sig); err != nil {
					return err
				}
				r.Deleted, r.Recovered = deleted != 0, recovered != 0
				m.hasParse = pname.Valid
				r.ParserName, r.ParserVersion = pname.String, pver.String
				if phash.Valid {
					h := phash.String
					r.ParserHash = &h
				}
				rows = append(rows, r)
				metas = append(metas, m)
				size += len(r.Payload) + len(r.Summary) + len(r.Type) + len(r.ArtifactID)
				if r.Body != nil {
					size += len(*r.Body)
				}
				if len(rows) >= verifyChunkRows || size >= verifyChunkBytes {
					more = true
					break
				}
			}
			if err := rs.Err(); err != nil {
				return err
			}
			if err := rs.Close(); err != nil {
				return err
			}
			// the record_times of this id range: after the previous chunk's last id
			// up to this chunk's last id (all the rest for the final chunk).
			q, args := timesSelectAll, []any{after}
			if more {
				q, args = timesSelectRange,
					[]any{after, rows[len(rows)-1].ID}
			}
			ts, err := h.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer func() { _ = ts.Close() }()
			for ts.Next() {
				var t timeRow
				if err := ts.Scan(&t.recordID, &t.Kind, &t.TS, &t.Basis, &t.TZOffsetMin, &t.sig); err != nil {
					return err
				}
				times = append(times, t)
			}
			return ts.Err()
		})
		if err != nil {
			unreadable(rep, "records", err)
			return false
		}
		if observe != nil {
			observe("records", len(rows))
		}
		index := make(map[int64]int, len(rows))
		for i, r := range rows {
			index[r.ID] = i
		}
		nRows += int64(len(rows))
		nTimes += int64(len(times))
		for _, t := range times {
			timesClasses.check(ps, t.sig, func() string { return fmt.Sprintf("record %d kind %q", t.recordID, t.Kind) })
			if t.recordID <= 0 {
				ps.add("nonpositive-id", "record_times row (kind %q) of record %d: record_id is not positive", t.Kind, t.recordID)
			}
			i, ok := index[t.recordID]
			if !ok {
				ps.add("orphan-time", "record_times row (kind %q) of record %d has no record", t.Kind, t.recordID)
				continue
			}
			rows[i].Times = append(rows[i].Times, t.RecordTime)
		}
		for i, r := range rows {
			recordsClasses.check(ps, metas[i].sig, func() string { return fmt.Sprintf("id %d", r.ID) })
			if r.ID <= 0 {
				ps.add("nonpositive-id", "record %d: id is not positive", r.ID)
			}
			fn(r, metas[i])
		}
		last := int64(0)
		if more {
			last = rows[len(rows)-1].ID
		}
		if !more || last == math.MaxInt64 {
			c.checkTableCount(ctx, rep, "records", nRows)
			c.checkTableCount(ctx, rep, "record_times", nTimes)
			return true
		}
		// every keyset scan must advance: the next chunk starts above this one's last key
		if last < after {
			ps.add("scan", "scan of records cannot make progress: the chunk after id %d ended at id %d", after, last)
			return false
		}
		after = last + 1
	}
}

// checkTableCount compares the number of rows a scan read with count(*) of the
// table (defence in depth: a row a keyset scan can never reach is still reported).
func (c *Case) checkTableCount(ctx context.Context, rep *VerifyReport, table string, read int64) {
	var n int64
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		return h.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n)
	})
	switch {
	case err != nil:
		unreadable(rep, table+" count", err)
	case n != read:
		rep.problemf("%s holds %d rows but verify read %d of them", table, n, read)
	}
}

type timeRow struct {
	recordID int64
	sig      string
	RecordTime
}

// checkNextID requires records_meta.next_id to be above every stored record id.
func (c *Case) checkNextID(ctx context.Context, rep *VerifyReport, maxID int64) {
	var v string
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		return h.QueryRowContext(ctx, `SELECT value FROM records_meta WHERE key = 'next_id'`).Scan(&v)
	})
	if err != nil {
		unreadable(rep, "records_meta next_id", err)
		return
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	switch {
	case perr != nil || n < 1:
		rep.problemf("records_meta next_id holds %q, which is not a valid id", v)
	case n <= maxID:
		rep.problemf("records_meta next_id is %d but record %d exists: next_id must be above the highest record id", n, maxID)
	}
}

// RecordsSummary is the part of the one-line verify summary that names the
// records checked (", N records"); it is empty when no record was checked, so the
// line of a case without records is unchanged.
func (r VerifyReport) RecordsSummary() string {
	if r.RecordsChecked == 0 {
		return ""
	}
	return fmt.Sprintf(", %d records", r.RecordsChecked)
}
