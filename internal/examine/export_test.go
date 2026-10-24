package examine

import (
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
)

// SetNewArtifact replaces how s creates artifacts, to inject faults.
func (s *Session) SetNewArtifact(f func(deviceID, acqID, rel string, src evidence.Source) (*evidence.ArtifactWriter, error)) {
	s.newArtifact = f
}

// WrapFS exposes the panic-protecting wrapper to the external tests.
func WrapFS(name string, fsys filesys.FileSystem) (filesys.FileSystem, error) {
	return wrapFS(name, fsys)
}

// SetFreeBytes replaces how s asks for free disk space, to test the space check.
func (s *Session) SetFreeBytes(f func(dir string) (int64, error)) { s.freeBytes = f }

// SetRecoverCaps lowers the entry and plan-run caps of a recovery run and returns the restore function.
func SetRecoverCaps(entries, runs int) (restore func()) {
	e, r := recoverEntryCap, planRunCap
	recoverEntryCap, planRunCap = entries, runs
	return func() { recoverEntryCap, planRunCap = e, r }
}

// FreeSpaceReserve is the reserve the free-space check keeps beyond the planned bytes.
var FreeSpaceReserve = freeSpaceReserve

// SetWalkSkipCap lowers how many walk errors a recovery plan lists one by one and returns the restore function.
func SetWalkSkipCap(n int) (restore func()) {
	old := walkSkipCap
	walkSkipCap = n
	return func() { walkSkipCap = old }
}

// SetReproduceReadObserver sees the size of every read of the reproduce check and returns the restore function.
func SetReproduceReadObserver(f func(n int)) (restore func()) {
	old := reproduceReadObserver
	reproduceReadObserver = f
	return func() { reproduceReadObserver = old }
}

// SetWarningEntryCap lowers how many analysis.warning entries one analysis writes and returns the restore function.
func SetWarningEntryCap(n int) (restore func()) {
	old := warningEntryCap
	warningEntryCap = n
	return func() { warningEntryCap = old }
}

// SetScanBudget sets the scan budget a session verifies under.
func (s *Session) SetScanBudget(b ScanBudget) { s.opts.Scan = b }

// SetImplausibleForTest makes the session treat its image's size claim as implausible.
func (s *Session) SetImplausibleForTest(v bool) { s.implausible = &v }

// SetReproduceWorkers fixes the number of reproduce workers (0 restores the default) and returns the restore function.
func SetReproduceWorkers(n int) (restore func()) {
	old := reproduceWorkersOverride
	reproduceWorkersOverride = n
	return func() { reproduceWorkersOverride = old }
}

// SetReproduceOneHook runs f with the artifact id at the start of each artifact's reproduction and returns the
// restore function.
func SetReproduceOneHook(f func(id string)) (restore func()) {
	old := reproduceOneHook
	reproduceOneHook = f
	return func() { reproduceOneHook = old }
}

// SetAllocationArtifactHook runs f with the artifact id at the start of each artifact's allocation check and
// returns the restore function.
func SetAllocationArtifactHook(f func(id string)) (restore func()) {
	old := allocationArtifactHook
	allocationArtifactHook = f
	return func() { allocationArtifactHook = old }
}
