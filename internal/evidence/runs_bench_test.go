package evidence

import (
	"fmt"
	"strings"
	"testing"
)

// BenchmarkReadRunLines records the cost of the streaming sidecar reader on a 1M-line sidecar (the
// 5C E55 figure was 3.78 s before the streaming decoder and 1.82 s after). The 256-byte line cap, the
// required keys and the line count are tested by the 5C tests, not here.
func BenchmarkReadRunLines(b *testing.B) {
	var sb strings.Builder
	n := MaxRecoveredRuns
	for i := 0; i < n; i++ {
		fmt.Fprintf(&sb, "{\"offset\":%d,\"length\":4096}\n", int64(i)*8192)
	}
	data := sb.String()
	b.Run("max", func(b *testing.B) {
		b.SetBytes(int64(len(data)))
		for i := 0; i < b.N; i++ {
			runs, err := readRunLines(strings.NewReader(data))
			if err != nil || len(runs) != n {
				b.Fatalf("%d runs, %v", len(runs), err)
			}
		}
	})
}
