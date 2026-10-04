package evidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

// ErrNeedsUpgrade is returned by RequireSchema (and so by every command that
// needs a newer schema) for a case whose database is older than the command
// needs. `minutiae case upgrade` is the only way forward; nothing migrates a
// case implicitly.
var ErrNeedsUpgrade = errors.New("case database needs upgrade")

// Audit actions of an upgrade (the codecs are below). Each upgrade is announced (and fsynced) before
// the migration touches the database, and concluded afterwards.
const (
	ActionCaseUpgrade      = "case.upgrade"       // details: from, to
	ActionCaseUpgradeDone  = "case.upgrade.done"  // details: from, to, resumed
	ActionCaseUpgradeError = "case.upgrade.error" // details: from, to, error
)

func upgradeDetails(from, to int) map[string]any {
	return map[string]any{"from": from, "to": to}
}

// upgradeDoneDetails: resumed is true only when the entry completes an earlier
// upgrade whose migration had committed but whose conclusion was never audited.
func upgradeDoneDetails(from, to int, resumed bool) map[string]any {
	return map[string]any{"from": from, "to": to, "resumed": resumed}
}

func upgradeErrorDetails(from, to int, err error) map[string]any {
	return map[string]any{"from": from, "to": to, "error": err.Error()}
}

// auditInt reads an integer detail of an audit entry as decoded from the log
// (json.Number) or as passed to Append (int, int64, float64).
func auditInt(details map[string]any, key string) (int, bool) {
	switch v := details[key].(type) {
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil && int64(int(n)) == n
	case int:
		return v, true
	case int64:
		return int(v), int64(int(v)) == v
	case float64:
		n := int(v)
		return n, float64(n) == v
	}
	return 0, false
}

// upgradeStep is the from/to of a case.upgrade audit entry.
type upgradeStep struct {
	From, To int
	Seq      int64
}

// schemaAudit is what the audit log says about the database schema.
type schemaAudit struct {
	// Version is the schema version the audit log says the database has:
	// case.create's schema_version (1 when absent: the v1.0.0 build did not
	// record it), then the `to` of every case.upgrade.done.
	Version int
	// Dangling is the last case.upgrade that no done/error entry followed, nil
	// when there is none.
	Dangling *upgradeStep
	Problems []string
}

// auditedSchema derives the audited schema state from the audit entries.
func auditedSchema(entries []AuditEntry) schemaAudit {
	sa := schemaAudit{Version: 1}
	seenCreate := false
	for _, e := range entries {
		switch e.Action {
		case firstAuditAction:
			if seenCreate {
				continue
			}
			seenCreate = true
			if _, present := e.Details["schema_version"]; !present {
				continue
			}
			v, ok := auditInt(e.Details, "schema_version")
			if !ok || v < 1 {
				sa.Problems = append(sa.Problems, fmt.Sprintf("audit seq %d: case.create schema_version %v is not a valid schema version", e.Seq, e.Details["schema_version"]))
				continue
			}
			sa.Version = v
		case ActionCaseUpgrade:
			from, okF := auditInt(e.Details, "from")
			to, okT := auditInt(e.Details, "to")
			if !okF || !okT {
				sa.Problems = append(sa.Problems, fmt.Sprintf("audit seq %d: case.upgrade details unreadable", e.Seq))
				continue
			}
			sa.Dangling = &upgradeStep{From: from, To: to, Seq: e.Seq}
		case ActionCaseUpgradeError:
			sa.Dangling = nil
		case ActionCaseUpgradeDone:
			from, okF := auditInt(e.Details, "from")
			to, okT := auditInt(e.Details, "to")
			switch {
			case !okF || !okT:
				sa.Problems = append(sa.Problems, fmt.Sprintf("audit seq %d: case.upgrade.done details unreadable", e.Seq))
			case from != sa.Version:
				sa.Problems = append(sa.Problems, fmt.Sprintf("audit seq %d: case.upgrade.done from schema version %d, but the audited schema version was %d", e.Seq, from, sa.Version))
			default:
				sa.Version = to
			}
			sa.Dangling = nil
		}
	}
	return sa
}

