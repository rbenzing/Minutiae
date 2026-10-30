package artparse

import (
	"context"
	"crypto/sha1" //nolint:gosec // the iOS backup file ID is defined as a SHA-1 of "<domain>-<relativePath>"; here it is an identifier check, not a security control
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/iosbackup"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// Backup resolution statuses.
const (
	BackupMapped   = "mapped"
	BackupUnparsed = "unparsed"
	BackupRefused  = "refused" // integrity
)

// Encryption states of a BackupResolution.
const (
	encEncrypted    = "encrypted"
	encNotEncrypted = "not-encrypted"
	encUnknown      = "unknown"
	encUndecided    = "undecided"
)

// Reasons the host gives (the text after the status of a plan line).
const (
	reasonEncrypted  = "encrypted backup (sub-project 10)"
	reasonMetadata   = "backup metadata file"
	reasonNotAPath   = "not a backup file path"
	reasonUnresolved = "backup not resolved in this snapshot"
	reasonNoManifest = "no Manifest.db in this backup"
	reasonNotListed  = "file ID not listed in Manifest.db"
	reasonDuplicate  = "listed more than once"
	reasonTooLong    = "logical path is too long"
	reasonIncomplete = "Manifest.db is incomplete"
	reasonNoPlist    = "no Manifest.plist in this backup"
)

// ArtifactRef names an artifact by its id and SHA-256.
type ArtifactRef struct{ ArtifactID, SHA256 string }

// BackupResolution is what the host decided about one iOS backup (one backup directory of one acquisition).
// ManifestDB and ManifestPlist say which record was IDENTIFIED for the backup, not that it was verified:
// an incomplete or over-size Manifest.plist (and an incomplete Manifest.db) is named but not loaded, and
// Reason says so (too large, incomplete).
type BackupResolution struct {
	Acquisition, Dir string       // acquisitionKey, the <dir> path element
	ManifestDB       *ArtifactRef // nil when absent
	ManifestPlist    *ArtifactRef
	Encryption       string // "encrypted" | "not-encrypted" | "unknown" | "undecided"
	Status, Reason   string
	Integrity        bool
	Mapped, Unmapped int // backup artifacts of this backup given / not given a logical path
	FileIDMismatch   int
}

// BackupMap is the mapping of one resolved backup: its resolution and, for each backup file of the
// snapshot, the logical path or the reason there is none. The Manifest.db rows are read once, at
// resolution; nothing of them is retained (4E-R40 keeps the *iosbackup.Files, this keeps its answers, so
// its budget is released as soon as the resolution is done).
type BackupMap struct {
	Resolution BackupResolution
	names      map[string]backupName // by file ID
	// listed are the logical paths of the -wal/-journal rows the Manifest.db lists, held by the case or not (bounded by
	// iosbackup.MaxCompanionRows; listedDropped counts the rest)
	listed        map[string]struct{}
	listedDropped int64
}

type backupName struct{ logical, reason string }

// Logical answers for one file ID of the backup: the logical path, or the reason it has none. A file ID
// the resolution did not ask about is never "not listed": that is undecided.
func (b *BackupMap) Logical(fileID string) (logical, reason string, ok bool) {
	if b == nil {
		return "", reasonNoManifest, false
	}
	n, found := b.names[fileID]
	if !found {
		return "", "undecided: the file ID is not a file of this backup in this snapshot", false
	}
	return n.logical, n.reason, n.logical != ""
}

// backupKey is the key of Env.Backups.
func backupKey(acquisition, dir string) string { return acquisition + "\x00" + dir }

// backupPathOf splits a backup record into its acquisition, backup directory and file ID.
func backupPathOf(m evidence.ManifestRecord) (acq, dir, fileID string, ok bool) {
	if m.Source.Kind != "backup" {
		return "", "", "", false
	}
	acq, ok = acquisitionKey(m.Path)
	if !ok {
		return "", "", "", false
	}
	dir, fileID, ok = iosbackup.SplitRemote(m.Source.RemotePath)
	return acq, dir, fileID, ok
}

// backupDirOf is the backup directory of any backup record: the first element of a remote path of at
// least two elements.
func backupDirOf(remote string) (dir string, ok bool) {
	dir, rest, found := strings.Cut(remote, "/")
	return dir, found && dir != "" && rest != ""
}

// metaName is the name of a backup metadata file at <dir>/<name>, or "".
func metaName(remote string) string {
	parts := strings.Split(remote, "/")
	if len(parts) != 2 || parts[0] == "" {
		return ""
	}
	switch parts[1] {
	case "Manifest.db", "Manifest.db-wal", "Manifest.db-journal", "Manifest.plist", "Manifest.mbdb", "Status.plist", "Info.plist":
		return parts[1]
	}
	return ""
}

// iosBackupNamer names a backup file `ios:<domain>/<relativePath>` from the Manifest.db of its own
// acquisition and backup directory (4E-R2), and from nothing else.
type iosBackupNamer struct{}

func (iosBackupNamer) Name() string { return "ios-backup" }

func (iosBackupNamer) Logical(m evidence.ManifestRecord, env Env) (Logical, bool) {
	acq, dir, fileID, ok := backupPathOf(m)
	if !ok || env.Backups == nil {
		return Logical{}, false
	}
	bm := env.Backups[backupKey(acq, dir)]
	if bm == nil {
		return Logical{}, false
	}
	logical, _, ok := bm.Logical(fileID)
	if !ok {
		return Logical{}, false
	}
	return Logical{Path: logical, Platform: parse.PlatformIOS, Namer: "ios-backup"}, true
}

// whyNoBackupLogical says why a backup record has no logical path (4E-R40: a snapshot that was never
// resolved says so, it never claims the Manifest.db is missing).
func whyNoBackupLogical(m evidence.ManifestRecord, env Env) string {
	if metaName(m.Source.RemotePath) != "" {
		return reasonMetadata
	}
	if _, ok := acquisitionKey(m.Path); !ok {
		return "manifest path outside the acquisition layout"
	}
	acq, dir, fileID, ok := backupPathOf(m)
	if !ok {
		return reasonNotAPath
	}
	if env.Backups == nil {
		return reasonUnresolved
	}
	bm := env.Backups[backupKey(acq, dir)]
	if bm == nil {
		return reasonNoManifest
	}
	if _, reason, found := bm.Logical(fileID); !found && reason != "" {
		return reason
	}
	return "no logical path"
}

// backupFile is a backup record shaped <dir>/<aa>/<fileID>.
type backupFile struct {
	rec    evidence.ManifestRecord
	fileID string
}

// backupGroup is the backup records of one (acquisition, backup directory).
type backupGroup struct {
	acq, dir string
	meta     map[string][]evidence.ManifestRecord // by metadata file name
	files    []backupFile
	other    []evidence.ManifestRecord // backup records of neither shape
}

func backupGroups(snap *Snapshot) []*backupGroup {
	byKey := map[string]*backupGroup{}
	var order []*backupGroup
	for _, m := range snap.records {
		if m.Source.Kind != "backup" {
			continue
		}
		acq, ok := acquisitionKey(m.Path)
		if !ok {
			continue
		}
		dir, ok := backupDirOf(m.Source.RemotePath)
		if !ok {
			continue
		}
		k := backupKey(acq, dir)
		g := byKey[k]
		if g == nil {
			g = &backupGroup{acq: acq, dir: dir, meta: map[string][]evidence.ManifestRecord{}}
			byKey[k] = g
			order = append(order, g)
		}
		switch name := metaName(m.Source.RemotePath); {
		case name != "":
			g.meta[name] = append(g.meta[name], m)
		default:
			if _, _, fid, ok := backupPathOf(m); ok {
				g.files = append(g.files, backupFile{rec: m, fileID: fid})
			} else {
				g.other = append(g.other, m)
			}
		}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].acq != order[j].acq {
			return order[i].acq < order[j].acq
		}
		return order[i].dir < order[j].dir
	})
	return order
}

