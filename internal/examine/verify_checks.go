package examine

import "github.com/rbenzing/minutiae/internal/evidence"

// VerifyChecks is every composed check of case verify, in order. The command line passes exactly this list,
// with Options{Drivers: d.FSDrivers} (nil drivers mean detect.Drivers). Nothing here can be skipped.
func VerifyChecks(opts Options) []evidence.VerifyCheck {
	return []evidence.VerifyCheck{RecoveredCheck(), AllocationCheck(opts.Drivers)}
}