// SchemaVersion returns the schema version of the case's artifacts.db.
func (c *Case) SchemaVersion() (int, error) { return c.store.SchemaVersion() }

// RequireSchema returns an error wrapping ErrNeedsUpgrade when the case schema
// is older than minVersion.
func (c *Case) RequireSchema(minVersion int) error {
	v, err := c.store.SchemaVersion()
	if err != nil {
		return fmt.Errorf("artifacts.db schema_version: %w", err)
	}
	if v < minVersion {
		return fmt.Errorf("%w: case schema v%d < v%d; run: minutiae case upgrade --case %s", ErrNeedsUpgrade, v, minVersion, c.Dir)
	}
	return nil
}

// UpgradeResult is the outcome of Case.Upgrade.
type UpgradeResult struct {
	From, To int
	Upgraded bool
	// Resumed is true when an announced upgrade whose migration had already
	// committed (the process died before its conclusion was audited) was
	// concluded now with a case.upgrade.done{resumed:true} entry. When that was the whole chain the
	// case was already at the current schema and Upgraded is false; after an older build's
	// interrupted step the upgrade may go on (Upgraded true).
	Resumed bool
	// ResumedFrom and ResumedTo are the versions of the upgrade that was concluded (the audit
	// entry's from and to), set only when Resumed is true. After an older build's interrupted 1 to 2
	// and a new 2 to 3 they are 1 and 2 while From and To are 2 and 3.
	ResumedFrom, ResumedTo int
	// RecordsToIndex is the number of records that are not in the full-text index because the
	// index is not built (a v2 case with records was upgraded): when it is above zero, run
	// `minutiae records reindex`. It is 0 for a case with no records and for a current index.
	RecordsToIndex int64
}

// Upgrade migrates the case database to CurrentSchema. It is the only way a
// case database is ever migrated, and it changes the case, so it is audited
// like any other write: case.upgrade{from,to} is appended and fsynced BEFORE
// the migration, then case.upgrade.done (or case.upgrade.error with the error;
// a pre-check failure wraps ErrMigrationBlocked and leaves the database
// untouched).
//
// The whole pending chain (for example v1 to v3) is one migration in one transaction and one
// audited {from,to} step: it commits entirely or not at all.
//
// A case already at the current schema is left alone and nothing is audited,
// except that a dangling case.upgrade (announced, migration committed, process
// died before the conclusion) whose `to` is the current version gets a
// case.upgrade.done{resumed:true} (UpgradeResult.Resumed). A dangling upgrade whose `to` is
// the database's version but not the current one (an older build announced 1 to 2) is concluded
// the same way first, and the upgrade then goes on.
//
// If the database schema version disagrees with the version the audit log says
// it should have (and no announced upgrade explains it), Upgrade refuses with
// ErrIntegrity before announcing or changing anything.
func (c *Case) Upgrade() (UpgradeResult, error) {
	from, err := c.store.SchemaVersion()
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("artifacts.db schema_version: %w", err)
	}
	to := CurrentSchema
	if from > to {
		return UpgradeResult{}, fmt.Errorf("artifacts.db schema version %d is newer than this build supports (%d)", from, to)
	}
	dangling, err := c.checkSchemaMatchesAudit(from)
	if err != nil {
		return UpgradeResult{From: from, To: from}, err
	}
	if from == to {
		concluded, err := c.resumeUpgrade(dangling, from)
		res := UpgradeResult{From: from, To: to}
		res.setResumed(concluded)
		if err != nil {
			return res, err
		}
		res.RecordsToIndex, err = c.recordsToIndex(to)
		return res, err
	}
	concluded, err := c.resumeUpgrade(dangling, from)
	if err != nil {
		return UpgradeResult{From: from, To: from}, err
	}
	res := UpgradeResult{From: from, To: from} // what has happened so far
	res.setResumed(concluded)
	if _, err := c.Audit.Append(ActionCaseUpgrade, "", upgradeDetails(from, to)); err != nil {
		return res, err
	}
	if c.upgradeHook != nil {
		c.upgradeHook()
	}
	if err := c.store.migrateTo(to); err != nil {
		_, aerr := c.Audit.Append(ActionCaseUpgradeError, "", upgradeErrorDetails(from, to, err))
		return res, errors.Join(err, aerr)
	}
	res.To, res.Upgraded = to, true
	if _, err := c.Audit.Append(ActionCaseUpgradeDone, "", upgradeDoneDetails(from, to, false)); err != nil {
		// The migration is committed; the next Upgrade completes the record.
		return res, err
	}
	res.RecordsToIndex, err = c.recordsToIndex(to)
	return res, err
}