// loadedSet holds the sources one backup's resolution opened and the in-memory budget still free.
type loadedSet struct {
	srcs    []*source
	memLeft int64
}

func (l *loadedSet) closeAll() {
	for _, s := range l.srcs {
		s.close()
	}
}

// finding is an integrity finding of a backup file.
type finding struct{ reason string }

// load opens one backup file through loadSource. An integrity failure is a finding (never an error); a
// context that ended and every other failure are the error.
func (h *Host) load(ctx context.Context, snap *Snapshot, rec evidence.ManifestRecord, memMax int64, lg *loadedSet) (*source, *finding, error) {
	s, err := h.loadSource(ctx, snap, rec.ID, memMax, lg.memLeft)
	switch {
	case errors.Is(err, evidence.ErrIntegrity):
		return nil, &finding{reason: "integrity: " + cleanAuditText(h.caseRelative(err.Error()), maxReason)}, nil
	case err != nil:
		return nil, nil, err
	}
	if s.data != nil {
		lg.memLeft -= int64(len(s.data))
	}
	lg.srcs = append(lg.srcs, s)
	return s, nil, nil
}

// rehash hashes the streamed sources again, now that they have been read: a file that changed while it was
// read is an integrity finding.
func (h *Host) rehash(ctx context.Context, lg *loadedSet) (*finding, error) {
	for _, s := range lg.srcs {
		if s.f == nil {
			continue
		}
		got, _, err := h.hashNow(ctx, s.rec)
		switch {
		case errors.Is(err, evidence.ErrIntegrity):
			return &finding{reason: "integrity: " + cleanAuditText(h.caseRelative(err.Error()), maxReason)}, nil
		case err != nil:
			return nil, err
		case got != s.rec.SHA256:
			e := &HashMismatchError{ArtifactID: s.rec.ID, Path: s.rec.Path, Want: s.rec.SHA256, Got: got}
			return &finding{reason: "integrity: " + cleanAuditText(h.caseRelative(e.Error()), maxReason)}, nil
		}
	}
	return nil, nil
}

