package recordstest

import (
	"encoding/json"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// TinyArtifactEvery makes the last artifact of a Scale receive exactly the records whose index is a
// multiple of it: a selective artifact for the filter measurements.
const TinyArtifactEvery = 100_000

const (
	scaleVocab = 20_000
	scaleEpoch = 1_577_836_800 // 2020-01-01T00:00:00Z
	scaleSpan  = 5 * 365 * 86400
)

// ScaleConfig configures the workload. Artifacts need at least four ids; the last is the tiny one.
type ScaleConfig struct {
	Seed      int64
	Artifacts []string
}

// Scale generates the synthetic workload of the 5D measurements: records of ~1 KiB (summary, body and
// payload text drawn from a 20,000-word Zipf vocabulary), 5% untimed, ties on whole hours, 5% deleted
// (a fifth of those recovered), 40% and 30% of the records on the first two artifacts. Record i is a pure
// function of (Seed, i), so a block can be regenerated and a query term can be chosen by rank.
type Scale struct {
	cfg   ScaleConfig
	vocab []string
	cdf   []float64
}

var scaleTypes = []struct {
	name   string
	weight int
}{{"message", 50}, {"web_visit", 15}, {"call", 8}, {"location", 8}, {"file", 6}, {"event", 4}, {"contact", 3}, {"calendar_event", 3}, {"note", 3}}

// NewScale builds the generator. It panics on fewer than four artifacts (a programming error).
func NewScale(cfg ScaleConfig) *Scale {
	if len(cfg.Artifacts) < 4 {
		panic("recordstest: Scale needs at least 4 artifacts")
	}
	s := &Scale{cfg: cfg, vocab: make([]string, 0, scaleVocab), cdf: make([]float64, scaleVocab)}
	seen := map[string]bool{}
	for k := 0; len(s.vocab) < scaleVocab; k++ {
		if w := makeWord(k); !seen[w] {
			seen[w] = true
			s.vocab = append(s.vocab, w)
		}
	}
	total := 0.0
	for r := range s.cdf {
		total += 1 / float64(r+1)
		s.cdf[r] = total
	}
	for r := range s.cdf {
		s.cdf[r] /= total
	}
	return s
}

func makeWord(k int) string {
	h := uint64(k)*0x9E3779B97F4A7C15 + 0x1234567
	b := make([]byte, 4+int(h%6)) // 4..9 letters
	for j := range b {
		h = h*6364136223846793005 + 1442695040888963407
		b[j] = 'a' + byte((h>>33)%26)
	}
	return string(b)
}

// Word returns the vocabulary word of frequency rank rank (0 = most frequent).
func (s *Scale) Word(rank int) string { return s.vocab[rank] }

// TinyArtifact is the artifact that receives every TinyArtifactEvery-th record and no other.
func (s *Scale) TinyArtifact() string { return s.cfg.Artifacts[len(s.cfg.Artifacts)-1] }

func (s *Scale) word(rng *rand.Rand) string {
	i := sort.SearchFloat64s(s.cdf, rng.Float64())
	if i >= len(s.vocab) {
		i = len(s.vocab) - 1
	}
	return s.vocab[i]
}

func (s *Scale) words(rng *rand.Rand, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s.word(rng))
	}
	return b.String()
}

func (s *Scale) artifact(rng *rand.Rand, i int64) string {
	a := s.cfg.Artifacts
	if i%TinyArtifactEvery == 0 {
		return a[len(a)-1]
	}
	switch u := rng.IntN(100); {
	case u < 40:
		return a[0]
	case u < 70:
		return a[1]
	default:
		return a[2+rng.IntN(len(a)-3)] // never the tiny one
	}
}

// At returns record i.
func (s *Scale) At(i int64) records.Record {
	rng := rand.New(rand.NewPCG(uint64(s.cfg.Seed), uint64(i)+0x5ca1e)) //nolint:gosec // a deterministic test generator
	pick, typ := rng.IntN(100), ""
	for _, t := range scaleTypes {
		if pick < t.weight {
			typ = t.name
			break
		}
		pick -= t.weight
	}
	payload := ValidPayload(typ)
	payload["i"] = i
	payload["text"] = s.words(rng, 30+rng.IntN(21))
	payload["tags"] = []any{s.word(rng), s.word(rng), s.word(rng)}
	payload["nested"] = map[string]any{"k": []any{int64(rng.IntN(1000)), true, nil}}
	r := records.Record{Type: typ, ArtifactID: s.artifact(rng, i), Summary: s.words(rng, 8+rng.IntN(7)), Payload: payload}
	if rng.IntN(100) < 65 {
		r.Body = s.words(rng, 60+rng.IntN(61))
	}
	if rng.IntN(3) > 0 {
		r.SourcePath = "/data/" + strconv.Itoa(rng.IntN(200)) + "/f" + strconv.Itoa(rng.IntN(5)) + ".db"
	}
	if rng.IntN(4) == 0 {
		r.Locator = "sqlite:table=t" + strconv.Itoa(rng.IntN(4)) + ";row=" + strconv.FormatInt(i, 10)
	}
	if rng.IntN(2) == 0 {
		r.Range = &records.Range{Offset: int64(rng.IntN(ArtifactSize - 4096)), Length: int64(rng.IntN(4096))}
	}
	if rng.IntN(100) >= 5 { // 5% untimed
		sec := int64(scaleEpoch + rng.IntN(scaleSpan))
		if rng.IntN(5) == 0 {
			sec -= sec % 3600 // ties on whole hours
		}
		r.Time = &records.Time{T: time.Unix(sec, 0).UTC()}
	}
	if rng.IntN(100) < 5 {
		r.Deleted = true
		if rng.IntN(5) == 0 {
			r.Recovery = "carve"
		}
	}
	if rng.IntN(3) == 0 {
		c := rng.IntN(101)
		r.Confidence = &c
	}
	return r
}

// Block returns records [from, from+n).
func (s *Scale) Block(from int64, n int) []records.Record {
	out := make([]records.Record, n)
	for k := range out {
		out[k] = s.At(from + int64(k))
	}
	return out
}

// RowBytes is the size of the text a record stores: summary, body, payload (as JSON), source path and
// locator. The 5D size ratio adds 64 fixed bytes per row for the numeric columns.
func RowBytes(r records.Record) int64 {
	p, _ := json.Marshal(r.Payload)
	return int64(len(r.Summary) + len(r.Body) + len(p) + len(r.SourcePath) + len(r.Locator))
}

// AddScaleArtifacts stores n artifacts of ArtifactSize bytes (distinct content) and returns their ids.
func AddScaleArtifacts(t testing.TB, c *evidence.Case, n int) []string {
	t.Helper()
	ids := make([]string, n)
	for i := range ids {
		data := make([]byte, ArtifactSize)
		for j := range data {
			data[j] = byte(i + j)
		}
		ids[i] = AddArtifact(t, c, "scale"+strconv.Itoa(i)+".db", data).ID
	}
	return ids
}
