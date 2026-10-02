// Package version holds build identity, set at link time with
// -ldflags "-X github.com/rbenzing/minutiae/internal/version.Version=v0.1.0 -X github.com/rbenzing/minutiae/internal/version.Commit=<sha>".
package version

var (
	Version = "dev"
	Commit  = "none"
)

// String returns "Version (Commit)"; it is recorded in every case and audit entry.
func String() string { return Version + " (" + Commit + ")" }