func refOf(m evidence.ManifestRecord) *ArtifactRef {
	return &ArtifactRef{ArtifactID: m.ID, SHA256: m.SHA256}
}

// dbKind is what reading a Manifest.db came to.
type dbKind int

const (
	dbReadable dbKind = iota
	dbLooksEncrypted
	dbOther // not a database, or no usable Files table, or over a cap
)

// ResolveBackups opens, hash-checks and reads every backup's Manifest.db (with its -wal/-journal of the
// same acquisition and directory) and Manifest.plist, stores the mappings in snap (its Env, which the
// ios-backup namer reads), and returns one resolution per (acquisition, backup directory) holding backup
// artifacts, sorted by Acquisition then Dir. It is idempotent per snapshot. Only a context that ended or
// an I/O failure is an error; every evidence finding is a resolution. On an error the resolutions made so
// far are returned with it, an integrity finding included (4E-R38).
func (h *Host) ResolveBackups(ctx context.Context, snap *Snapshot) ([]BackupResolution, error) {
	if snap.resolved {
		return slices.Clone(snap.resolutions), nil
	}
	newBudget := parse.NewBudget
	if h.resolveBudget != nil {
		newBudget = h.resolveBudget
	}
	bud := newBudget(h.limits.MemBudget)
	maps := map[string]*BackupMap{}
	var out []BackupResolution
	for _, g := range backupGroups(snap) {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		bm, err := h.resolveGroup(ctx, snap, g, bud)
		if err != nil {
			return out, err
		}
		out = append(out, bm.Resolution)
		maps[backupKey(g.acq, g.dir)] = bm
	}
	snap.env.Backups, snap.resolutions, snap.resolved = maps, out, true
	return slices.Clone(out), nil
}

// resolveGroup resolves one backup. Everything it opens or charges is closed or released on every path.
func (h *Host) resolveGroup(ctx context.Context, snap *Snapshot, g *backupGroup, bud *parse.Budget) (*BackupMap, error) {
	res := BackupResolution{Acquisition: g.acq, Dir: g.dir, Encryption: encUnknown}
	lg := &loadedSet{memLeft: h.limits.MemInputTotal}
	defer lg.closeAll()
	var files *iosbackup.Files
	defer func() {
		if files != nil {
			files.Release()
		}
	}()
	var err error
	files, err = h.decideBackup(ctx, snap, g, bud, lg, &res)
	if err != nil {
		return nil, err
	}
	return h.nameBackup(g, res, files), nil
}

