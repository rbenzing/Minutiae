//go:build sqlitematrix

package sqlitefile_test

import "testing"

// TestLiveMatchesEngineWALMutationsFull mutates every frame in every way (200+
// mutations); run manually with -tags sqlitematrix.
func TestLiveMatchesEngineWALMutationsFull(t *testing.T) { runWALMutations(t, true) }
