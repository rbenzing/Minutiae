package evidence_test

import (
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func BenchmarkOpenArtifact(b *testing.B) {
	for _, n := range []int{100, 1000, 10000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			c := recordstest.NewCase(b)
			rec := recordstest.AddArtifact(b, c, "a.db", []byte("x"))
			recordstest.AppendFillerManifest(b, c.Dir, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				f, _, err := c.OpenArtifact(rec.ID)
				if err != nil {
					b.Fatal(err)
				}
				_ = f.Close()
			}
		})
	}
}