func (res *BackupResolution) set(status, encryption, reason string) {
	res.Status, res.Reason = status, reason
	if encryption != "" {
		res.Encryption = encryption
	}
}

func (res *BackupResolution) refuse(f *finding) {
	res.Status, res.Reason, res.Integrity = BackupRefused, f.reason, true
}

// decideBackup reads the backup's metadata and decides its status (4E-R4). It returns the Files the
// mapping was read from when the backup is mapped (the caller releases them); res is filled in.
func (h *Host) decideBackup(ctx context.Context, snap *Snapshot, g *backupGroup, bud *parse.Budget, lg *loadedSet, res *BackupResolution) (*iosbackup.Files, error) {
	dbs, plists := g.meta["Manifest.db"], g.meta["Manifest.plist"]
	wals, journals := g.meta["Manifest.db-wal"], g.meta["Manifest.db-journal"]
	switch {
	case len(dbs) == 0 && len(g.meta["Manifest.mbdb"]) > 0:
		res.set(BackupUnparsed, "", "Manifest.mbdb backups are not mapped")
		return nil, nil
	case len(dbs) == 0:
		res.set(BackupUnparsed, "", reasonNoManifest)
		return nil, nil
	case len(dbs) > 1 || len(wals) > 1 || len(journals) > 1:
		res.set(BackupUnparsed, "", "ambiguous: more than one Manifest.db (or -wal/-journal) in this backup")
		return nil, nil
	}
	db := dbs[0]
	res.ManifestDB = refOf(db)
	if len(plists) == 1 {
		res.ManifestPlist = refOf(plists[0])
	}
	sidecars := append(append([]evidence.ManifestRecord{db}, wals...), journals...)
	for _, m := range sidecars {
		if m.Incomplete {
			res.set(BackupUnparsed, "", reasonIncomplete)
			return nil, nil
		}
	}

	enc, encReason := iosbackup.EncUnknown, reasonNoPlist
	switch {
	case len(plists) > 1:
		encReason = "ambiguous: more than one Manifest.plist in this backup"
	case len(plists) == 1:
		var f *finding
		var err error
		if enc, encReason, f, err = h.readEncryption(ctx, snap, plists[0], lg); err != nil {
			return nil, err
		} else if f != nil {
			res.refuse(f)
			return nil, nil
		}
	}

	var f *finding
	srcs := make([]*source, 0, 3)
	for _, m := range sidecars {
		s, fd, err := h.load(ctx, snap, m, h.limits.MemInputMax, lg)
		if err != nil {
			return nil, err
		}
		if fd != nil {
			f = fd
			break
		}
		srcs = append(srcs, s)
	}
	if f != nil {
		res.refuse(f)
		return nil, nil
	}
	in := sqlitedb.Files{DB: srcs[0], DBSize: db.Size}
	if len(wals) == 1 {
		in.WAL, in.WALSize = srcs[1], wals[0].Size
	}
	if len(journals) == 1 {
		in.Journal, in.JournalSize = srcs[len(srcs)-1], journals[0].Size
	}

	// want is built before the read and never changes: Lookup asks it to tell "not listed" from
	// "never requested".
	ids := make(map[string]struct{}, len(g.files))
	for _, bf := range g.files {
		ids[bf.fileID] = struct{}{}
	}
	maxRows := h.maxFilesRows
	files, rerr := iosbackup.ReadFiles(ctx, in, bud, func(id string) bool { _, ok := ids[id]; return ok }, iosbackup.Options{MaxRows: maxRows})
	if ctx.Err() != nil {
		if files != nil {
			files.Release()
		}
		return nil, ctx.Err()
	}
	kind, dbReason := dbOther, ""
	switch {
	case rerr == nil:
		kind = dbReadable
		if h.afterManifestRead != nil {
			h.afterManifestRead()
		}
	case errors.Is(rerr, sqlitedb.ErrEncrypted):
		kind = dbLooksEncrypted
	default:
		dbReason = h.manifestReason(rerr, maxRows)
	}
	// The streamed files were hashed once before the read; hash them again now, so a file that changed
	// while it was read is found (a read of a changing file proves nothing).
	fd, herr := h.rehash(ctx, lg)
	if herr == nil && ctx.Err() != nil {
		herr = ctx.Err()
	}
	if herr != nil || fd != nil {
		if files != nil {
			files.Release()
		}
		if herr != nil {
			return nil, herr
		}
		res.refuse(fd)
		return nil, nil
	}
	if kind != dbReadable && files != nil {
		files.Release()
		files = nil
	}
	status, encryption, reason := decideStatus(enc, encReason, kind, dbReason)
	res.set(status, encryption, cleanAuditText(reason, maxReason))
	if status != BackupMapped && files != nil {
		files.Release()
		files = nil
	}
	return files, nil
}

