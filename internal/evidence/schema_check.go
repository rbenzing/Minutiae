package evidence

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Schema-object verification. sqlite_master is the schema: the tables, the
// indexes, the triggers and any view a reader's query would run through. A
// redefined index (`records_deleted` over another predicate), a view that shadows
// a table, a table whose CHECK constraints were removed or an extra trigger
// changes what every query returns without touching a single row, so neither the
// row digests nor `PRAGMA quick_check` see it. Verify therefore compares the
// WHOLE of sqlite_master with the schema this build creates for the database's
// schema version, and the records reader does the same cheap comparison before it
// serves a query.

// schemaObject is one row of sqlite_master as the comparison sees it.
type schemaObject struct {
	typ, name, table, sql string
}

func (o schemaObject) key() string { return o.typ + "\x00" + o.name }

// describe names the object for a problem: `index "records_ts"`.
func (o schemaObject) describe() string { return fmt.Sprintf("%s %q", o.typ, o.name) }

var (
	expectedSchemaMu    sync.Mutex
	expectedSchemaCache = map[int]map[string]schemaObject{}
)

// expectedSchema returns the objects of a database at the given schema version,
// exactly as this build's migrations create them (so there is one definition of
// the schema, the DDL, and the comparison cannot drift from it). The migrations
// run once per version in a scratch in-memory database.
func expectedSchema(version int) (map[string]schemaObject, error) {
	expectedSchemaMu.Lock()
	defer expectedSchemaMu.Unlock()
	if m, ok := expectedSchemaCache[version]; ok {
		return m, nil
	}
	if version < 1 || version > len(migrations) {
		return nil, fmt.Errorf("no schema is defined for version %d", version)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := applyMigrations(db, 0, version); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		return nil, err
	}
	m, err := scanSchemaObjects(rows)
	if err != nil {
		return nil, err
	}
	expectedSchemaCache[version] = m
	return m, nil
}

// scanSchemaObjects reads sqlite_master rows (type, name, tbl_name, sql) into a
// map keyed by type and name, with the SQL whitespace-normalized. It closes rows.
func scanSchemaObjects(rows *sql.Rows) (map[string]schemaObject, error) {
	defer func() { _ = rows.Close() }()
	m := map[string]schemaObject{}
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.typ, &o.name, &o.table, &o.sql); err != nil {
			return nil, err
		}
		o.sql = normalizeSpace(o.sql)
		m[o.key()] = o
	}
	return m, rows.Err()
}

// readSchemaObjects reads the database's sqlite_master.
func readSchemaObjects(ctx context.Context, h ReadHandle) (map[string]schemaObject, error) {
	rows, err := h.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master`)
	if err != nil {
		return nil, err
	}
	return scanSchemaObjects(rows)
}

// schemaDifferences compares the objects found with the expected ones and
// returns one problem text per difference, sorted. The texts for triggers are
// the ones P12 always had.
func schemaDifferences(expected, found map[string]schemaObject) []string {
	var out []string
	keys := make([]string, 0, len(expected))
	for k := range expected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := expected[k]
		got, ok := found[k]
		switch {
		case !ok && want.typ == "trigger":
			out = append(out, fmt.Sprintf("trigger %q is missing: the table %q is no longer protected against updates and deletes", want.name, want.table))
		case !ok:
			out = append(out, fmt.Sprintf("%s is missing from the schema: the table %q no longer has it", want.describe(), want.table))
		case got.sql != want.sql || got.table != want.table:
			if want.typ == "trigger" {
				out = append(out, fmt.Sprintf("trigger %q was altered: its definition %q is not the expected %q", want.name, got.sql, want.sql))
				break
			}
			out = append(out, fmt.Sprintf("%s was altered: it is defined as %q on table %q, not the expected %q on table %q",
				want.describe(), got.sql, got.table, want.sql, want.table))
		}
	}
	extra := make([]string, 0)
	for k := range found {
		if _, ok := expected[k]; !ok {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	for _, k := range extra {
		o := found[k]
		out = append(out, fmt.Sprintf("%s is not part of the schema (table %q, definition %q)", o.describe(), o.table, o.sql))
	}
	return out
}

// schemaVersionIn reads the schema version inside a read transaction.
func schemaVersionIn(ctx context.Context, h ReadHandle) (int, error) {
	var v int
	err := h.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&v)
	return v, err
}

// ReadRecordsTx is ReadTx for readers and writers of the record tables: inside the
// same transaction, before fn runs, it requires every schema object (table, index,
// trigger, view) of the database to be exactly the one this build defines for the
// database's schema version, and otherwise returns an error wrapping ErrIntegrity
// (`case verify` lists every difference). A redefined index or view, a shadowing
// table or an altered table definition can change what a query returns without
// changing a row, so no query runs against such a database. The comparison reads
// sqlite_master only (a few dozen rows).
func (c *Case) ReadRecordsTx(ctx context.Context, fn func(ReadHandle) error) error {
	return c.ReadTx(ctx, func(h ReadHandle) error {
		v, err := schemaVersionIn(ctx, h)
		if err != nil {
			return fmt.Errorf("artifacts.db schema_version: %w", err)
		}
		expected, err := expectedSchema(v)
		if err != nil {
			return fmt.Errorf("%w: artifacts.db: %w", ErrIntegrity, err)
		}
		found, err := readSchemaObjects(ctx, h)
		if err != nil {
			return fmt.Errorf("artifacts.db schema: %w", err)
		}
		if diffs := schemaDifferences(expected, found); len(diffs) > 0 {
			more := ""
			if len(diffs) > 3 {
				more = fmt.Sprintf(" (and %d more)", len(diffs)-3)
				diffs = diffs[:3]
			}
			return fmt.Errorf("%w: artifacts.db does not have the schema objects of schema v%d, so no query is run against it: %s%s; run: minutiae case verify --case %s",
				ErrIntegrity, v, strings.Join(diffs, "; "), more, c.Dir)
		}
		return fn(h)
	})
}

// RequireSchemaObjects checks the schema objects as ReadRecordsTx does, without
// reading anything else.
func (c *Case) RequireSchemaObjects(ctx context.Context) error {
	return c.ReadRecordsTx(ctx, func(ReadHandle) error { return nil })
}
