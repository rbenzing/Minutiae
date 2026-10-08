package evidence

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Reindex limits.
const (
	defaultReindexChunkRows = 5000
	maxReindexChunkRows     = 20000
	// reindexChunkBytes ends a chunk of documents early when their text is this large, so a table
	// of huge records cannot make one write transaction (or its buffer) unbounded.
	reindexChunkBytes = 64 << 20
	// maxReindexErrorLen bounds the error text a records.reindex.error entry carries.
	maxReindexErrorLen = 2000
	// maxReindexVersionLen bounds the old index state value an audit entry copies.
	maxReindexVersionLen = 256
)

// ReindexOptions are the options of ReindexText.
type ReindexOptions struct {
	// ChunkRows is the number of records read, and written to the index in one transaction, at a
	// time: 0 means 5000, the range is 1 to 20000.
	ChunkRows int
}

// ReindexResult is the outcome of ReindexText.
type ReindexResult struct {
	ReindexID string `json:"reindex_id"`
	// FromNormVersion is what records_meta held when the reindex began ("" when the index was never
	// built or the key was missing); NormVersion is what the index is built with now.
	FromNormVersion string `json:"from_norm_version"`
	NormVersion     string `json:"norm_version"`
	// Records is the number of rows of the records table; Indexed the number that have text and so a
	// document in each index; Docs the documents each full-text table holds afterwards.
	Records int64            `json:"records"`
	Indexed int64            `json:"indexed"`
	Docs    map[string]int64 `json:"docs"`
}

// ReindexText rebuilds both full-text indexes from the records. It is the one command that writes
// the index outside an ingest, and it writes nothing but the two FTS tables and the index state
// (records_meta fts_norm_version): the records, their times, runs and every other table are only
// read.
//
// It requires schema v3 and the schema objects this build defines (a tampered schema is
// ErrIntegrity; nothing is audited or changed), and the case's live-ingest slot (ErrIngestActive
// while a records writer is running in this process). It then reads the index state and the record
// count and, in this order:
//
//  1. audits records.reindex (fsynced) while the index is untouched;
//  2. in one transaction drops and recreates both FTS tables from FTSTableDDL (so a corrupt index
//     structure does not stop it) and sets the state to "building": from here on neither a search nor
//     the writer may use the index;
//  3. reads the records in chunks (the one stream verify reads them with) and writes each chunk's
//     documents in a transaction of its own;
//  4. counts the documents of each table (its _docsize rows), sets the state to FTSNormVersion()
//     and only then audits records.reindex.done, as case.upgrade.done follows its migration.
//
// Any error from step 1 on is audited as records.reindex.error and returned; once step 2 ran the
// state stays "building". A process that dies after step 4 set the state and before it audited the
// conclusion leaves a current index and an announced reindex: verify reports that in a notice and the
// next reindex concludes it. The reads and the writes are never nested: one connection serves them in
// turn.
func (c *Case) ReindexText(ctx context.Context, o ReindexOptions) (ReindexResult, error) {
	chunk, err := o.chunkRows()
	if err != nil {
		return ReindexResult{}, err
	}
	if err := c.RequireSchema(3); err != nil {
		return ReindexResult{}, err
	}
	if err := c.RequireSchemaObjects(ctx); err != nil {
		return ReindexResult{}, err
	}
	id, err := newReindexID()
	if err != nil {
		return ReindexResult{}, err
	}
	if active, ok := c.BeginIngest(id); !ok {
		return ReindexResult{}, fmt.Errorf("%w: %s", ErrIngestActive, active)
	}
	defer c.EndIngest(id)

	res := ReindexResult{ReindexID: id, NormVersion: FTSNormVersion()}
	err = c.ReadRecordsTx(ctx, func(h ReadHandle) error {
		st, err := ReadIndexState(ctx, h)
		if err != nil {
			return err
		}
		res.FromNormVersion = clipText(st.Value, maxReindexVersionLen)
		return h.QueryRowContext(ctx, `SELECT count(*) FROM records`).Scan(&res.Records)
	})
	if err != nil {
		return ReindexResult{}, err
	}
	if _, err := c.Audit.Append(ActionReindex, "", ReindexStart{
		ReindexID: id, FromNormVersion: res.FromNormVersion, NormVersion: res.NormVersion, Records: res.Records, Tables: FTSTables(),
	}.Details()); err != nil {
		return ReindexResult{}, fmt.Errorf("audit reindex start: %w", err)
	}

	r := &reindexRun{c: c, res: &res, chunk: chunk}
	if err := r.run(ctx); err != nil {
		if r.concluded {
			return res, err // the index is current; there is no failure to audit
		}
		return res, c.auditReindexError(id, res.Indexed, err)
	}
	return res, nil
}