// readEncryption loads Manifest.plist (never one over MaxManifestPlistBytes, 4E-R36) and reads its
// IsEncrypted key. A reason is returned whenever the answer is EncUnknown.
func (h *Host) readEncryption(ctx context.Context, snap *Snapshot, p evidence.ManifestRecord, lg *loadedSet) (iosbackup.Encryption, string, *finding, error) {
	switch {
	case p.Incomplete:
		return iosbackup.EncUnknown, "Manifest.plist is incomplete", nil, nil
	case p.Size > iosbackup.MaxManifestPlistBytes:
		return iosbackup.EncUnknown, fmt.Sprintf("Manifest.plist is too large (%d bytes, limit %d)", p.Size, iosbackup.MaxManifestPlistBytes), nil, nil
	}
	// The plist decoder never frees a successful charge (the budget is dropped with the invocation), so
	// it gets a budget of its own, which holds the raw bytes too.
	bud := parse.NewBudget(h.limits.MemBudget)
	if err := bud.Alloc(p.Size); err != nil {
		return iosbackup.EncUnknown, "Manifest.plist needs more memory than the budget allows", nil, nil
	}
	// Always read into memory: it is at most 16 MiB and ReadEncryption needs the bytes.
	saved := lg.memLeft
	lg.memLeft = iosbackup.MaxManifestPlistBytes
	s, f, err := h.load(ctx, snap, p, iosbackup.MaxManifestPlistBytes, lg)
	lg.memLeft = saved
	if err != nil || f != nil {
		return iosbackup.EncUnknown, "", f, err
	}
	lg.memLeft = max(saved-int64(len(s.data)), 0) // the plist bytes count toward MemInputTotal
	enc, reason := iosbackup.ReadEncryption(s.data, bud)
	return enc, reason, nil, nil
}

// manifestReason is the reason a Manifest.db could not be read, from the typed error (4E-R8).
func (h *Host) manifestReason(err error, maxRows int) string {
	var ue *sqlitedb.UnsupportedSchemaError
	switch {
	case errors.Is(err, sqlitedb.ErrNotSQLite):
		return "Manifest.db is not a database"
	case errors.Is(err, sqlitedb.ErrNoSuchTable):
		return "Manifest.db has no Files table"
	case errors.As(err, &ue):
		return "Manifest.db has an unsupported Files table (missing columns: " + cleanAuditText(strings.Join(ue.Missing, ", "), 128) + ")"
	case errors.Is(err, iosbackup.ErrTooLarge):
		return "Manifest.db is larger than 1 GiB"
	case errors.Is(err, iosbackup.ErrTooManyRows):
		if maxRows <= 0 {
			maxRows = iosbackup.MaxFilesRows
		}
		return fmt.Sprintf("Manifest.db has more Files rows than %d", maxRows)
	case errors.Is(err, parse.ErrBudget):
		return "Manifest.db needs more memory than the budget allows"
	}
	return "Manifest.db unreadable: " + cleanAuditText(h.caseRelative(err.Error()), 256)
}

