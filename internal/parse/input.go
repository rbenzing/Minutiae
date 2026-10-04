package parse

import "io"

// SnapshotInfo names the filesystem snapshot an artifact was extracted from.
type SnapshotInfo struct {
	Name string
	Xid  uint64
}

// SourceInfo is what the manifest records about where an artifact came from.
// Its strings come from a device and are untrusted text.
type SourceInfo struct {
	Kind, DeviceID, RemotePath, OriginalPath, Partition, FSType, FSPath string
	Encrypted, ParentIncomplete                                         bool
	Snapshot                                                            *SnapshotInfo // non-nil for an artifact extracted from a filesystem snapshot
}

// RecoveryInfo describes how a recovered or carved artifact was obtained.
type RecoveryInfo struct {
	Class, Method string
	Confidence    *int
}

// Artifact is one verified input. R is read-only, bounded and sealed when the
// job ends (reads then fail with ErrSealed); it is nil in Lookuper results.
type Artifact struct {
	ID, SHA256, Platform, Logical string
	Size                          int64
	Incomplete                    bool
	Recovery                      *RecoveryInfo // non-nil only for recovered or carved artifacts
	Source                        SourceInfo
	R                             io.ReaderAt
}

// JobInfo identifies the invocation.
type JobInfo struct {
	ParseID string
	Job     int
	Parser  Identity
}

// Lookuper finds and opens other artifacts of the case, read-only.
type Lookuper interface {
	// Find returns the artifacts whose logical path matches the glob, sorted
	// by artifact id. R is nil in the results.
	Find(logicalGlob string) []Artifact
	// Open returns a reader for an artifact Find returned, under the same
	// hash-verified, sealed and bounded rules as the bundle.
	Open(a Artifact) (io.ReaderAt, error)
}

// Input is everything a parser invocation receives. Every value reachable
// from an Input handed to a parser is a copy the host made for that
// invocation (see Clone), except the reader, the Lookuper and the BudgetView,
// which are the host's single sealed handles for that invocation.
type Input struct {
	Job       JobInfo
	Primary   Artifact
	Artifacts map[string]Artifact // by role, primary included; absent optional roles are missing
	Lookup    Lookuper
	Limits    Limits
	Budget    *BudgetView // a read-mostly view of the host's budget
}