func (o ReindexOptions) chunkRows() (int, error) {
	switch {
	case o.ChunkRows == 0:
		return defaultReindexChunkRows, nil
	case o.ChunkRows < 1 || o.ChunkRows > maxReindexChunkRows:
		return 0, fmt.Errorf("reindex: ChunkRows %d is out of range (0 for the default of %d, or 1 to %d)", o.ChunkRows, defaultReindexChunkRows, maxReindexChunkRows)
	}
	return o.ChunkRows, nil
}

func newReindexID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("reindex id: %w", err)
	}
	return "rix-" + hex.EncodeToString(b[:]), nil
}

// auditReindexError records the failure of an announced reindex and returns err (joined with the
// failure to audit it, if that failed too).
func (c *Case) auditReindexError(id string, indexed int64, err error) error {
	if _, aerr := c.Audit.Append(ActionReindexError, "", ReindexFailure{ReindexID: id, Error: clipText(err.Error(), maxReindexErrorLen), RecordsIndexed: indexed}.Details()); aerr != nil {
		return errors.Join(err, fmt.Errorf("audit reindex error: %w", aerr))
	}
	return err
}

// clipText cuts s to at most n bytes on a rune boundary, marking a cut with "...": the text of a
// tampered value or of an error must not make an audit line unbounded.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}

// reindexRun is the state of one ReindexText after its start entry was audited.
type reindexRun struct {
	c     *Case
	res   *ReindexResult
	chunk int
	// concluded is set once the index state is current: an error after that point is not a failed
	// reindex.
	concluded bool
}

func (r *reindexRun) hook(point string) error {
	if r.c.reindexHook == nil {
		return nil
	}
	return r.c.reindexHook(point)
}

func (r *reindexRun) run(ctx context.Context) error {
	if err := r.hook("after-start-audit"); err != nil {
		return err
	}
	if err := r.c.StoreTx(ctx, func(tx *sql.Tx) error { return resetIndex(ctx, tx) }); err != nil {
		return fmt.Errorf("reset the full-text index: %w", err)
	}
	if err := r.hook("after-reset"); err != nil {
		return err
	}
	if err := r.rebuild(ctx); err != nil {
		return err
	}
	if err := r.hook("before-meta"); err != nil {
		return err
	}
	docs, err := r.countDocs(ctx)
	if err != nil {
		return err
	}
	r.res.Docs = docs
	if err := r.c.StoreTx(ctx, func(tx *sql.Tx) error { return setIndexState(ctx, tx, r.res.NormVersion) }); err != nil {
		return fmt.Errorf("set the full-text index state: %w", err)
	}
	r.concluded = true
	if err := r.hook("after-meta"); err != nil {
		return err
	}
	if _, err := r.c.Audit.Append(ActionReindexDone, "", ReindexDone{
		ReindexID: r.res.ReindexID, NormVersion: r.res.NormVersion, RecordsIndexed: r.res.Indexed, Docs: docs,
	}.Details()); err != nil {
		return fmt.Errorf("audit reindex done: %w", err)
	}
	return nil
}