// decideStatus is the decision table of 4E-R4.
func decideStatus(enc iosbackup.Encryption, encReason string, kind dbKind, dbReason string) (status, encryption, reason string) {
	switch enc {
	case iosbackup.EncYes:
		if kind == dbReadable {
			return BackupMapped, encEncrypted, ""
		}
		return BackupUnparsed, encEncrypted, reasonEncrypted
	case iosbackup.EncNo:
		switch kind {
		case dbReadable:
			return BackupMapped, encNotEncrypted, ""
		case dbLooksEncrypted:
			return BackupUnparsed, encUndecided, "Manifest.plist says not encrypted but Manifest.db looks encrypted"
		}
		return BackupUnparsed, encNotEncrypted, dbReason
	}
	switch kind {
	case dbReadable:
		return BackupMapped, encUnknown, encReason
	case dbLooksEncrypted:
		return BackupUnparsed, encEncrypted, reasonEncrypted
	}
	return BackupUnparsed, encUnknown, dbReason
}

// nameBackup answers for every backup file of the group, counts them and builds the BackupMap. files is
// non-nil exactly when the backup is mapped.
func (h *Host) nameBackup(g *backupGroup, res BackupResolution, files *iosbackup.Files) *BackupMap {
	bm := &BackupMap{names: make(map[string]backupName, len(g.files))}
	unmapped := res.Reason
	if res.Status == BackupRefused {
		unmapped = "Manifest.db failed its integrity check"
	}
	for _, bf := range g.files {
		var n backupName
		if files == nil {
			n = backupName{reason: unmapped}
		} else {
			n = nameOf(bf.fileID, files, &res)
		}
		bm.names[bf.fileID] = n
		if n.logical == "" {
			res.Unmapped++
		} else {
			res.Mapped++
		}
	}
	if files != nil {
		cs, dropped := files.Companions()
		bm.listed, bm.listedDropped = make(map[string]struct{}, len(cs)), dropped
		for _, e := range cs {
			bm.listed["ios:"+e.Domain+"/"+e.RelativePath] = struct{}{}
		}
	}
	res.Unmapped += len(g.other)
	bm.Resolution = res
	return bm
}

// nameOf is the logical path or the reason of one file ID of a mapped backup.
func nameOf(fileID string, files *iosbackup.Files, res *BackupResolution) backupName {
	e, st, why := files.Lookup(fileID)
	switch st {
	case iosbackup.Mapped:
		logical := "ios:" + e.Domain + "/" + e.RelativePath
		if len(logical) > maxLogicalPath {
			return backupName{reason: reasonTooLong}
		}
		sum := sha1.Sum([]byte(e.Domain + "-" + e.RelativePath)) //nolint:gosec // see the import
		if hex.EncodeToString(sum[:]) != fileID {
			res.FileIDMismatch++ // counted, never unmapped: the Files row is the device's own statement (4E-R3)
		}
		return backupName{logical: logical}
	case iosbackup.NotListed:
		return backupName{reason: reasonNotListed}
	case iosbackup.Duplicate:
		return backupName{reason: reasonDuplicate}
	case iosbackup.Undecided:
		return backupName{reason: cleanAuditText("undecided: "+why, maxReason)}
	}
	return backupName{reason: cleanAuditText(why, maxReason)}
}

// snapshotFor is the one way an entry point takes a snapshot that names backup files (4E-R38): the
// snapshot and its backup resolutions. On an error the snapshot (when there is one) and the resolutions
// made so far are returned with it.
func (h *Host) snapshotFor(ctx context.Context) (*Snapshot, []BackupResolution, error) {
	snap, err := h.TakeSnapshot()
	if err != nil {
		return nil, nil, err
	}
	rs, err := h.ResolveBackups(ctx, snap)
	return snap, rs, err
}

// encryptedBackup reports whether any member of the job belongs to an encrypted backup.
func encryptedBackup(j Job, env Env) bool {
	enc := func(m Member) bool {
		acq, dir, _, ok := backupPathOf(m.Artifact)
		if !ok || env.Backups == nil {
			return false
		}
		bm := env.Backups[backupKey(acq, dir)]
		return bm != nil && bm.Resolution.Encryption == encEncrypted
	}
	if enc(j.Primary) {
		return true
	}
	for _, m := range j.Others {
		if enc(m) {
			return true
		}
	}
	return false
}

