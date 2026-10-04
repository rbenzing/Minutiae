package evidence

import (
	"database/sql"
	"errors"
	"fmt"
)

// immutableTables are the tables that are append-only: BEFORE UPDATE and BEFORE
// DELETE triggers abort any change. records_meta is the one mutable table. The
// triggers catch our own bugs; they are not tamper-proof (an attacker with file
// access can drop one), so verification both checks that they exist and
// recomputes the audited digests.
var immutableTables = []string{
	"artifacts", "parsers", "record_batches", "records",
	"record_times", "record_runs", "record_run_artifacts", "record_superseded",
}

// immutabilityTriggerName returns "immut_<table>_upd" or "immut_<table>_del".
func immutabilityTriggerName(table, op string) string {
	return "immut_" + table + "_" + op
}

// immutabilityTriggerSQL returns the CREATE TRIGGER statement for table and op
// ("upd" or "del"). Task 6's verify compares the stored statement with it.
func immutabilityTriggerSQL(table, op string) string {
	event := "UPDATE"
	if op == "del" {
		event = "DELETE"
	}
	return `CREATE TRIGGER ` + immutabilityTriggerName(table, op) + ` BEFORE ` + event + ` ON ` + table +
		` BEGIN SELECT RAISE(ABORT, 'immutable table ` + table + `'); END`
}

// expectedTriggers maps every immutability trigger name to its table.
func expectedTriggers() map[string]string {
	m := make(map[string]string, 2*len(immutableTables))
	for _, t := range immutableTables {
		m[immutabilityTriggerName(t, "upd")] = t
		m[immutabilityTriggerName(t, "del")] = t
	}
	return m
}

// ErrMigrationBlocked means a schema migration was refused and nothing was
// changed (for v2: the v1 records table is not empty).
var ErrMigrationBlocked = errors.New("schema migration blocked")