// resetIndex drops both FTS tables and creates them again from the shared DDL, and marks the index
// as being built. Dropping reads nothing of the old index, so a wrecked structure does not matter.
// The vocab tables stay: they name the FTS tables, not their storage.
func resetIndex(ctx context.Context, tx *sql.Tx) error {
	for _, table := range FTSTables() {
		ddl, ok := FTSTableDDL(table)
		if !ok {
			return fmt.Errorf("no definition for %s", table)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE `+table); err != nil { //nolint:gosec // the table is one of the two FTS table constants
			return fmt.Errorf("drop %s: %w", table, err)
		}
		if _, err := tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("create %s: %w", table, err)
		}
	}
	return setIndexState(ctx, tx, indexBuildingValue)
}

// setIndexState writes the index state value. The key is upserted: a case whose key was removed gets
// it back.
func setIndexState(ctx context.Context, tx *sql.Tx, value string) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO records_meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		MetaFTSNormVersion, value); err != nil {
		return fmt.Errorf("set %s: %w", MetaFTSNormVersion, err)
	}
	return nil
}

// rebuild streams the records and writes their documents chunk by chunk, each chunk in a
// transaction of its own. The stream calls back after the chunk's read transaction closed, so the
// write below never nests inside it. The first error stops the stream by cancelling its context.
func (r *reindexRun) rebuild(ctx context.Context) error {
	sctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	var (
		docs    []FTSDoc
		size    int
		pending int   // rows handed over since the last write
		seen    int64 // rows handed over in all
		stepErr error
	)
	flush := func() error {
		if len(docs) > 0 {
			if err := r.c.StoreTx(ctx, func(tx *sql.Tx) error {
				st, err := readIndexState(ctx, tx)
				if err != nil {
					return err
				}
				if st.Kind != IndexBuilding {
					return fmt.Errorf("the index state changed to %q while it was being rebuilt", st.Value)
				}
				return insertFTSDocs(ctx, tx, docs)
			}); err != nil {
				return fmt.Errorf("write a chunk of the full-text index: %w", err)
			}
			r.res.Indexed += int64(len(docs))
			if err := r.hook("after-chunk"); err != nil {
				return err
			}
		}
		docs, size, pending = docs[:0], 0, 0
		return nil
	}

	rep := &VerifyReport{Problems: []string{}, Notices: []string{}}
	ps := &problemSet{rep: rep, counts: map[string]int{}}
	ok := r.c.streamRecords(sctx, ps, r.chunk, nil, func(row RecordRow, _ recordRowMeta) {
		if stepErr != nil {
			return
		}
		seen++
		pending++
		if doc, has := NewFTSDoc(row.ID, row.Summary, row.Body); has {
			docs = append(docs, doc)
			size += len(doc.Summary) + len(doc.Body)
		}
		if pending >= r.chunk || size >= reindexChunkBytes {
			if stepErr = flush(); stepErr != nil {
				cancel(stepErr)
			}
		}
	})
	switch {
	case stepErr != nil:
		return stepErr
	case !ok:
		if err := context.Cause(ctx); err != nil {
			return fmt.Errorf("read the records: %w", err)
		}
		return fmt.Errorf("read the records: %s", strings.Join(rep.Problems, "; "))
	}
	if err := flush(); err != nil {
		return err
	}
	if seen != r.res.Records {
		return fmt.Errorf("the records table held %d rows when the reindex began but %d could be read", r.res.Records, seen)
	}
	return nil
}

// countDocs reads the number of documents each full-text table holds (the rows of its _docsize
// shadow table) through the schema guard, which also confirms the tables are the ones defined, and
// requires them to equal the number of records indexed.
func (r *reindexRun) countDocs(ctx context.Context) (map[string]int64, error) {
	docs := map[string]int64{}
	err := r.c.ReadRecordsTx(ctx, func(h ReadHandle) error {
		for _, table := range FTSTables() {
			var n int64
			if err := h.QueryRowContext(ctx, `SELECT count(*) FROM `+table+`_docsize`).Scan(&n); err != nil { //nolint:gosec // the table is one of the two FTS table constants
				return fmt.Errorf("count the documents of %s: %w", table, err)
			}
			docs[table] = n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, table := range FTSTables() {
		if docs[table] != r.res.Indexed {
			return nil, fmt.Errorf("%s holds %d documents after the rebuild, but %d records were indexed", table, docs[table], r.res.Indexed)
		}
	}
	return docs, nil
}