// backupWarnings are the host warnings of a run, one per backup that is not mapped and one per mapped encrypted or
// encryption-unknown backup (4E-R5, R37).
func backupWarnings(parseID string, rs []BackupResolution) []HostWarning {
	var out []HostWarning
	for _, r := range rs {
		var reason string
		switch {
		case r.Status != BackupMapped:
			reason = r.Reason
		case r.Encryption == encEncrypted:
			reason = "encrypted backup: Manifest.db is readable but the file contents are encrypted and are not parsed (sub-project 10)"
		case r.Encryption == encUnknown:
			reason = "encryption unknown: " + r.Reason + "; each database's Probe decides"
		default:
			continue
		}
		out = append(out, HostWarning{
			AnalysisID: parseID, Path: "backup:" + cleanAuditText(r.Acquisition, maxPathText) + "/" + cleanAuditText(r.Dir, maxPathText),
			Reason: cleanAuditText(reason, maxReason), Source: sourceHost,
		})
	}
	return out
}

func auditRef(r *ArtifactRef) *AuditArtifactRef {
	if r == nil {
		return nil
	}
	return &AuditArtifactRef{ArtifactID: r.ArtifactID, SHA256: r.SHA256}
}

// auditBackups is the always-present backups array of analysis.start.
func auditBackups(rs []BackupResolution) []AuditBackup {
	out := make([]AuditBackup, 0, len(rs))
	for _, r := range rs {
		out = append(out, AuditBackup{
			Acquisition: cleanAuditText(r.Acquisition, maxPathText), Dir: cleanAuditText(r.Dir, maxPathText),
			ManifestDB: auditRef(r.ManifestDB), ManifestPlist: auditRef(r.ManifestPlist),
			Encryption: r.Encryption, Status: r.Status, Reason: cleanAuditText(r.Reason, maxReason),
			Mapped: r.Mapped, Unmapped: r.Unmapped, FileIDMismatch: r.FileIDMismatch,
		})
	}
	return out
}

// backupOf is the acquisition and backup directory of any backup record.
func backupOf(m evidence.ManifestRecord) (acq, dir string, ok bool) {
	if m.Source.Kind != "backup" {
		return "", "", false
	}
	if acq, ok = acquisitionKey(m.Path); !ok {
		return "", "", false
	}
	dir, ok = backupDirOf(m.Source.RemotePath)
	return acq, dir, ok
}

// unnamedBackupFileReason says why a backup data file has no logical path in a resolved snapshot ("" when
// it has one or is not a backup data file). ClaimedTables must not read that as "no parser claims it":
// the file may be a claimed table in a database nobody could name.
func unnamedBackupFileReason(snap *Snapshot, rec evidence.ManifestRecord) string {
	acq, dir, fileID, ok := backupPathOf(rec)
	if !ok {
		return ""
	}
	bm := snap.env.Backups[backupKey(acq, dir)]
	if bm == nil {
		return "backup unparsed: " + reasonNoManifest
	}
	if bm.Resolution.Status != BackupMapped {
		return "backup unparsed: " + bm.Resolution.Reason
	}
	if _, why, found := bm.Logical(fileID); !found {
		return why
	}
	return ""
}

// BackupSiblings is what an iOS backup's Manifest.db says about the companions of one backup database file.
type BackupSiblings struct {
	Named        bool         // the backup was resolved and the artifact has a logical path in it
	WAL, Journal string       // artifact ids of <logical>-wal and <logical>-journal in the same backup; "" when the manifest names none
	ManifestDB   *ArtifactRef // the hash-checked Manifest.db that named them; nil when not Named
	Reason       string       // why not Named (encrypted backup, no Manifest.db, not a backup file, ...)
	Logical      string       // the database's logical path in the backup (when Named)

	// Unknown is never absent (4h-R8): what the Manifest.db says that the case cannot confirm.
	WalListedNotHeld, JournalListedNotHeld bool  // the Manifest.db lists <logical>-wal / -journal, the case holds no such file
	UnmappedHeld                           int   // held files of this backup with no logical path (a companion may be among them)
	ListedUnchecked                        int64 // listed companion paths beyond the bound, not checked
}