// v2Precheck refuses to migrate a v1 database whose records table holds rows:
// v2 drops that table, and a migration never drops data.
func v2Precheck(tx *sql.Tx) error {
	var n int64
	if err := tx.QueryRow(`SELECT count(*) FROM records`).Scan(&n); err != nil {
		return fmt.Errorf("count v1 records: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("%w: the v1 records table holds %d rows (v2 would drop it); nothing was changed", ErrMigrationBlocked, n)
	}
	return nil
}

// v2Statements upgrades schema v1 to v2: it replaces the unused v1 records
// table with the unified record tables, indexes and immutability triggers. Every
// new table is STRICT (SQLite refuses a value of the wrong storage class); verify
// checks the classes itself and does not rely on it (verify_class.go).
var v2Statements = buildV2Statements()

func buildV2Statements() []string {
	stmts := []string{
		`DROP INDEX records_type`,
		`DROP INDEX records_ts`,
		`DROP TABLE records`,
		`CREATE TABLE parsers (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			version TEXT NOT NULL,
			hash TEXT,
			UNIQUE (name, version)) STRICT`,
		`CREATE TABLE record_batches (
			batch_id INTEGER PRIMARY KEY,
			ingest_id TEXT NOT NULL,
			batch_no INTEGER NOT NULL,
			first_id INTEGER NOT NULL,
			count INTEGER NOT NULL,
			digest TEXT NOT NULL,
			created TEXT NOT NULL,
			UNIQUE (ingest_id, batch_no)) STRICT`,
		`CREATE TABLE records (
			id INTEGER PRIMARY KEY,
			batch_id INTEGER NOT NULL REFERENCES record_batches(batch_id),
			type TEXT NOT NULL,
			payload_v INTEGER NOT NULL,
			artifact_id TEXT NOT NULL REFERENCES artifacts(id),
			source_path TEXT,
			locator TEXT,
			src_offset INTEGER,
			src_length INTEGER,
			ts INTEGER,
			ts_end INTEGER,
			ts_basis TEXT,
			tz_offset_min INTEGER,
			deleted INTEGER NOT NULL DEFAULT 0,
			recovered INTEGER NOT NULL DEFAULT 0,
			recovery_method TEXT,
			confidence INTEGER,
			parser_id INTEGER NOT NULL REFERENCES parsers(id),
			summary TEXT NOT NULL DEFAULT '',
			body TEXT,
			payload TEXT NOT NULL,
			CHECK (recovered IN (0,1) AND deleted IN (0,1)),
			CHECK (recovered = 0 OR (deleted = 1 AND recovery_method IS NOT NULL)),
			CHECK (recovered = 1 OR recovery_method IS NULL),
			CHECK ((src_offset IS NULL) = (src_length IS NULL)),
			CHECK (src_offset IS NULL OR (src_offset >= 0 AND src_length >= 0)),
			CHECK (confidence IS NULL OR confidence BETWEEN 0 AND 100),
			CHECK (ts_basis IS NULL OR ts_basis IN ('utc','local-offset','local-unknown')),
			CHECK ((ts IS NULL) = (ts_basis IS NULL)),
			CHECK (ts_end IS NULL OR ts IS NOT NULL),
			CHECK ((tz_offset_min IS NOT NULL) = (ts_basis IS 'local-offset'))) STRICT`,
		`CREATE TABLE record_times (
			record_id INTEGER NOT NULL REFERENCES records(id),
			kind TEXT NOT NULL,
			ts INTEGER NOT NULL,
			ts_basis TEXT NOT NULL CHECK (ts_basis IN ('utc','local-offset','local-unknown')),
			tz_offset_min INTEGER,
			PRIMARY KEY (record_id, kind),
			CHECK ((tz_offset_min IS NOT NULL) = (ts_basis = 'local-offset'))) STRICT, WITHOUT ROWID`,
		`CREATE TABLE record_runs (
			end_seq INTEGER PRIMARY KEY,
			ingest_id TEXT NOT NULL UNIQUE,
			parser_id INTEGER NOT NULL REFERENCES parsers(id),
			analysis_id TEXT,
			outcome TEXT NOT NULL CHECK (outcome IN ('complete','incomplete','interrupted')),
			batches INTEGER NOT NULL,
			records INTEGER NOT NULL,
			first_id INTEGER NOT NULL,
			last_id INTEGER NOT NULL,
			rollup TEXT NOT NULL,
			ended TEXT NOT NULL) STRICT`,
		`CREATE TABLE record_run_artifacts (
			ingest_id TEXT NOT NULL REFERENCES record_runs(ingest_id),
			artifact_id TEXT NOT NULL REFERENCES artifacts(id),
			PRIMARY KEY (ingest_id, artifact_id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE record_superseded (
			ingest_id TEXT NOT NULL,
			artifact_id TEXT NOT NULL,
			PRIMARY KEY (ingest_id, artifact_id)) STRICT, WITHOUT ROWID`,
		`CREATE TABLE records_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT`,
		`INSERT INTO records_meta (key, value) VALUES ('next_id', '1')`,
		`CREATE INDEX records_type_ts ON records (type, (ts IS NULL), ts, id)`,
		`CREATE INDEX records_ts ON records ((ts IS NULL), ts, id)`,
		`CREATE INDEX records_artifact ON records (artifact_id, parser_id, id)`,
		`CREATE INDEX records_deleted ON records ((ts IS NULL), ts, id) WHERE deleted = 1`,
		`CREATE INDEX records_parser ON records (parser_id, type, id)`,
		`CREATE INDEX record_times_kind ON record_times (kind, ts, record_id)`,
		`CREATE INDEX record_run_artifacts_artifact ON record_run_artifacts (artifact_id, ingest_id)`,
	}
	for _, t := range immutableTables {
		stmts = append(stmts, immutabilityTriggerSQL(t, "upd"), immutabilityTriggerSQL(t, "del"))
	}
	return stmts
}

// TriggerDef is one immutability trigger as the schema defines it.
type TriggerDef struct {
	Name  string
	Table string
	SQL   string // the CREATE TRIGGER statement
}

// ImmutabilityTriggers returns the 16 immutability triggers (BEFORE UPDATE and
// BEFORE DELETE on each immutable table) exactly as the schema creates them.
// Verify compares the database's triggers with these; test helpers that tamper
// with the database re-create them from here after tampering.
func ImmutabilityTriggers() []TriggerDef {
	out := make([]TriggerDef, 0, 2*len(immutableTables))
	for _, t := range immutableTables {
		for _, op := range []string{"upd", "del"} {
			out = append(out, TriggerDef{Name: immutabilityTriggerName(t, op), Table: t, SQL: immutabilityTriggerSQL(t, op)})
		}
	}
	return out
}
