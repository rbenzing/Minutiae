package evidence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// ErrIndexNotCurrent is returned (wrapped) by everything that needs the full-text index to answer
// or to be maintained, when the index is not current: it was never built (a case upgraded from
// schema v2 holding records), a rebuild was interrupted, it was built by another normalization or
// library version, or its state cannot be trusted. The remedy is always
// `minutiae records reindex --case <dir>`.
var ErrIndexNotCurrent = errors.New("full-text index is not current")

// IndexKind is the state of the full-text index, from records_meta.fts_norm_version.
type IndexKind string

// The index states.
const (
	// IndexCurrent: the value equals FTSNormVersion(); the index answers and is maintained.
	IndexCurrent IndexKind = "current"
	// IndexUnbuilt: the value is '' (a v2 case with records was upgraded; nothing was indexed).
	IndexUnbuilt IndexKind = "unbuilt"
	// IndexBuilding: the value is 'building' (a rebuild started and never concluded).
	IndexBuilding IndexKind = "building"
	// IndexStale: a well-formed version of another build (fts<n>/...): the index may be sound for
	// that build, but this build would tokenize and fold differently.
	IndexStale IndexKind = "stale"
	// IndexInvalid: anything else, including a missing key.
	IndexInvalid IndexKind = "invalid"
)

// IndexState is the state of the full-text index of a case.
type IndexState struct {
	Kind IndexKind
	// Value is what records_meta holds ("" when the key is missing too: see Kind).
	Value string
	// Current is FTSNormVersion() of this build.
	Current string
}

// maxIndexValueLen bounds the version text a stored value may have to count as well-formed.
const maxIndexValueLen = 200

// wellFormedIndexVersion matches the shape of every version string any build wrote:
// fts<pipeline>/<parts separated by slashes>.
var wellFormedIndexVersion = regexp.MustCompile(`^fts[0-9]{1,9}/[0-9A-Za-z._/-]+$`)

const indexBuildingValue = "building"

// classifyIndexValue maps a stored value to its kind.
func classifyIndexValue(value, current string) IndexKind {
	switch {
	case value == current:
		return IndexCurrent
	case value == "":
		return IndexUnbuilt
	case value == indexBuildingValue:
		return IndexBuilding
	case len(value) <= maxIndexValueLen && wellFormedIndexVersion.MatchString(value):
		return IndexStale
	}
	return IndexInvalid
}

// ReadIndexState reads the state of the full-text index inside the read transaction of h. The
// meta key must exist exactly once and hold text; a missing key, a second row or a value that is
// not text is IndexInvalid (not an error: the state is the finding). The caller guarantees the
// schema is v3 or newer (Case.IndexState checks it).
func ReadIndexState(ctx context.Context, h ReadHandle) (IndexState, error) {
	return readIndexState(ctx, h)
}

// ctxQuerier is what a read handle and a write transaction share.
type ctxQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// readIndexState is ReadIndexState over any querier (the write path reads the state inside its own transaction).
func readIndexState(ctx context.Context, h ctxQuerier) (IndexState, error) {
	current := FTSNormVersion()
	rows, err := h.QueryContext(ctx, `SELECT typeof(value), CAST(value AS TEXT) FROM records_meta WHERE key = ? LIMIT 2`, MetaFTSNormVersion)
	if err != nil {
		return IndexState{}, fmt.Errorf("read %s: %w", MetaFTSNormVersion, err)
	}
	defer func() { _ = rows.Close() }()
	var values []string
	var texts int
	for rows.Next() {
		var typ, v string
		if err := rows.Scan(&typ, &v); err != nil {
			return IndexState{}, fmt.Errorf("read %s: %w", MetaFTSNormVersion, err)
		}
		if typ == "text" {
			texts++
		}
		values = append(values, v)
	}
	if err := rows.Err(); err != nil {
		return IndexState{}, fmt.Errorf("read %s: %w", MetaFTSNormVersion, err)
	}
	if len(values) != 1 || texts != 1 {
		v := ""
		if len(values) == 1 {
			v = values[0]
		}
		return IndexState{Kind: IndexInvalid, Value: v, Current: current}, nil
	}
	return IndexState{Kind: classifyIndexValue(values[0], current), Value: values[0], Current: current}, nil
}