// BackupSiblings resolves the case's backups (snapshotFor) and returns the siblings of artifactID. A Manifest.db
// that fails its hash check is an error matching evidence.ErrIntegrity.
func (h *Host) BackupSiblings(ctx context.Context, artifactID string) (BackupSiblings, error) {
	snap, rs, err := h.snapshotFor(ctx)
	if err != nil {
		return BackupSiblings{}, err
	}
	// every backup of the case is resolved, as a recovery does: a Manifest.db that fails its hash check names
	// nothing, and it is never read as "no siblings"
	for _, r := range rs {
		if r.Integrity {
			return BackupSiblings{}, fmt.Errorf("%w: backup %s/%s: %s", evidence.ErrIntegrity, cleanAuditText(r.Acquisition, maxPathText), cleanAuditText(r.Dir, maxPathText), cleanAuditText(r.Reason, maxReason))
		}
	}
	rec, ok := snap.Record(artifactID)
	if !ok {
		return BackupSiblings{}, fmt.Errorf("%w: %q", evidence.ErrUnknownArtifact, artifactID)
	}
	acq, dir, fileID, ok := backupPathOf(rec)
	if !ok {
		return BackupSiblings{Reason: whyNoBackupLogical(rec, snap.env)}, nil
	}
	bm := snap.env.Backups[backupKey(acq, dir)]
	switch {
	case bm == nil:
		return BackupSiblings{Reason: reasonNoManifest}, nil
	case bm.Resolution.Encryption == encEncrypted:
		return BackupSiblings{Reason: reasonEncrypted}, nil
	case bm.Resolution.Status != BackupMapped:
		return BackupSiblings{Reason: bm.Resolution.Reason}, nil
	}
	logical, why, ok := bm.Logical(fileID)
	if !ok {
		if why == "" {
			why = "no logical path"
		}
		return BackupSiblings{Reason: why}, nil
	}
	out := BackupSiblings{Named: true, ManifestDB: bm.Resolution.ManifestDB, Logical: logical}
	out.ListedUnchecked = bm.listedDropped
	for _, m := range snap.records { // held files of this backup that the Manifest.db gave no logical path
		if a2, d2, f2, ok := backupPathOf(m); ok && a2 == acq && d2 == dir {
			if _, _, named := bm.Logical(f2); !named {
				out.UnmappedHeld++
			}
		}
	}
	for _, c := range []struct {
		suffix  string
		into    *string
		notHeld *bool
	}{{"-wal", &out.WAL, &out.WalListedNotHeld}, {"-journal", &out.Journal, &out.JournalListedNotHeld}} {
		var found []string
		for _, m := range snap.records {
			a2, d2, f2, ok := backupPathOf(m)
			if !ok || a2 != acq || d2 != dir {
				continue
			}
			if l2, _, named := bm.Logical(f2); named && l2 == logical+c.suffix {
				found = append(found, m.ID)
			}
		}
		switch len(found) {
		case 0:
			_, *c.notHeld = bm.listed[logical+c.suffix]
		case 1:
			*c.into = found[0]
		default:
			return BackupSiblings{Reason: fmt.Sprintf("ambiguous: %d files of the backup are listed as %s%s", len(found), cleanAuditText(logical, maxPathText), c.suffix)}, nil
		}
	}
	return out, nil
}

// Warnings says, one line each, what the Manifest.db listed or left unnamed that the pairing could not use.
func (b BackupSiblings) Warnings() []string {
	var out []string
	if b.WalListedNotHeld {
		out = append(out, fmt.Sprintf("the Manifest.db lists %s-wal but the case holds no such file", b.Logical))
	}
	if b.JournalListedNotHeld {
		out = append(out, fmt.Sprintf("the Manifest.db lists %s-journal but the case holds no such file", b.Logical))
	}
	if b.UnmappedHeld > 0 {
		out = append(out, fmt.Sprintf("%d backup files of this backup have no logical path; a companion may be among them", b.UnmappedHeld))
	}
	if b.ListedUnchecked > 0 {
		out = append(out, fmt.Sprintf("%d listed -wal/-journal paths beyond the bound were not checked; a companion may be among them", b.ListedUnchecked))
	}
	return out
}