// recordsToIndex is the number of records an unbuilt full-text index leaves unsearchable: the
// record count when the index state is "unbuilt", 0 for any other state and for a schema older
// than v3 (which has no index).
func (c *Case) recordsToIndex(schema int) (int64, error) {
	if schema < 3 {
		return 0, nil
	}
	var value string
	if err := c.store.db.QueryRow(`SELECT value FROM records_meta WHERE key = ?`, MetaFTSNormVersion).Scan(&value); err != nil {
		return 0, fmt.Errorf("read %s: %w", MetaFTSNormVersion, err)
	}
	if classifyIndexValue(value, FTSNormVersion()) != IndexUnbuilt {
		return 0, nil
	}
	var n int64
	if err := c.store.db.QueryRow(`SELECT count(*) FROM records`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count records: %w", err)
	}
	return n, nil
}

// checkSchemaMatchesAudit refuses (ErrIntegrity) to go on when the database
// schema version disagrees with what the audit log says it must be, unless an
// announced, unconcluded upgrade explains it (the migration committed and the
// process died before case.upgrade.done). It returns that dangling upgrade, nil
// when there is none. Nothing is audited and nothing is changed before it
// returns: the case is inconsistent, and `case verify` reports why.
func (c *Case) checkSchemaMatchesAudit(dbVersion int) (*upgradeStep, error) {
	entries, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil {
		if errors.Is(err, errAuditCorrupt) {
			return nil, fmt.Errorf("%w: %w", ErrIntegrity, err)
		}
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	sa := auditedSchema(entries)
	if len(sa.Problems) > 0 {
		return nil, fmt.Errorf("%w: the audit log's schema history is inconsistent (%s); run: minutiae case verify --case %s", ErrIntegrity, sa.Problems[0], c.Dir)
	}
	d := sa.Dangling
	if dbVersion == sa.Version || (d != nil && sa.Version == d.From && dbVersion == d.To) {
		return d, nil
	}
	return nil, fmt.Errorf("%w: artifacts.db is at schema v%d but the audit log says v%d; refusing to upgrade (run: minutiae case verify --case %s)", ErrIntegrity, dbVersion, sa.Version, c.Dir)
}

// resumeUpgrade audits the completion of the dangling upgrade when its `to` is
// the current version, and returns the step it concluded (nil when there was none).
func (c *Case) resumeUpgrade(d *upgradeStep, current int) (*upgradeStep, error) {
	if d == nil || d.To != current {
		return nil, nil
	}
	if _, err := c.Audit.Append(ActionCaseUpgradeDone, "", upgradeDoneDetails(d.From, d.To, true)); err != nil {
		return nil, err
	}
	return d, nil
}

// setResumed records the concluded step (nil: none) in the result.
func (r *UpgradeResult) setResumed(d *upgradeStep) {
	if d != nil {
		r.Resumed, r.ResumedFrom, r.ResumedTo = true, d.From, d.To
	}
}
