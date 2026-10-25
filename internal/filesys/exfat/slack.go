package exfat

import "github.com/rbenzing/minutiae/internal/filesys"

// AllocatedRuns reports the whole clusters the file's data runs occupy, one run
// per data run with its length rounded up to the cluster size
// (filesys.AllocatedRunner). It is derived from the same runs as Runs, so:
//   - the range [ValidDataLength, DataLength) is a hole run and reports no
//     clusters (it was never initialised, so it is not slack);
//   - a chain shorter than DataLength lists only the clusters that could be
//     followed (a prefix), never an invented allocation;
//   - a file without clusters (DataLength 0) reports none.
//
// The slice is fresh on every call.
func (fl *file) AllocatedRuns() []filesys.Run { return filesys.UnitRuns(fl.runs, fl.unit) }

// ReportsSlack declares filesys.SlackReporter: the volume occupies exactly
// [0, Info().Size) of its partition and its regular files implement
// filesys.AllocatedRunner.
func (f *FS) ReportsSlack() bool { return true }
