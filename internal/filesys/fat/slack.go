package fat

import "github.com/rbenzing/minutiae/internal/filesys"

// AllocatedRuns reports the whole clusters the file's chain holds, one run per
// data run with its length rounded up to the cluster size (filesys.AllocatedRunner).
// It is derived from the same runs as Runs, so a chain that is shorter than the
// size lists only the clusters that could be followed (a prefix), and a file
// without clusters (size 0) reports none. The slice is fresh on every call.
func (fl *file) AllocatedRuns() []filesys.Run { return filesys.UnitRuns(fl.runs, fl.unit) }

// ReportsSlack declares filesys.SlackReporter: the volume occupies exactly
// [0, Info().Size) of its partition and its regular files implement
// filesys.AllocatedRunner.
func (f *FS) ReportsSlack() bool { return true }