// IndexState returns the state of the case's full-text index. A case older than schema v3 has no
// index: ErrNeedsUpgrade.
func (c *Case) IndexState(ctx context.Context) (IndexState, error) {
	var st IndexState
	err := c.ReadTx(ctx, func(h ReadHandle) error {
		v, err := schemaVersionIn(ctx, h)
		if err != nil {
			return fmt.Errorf("artifacts.db schema_version: %w", err)
		}
		if v < 3 {
			return fmt.Errorf("%w: case schema v%d < v3 has no full-text index; run: minutiae case upgrade --case %s", ErrNeedsUpgrade, v, c.Dir)
		}
		st, err = ReadIndexState(ctx, h)
		return err
	})
	return st, err
}

// RequireIndexCurrent returns nil when the index is current, ErrNeedsUpgrade for a case older than
// v3, and otherwise an error wrapping ErrIndexNotCurrent that names `records reindex --case <dir>`.
func (c *Case) RequireIndexCurrent(ctx context.Context) error {
	st, err := c.IndexState(ctx)
	if err != nil {
		return err
	}
	return st.Require(c.Dir)
}

// RequireIndexCurrentIn is RequireIndexCurrent inside an open read transaction (the check then
// belongs to the same snapshot as the query it guards). The remedy names `<dir>`, which a
// ReadHandle does not know.
func RequireIndexCurrentIn(ctx context.Context, h ReadHandle) error {
	st, err := ReadIndexState(ctx, h)
	if err != nil {
		return err
	}
	return st.Require("<dir>")
}

// Require returns nil for a current index and the refusal (wrapping ErrIndexNotCurrent, naming `records reindex --case <dir>`) otherwise.
func (s IndexState) Require(dir string) error {
	if s.Kind == IndexCurrent {
		return nil
	}
	return fmt.Errorf("%w: %s; run: minutiae records reindex --case %s", ErrIndexNotCurrent, s.describe(), dir)
}

// describe says why the index cannot be used, in one clause.
func (s IndexState) describe() string {
	switch s.Kind {
	case IndexUnbuilt:
		return "it was never built for the records of this case"
	case IndexBuilding:
		return "a rebuild was started and never finished"
	case IndexStale:
		return fmt.Sprintf("it was built by %q and this build is %q", s.Value, s.Current)
	case IndexCurrent:
		return "it is current"
	}
	return fmt.Sprintf("records_meta %s holds %q (or the key is missing), which is not a version of the index", MetaFTSNormVersion, s.Value)
}

// FTSDoc is one record's normalized index text.
type FTSDoc struct {
	ID            int64
	Summary, Body string
}

// NewFTSDoc normalizes the text of record id with NormalizeText, the one function every index text
// goes through. ok is false when nothing is left to index (the summary is empty and the body is nil
// or empty, after normalization): such a record has no row in either index.
func NewFTSDoc(id int64, summary string, body *string) (doc FTSDoc, ok bool) {
	doc = FTSDoc{ID: id, Summary: NormalizeText(summary)}
	if body != nil {
		doc.Body = NormalizeText(*body)
	}
	return doc, doc.Summary != "" || doc.Body != ""
}

// InsertFTSDocs writes the documents into both full-text tables inside tx, the transaction that
// writes their records. The index must be current: the meta value is read inside tx and must equal
// FTSNormVersion(), otherwise nothing is written and the error wraps ErrIndexNotCurrent.
func InsertFTSDocs(ctx context.Context, tx *sql.Tx, docs []FTSDoc) error {
	st, err := readIndexState(ctx, tx)
	if err != nil {
		return err
	}
	if err := st.Require("<dir>"); err != nil {
		return err
	}
	return insertFTSDocs(ctx, tx, docs)
}

// insertFTSDocs is InsertFTSDocs without the state check: reindex (state 'building') and the
// verify rebuild use it, so there is one insert statement and one place that decides what is
// indexed. The table names are the two constants, never input.
func insertFTSDocs(ctx context.Context, tx *sql.Tx, docs []FTSDoc) error {
	for _, table := range FTSTables() {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+table+` (rowid, summary, body) VALUES (?, ?, ?)`) //nolint:gosec // the table is one of the two FTS table constants
		if err != nil {
			return fmt.Errorf("prepare %s insert: %w", table, err)
		}
		for _, d := range docs {
			if _, err := stmt.ExecContext(ctx, d.ID, d.Summary, d.Body); err != nil {
				_ = stmt.Close()
				return fmt.Errorf("index record %d in %s: %w", d.ID, table, err)
			}
		}
		if err := stmt.Close(); err != nil {
			return fmt.Errorf("close %s insert: %w", table, err)
		}
	}
	return nil
}
