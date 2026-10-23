package evidence

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// The read-only connections of a concurrent verify (5D Task 15c).
//
// The case database has ONE connection (openDB: SetMaxOpenConns(1), and every ReadTx and StoreTx queues on it).
// That rule keeps a writer from ever meeting another writer or a reader mid-transaction. Verify is the one reader
// that needs more: P10 (SQLite's integrity_check, the longest phase) must run beside the record stream and the
// index pass, and the two full-text tables are decoded together. EXCEPTION to the rule, recorded in
// docs/architecture.md: verify opens, for its own duration, a pool of connections to artifacts.db that
//
//   - is opened mode=rw (never created) with PRAGMA query_only = ON on every connection, so SQLite refuses every write,
//     and is used only through ReadHandle, which takes one SELECT or WITH statement and no PRAGMA,
//   - exists only between the start of the record part of verify and its end, under the case lock verify already
//     holds, so no other process writes while it is open and the case connection writes nothing during verify,
//   - is closed on every path, a panic and a cancel included.
//
// Why not mode=ro: measured with modernc.org/sqlite v1.60.1, PRAGMA integrity_check on a connection opened mode=ro
// does NOT report a row that breaks a CHECK constraint ("ok"), while the same file read through a read-write
// connection with query_only does ("CHECK constraint failed in records"; TestVerifyIntegrityCheckFlagged). A
// read-only open would silently weaken P10, so the pool is read-write at the file level and read-only by query_only.
// TestVerifyIntegrityCheckConnectionIsReadOnly proves that no write gets through and that artifacts.db is unchanged.
//
// When the pool cannot be opened (the file is unreadable to a second handle) verify runs the serial order on the
// case connection, which reports the same findings.

// verifyReaderConns is the size of the pool: P10 and one connection for each of the two full-text tables.
const verifyReaderConns = 3

// verifyReaders is the pool of read-only connections of one verify.
type verifyReaders struct {
	db *sql.DB
}

// readerURI is the SQLite URI of the database file with mode=rw (the file is opened read-write but never created).
// The path is escaped, so spaces, '?', '#' and '%' stay part of the file name, and a Windows drive letter becomes
// /C:/...
func readerURI(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: "mode=rw"}
	return u.String(), nil
}

// roConnector dials the read-only connections to artifacts.db: every one has query_only on before it is handed out.
type roConnector struct{ uri string }

func (c roConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func (c roConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.uri)
	if err != nil {
		return nil, fmt.Errorf("open artifacts.db read-only: %w", err)
	}
	// query_only makes SQLite refuse every write; temp_store = MEMORY keeps a sorter or temporary b-tree of
	// integrity_check out of the OS temp directory, as ReadTx does for the case connection
	for _, p := range []string{`PRAGMA query_only = ON`, `PRAGMA temp_store = MEMORY`} {
		if err := runPragma(ctx, conn, p); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

// openVerifyReaders opens the pool for the database file at path and proves that a connection can be made.
func openVerifyReaders(ctx context.Context, path string) (*verifyReaders, error) {
	uri, err := readerURI(path)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(roConnector{uri: uri})
	db.SetMaxOpenConns(verifyReaderConns)
	db.SetMaxIdleConns(verifyReaderConns)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	// the URI must name the file the case connection has open, not another one the escaping made of the path
	var opened string
	if err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&opened); err != nil {
		_ = db.Close()
		return nil, err
	}
	want, err1 := os.Stat(path)
	got, err2 := os.Stat(opened)
	if err1 != nil || err2 != nil || !os.SameFile(want, got) {
		_ = db.Close()
		return nil, fmt.Errorf("the read-only connections opened %q, not %q", opened, path)
	}
	return &verifyReaders{db: db}, nil
}

// close closes every connection of the pool. It is safe to call twice.
func (v *verifyReaders) close() {
	if v != nil && v.db != nil {
		_ = v.db.Close()
	}
}

// readTx runs fn inside one read transaction on a connection of the pool, as ReadTx does on the case connection:
// the handle takes SELECT and WITH statements only, and the transaction is always rolled back.
func (v *verifyReaders) readTx(ctx context.Context, fn func(ReadHandle) error) (err error) {
	conn, err := v.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("artifacts.db: %w", err)
	}
	defer func() { _ = conn.Close() }()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("artifacts.db begin: %w", err)
	}
	defer func() { // also runs when fn panics (the panic continues)
		if rerr := tx.Rollback(); rerr != nil && !errors.Is(rerr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("artifacts.db rollback: %w", rerr))
		}
	}()
	return fn(ReadHandle{tx: tx})
}
