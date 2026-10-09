package dbrecover

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
)

func slackScenePlanted(t *testing.T) *slackScene {
	s := plainScene()
	off, _ := s.gap(t, 2)
	s.plant(2, off+40, slackCell(7, 70, "deleted-row-seven-"+filler(30, 7)))
	s.plant(2, off+200, slackCell(2, 20, "live-row-two-"+filler(30, 2))) // a copy of a live cell
	return s
}

func TestSlackRowsMapAndVerify(t *testing.T) {
	c := recordstest.NewCase(t)
	s := slackScenePlanted(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	var slack []records.Full
	for _, f := range storedRecords(t, c) {
		if f.Method == "sqlite-page-slack" {
			slack = append(slack, f)
		}
	}
	if len(slack) != 1 || res.Counts.ByMethod["sqlite-page-slack"] != 1 {
		t.Fatalf("%d stored slack records, ByMethod %v, want 1", len(slack), res.Counts.ByMethod)
	}
	f := slack[0]
	p := payloadOf(t, f)
	if _, has := p["cell"]; has {
		t.Errorf("a slack record carries a cell key: %v", p)
	}
	if _, has := p["carved"]; !has {
		t.Errorf("a slack record carries no carved object: %v", p)
	}
	if !strings.Contains(f.Locator, ";span=") || strings.Contains(f.Locator, ";cell=") {
		t.Errorf("locator %q, want a span= locator and no cell=", f.Locator)
	}
	if !f.Recovered || !f.Deleted {
		t.Errorf("recovered %v deleted %v, want both (never live)", f.Recovered, f.Deleted)
	}
	if f.Confidence == nil || *f.Confidence > 25 {
		t.Errorf("confidence %v, want at most 25", f.Confidence)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func TestSlackStatsAreAudited(t *testing.T) {
	c := recordstest.NewCase(t)
	s := slackScenePlanted(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	if res.Counts.SlackCopiesOfLive != 1 {
		t.Errorf("SlackCopiesOfLive %d, want 1", res.Counts.SlackCopiesOfLive)
	}
	if res.Counts.SlackUnparsedBytes != 0 {
		t.Logf("SlackUnparsedBytes %d", res.Counts.SlackUnparsedBytes)
	}
	end := lastEntry(t, readAudit(t, c), "analysis.end")
	if got := fmt.Sprint(end.Details["slack_copies_of_live"]); got != "1" {
		t.Errorf("analysis.end slack_copies_of_live %s, want 1", got)
	}
	if got := fmt.Sprint(end.Details["slack_unparsed_bytes"]); got != fmt.Sprint(res.Counts.SlackUnparsedBytes) {
		t.Errorf("analysis.end slack_unparsed_bytes %s, result %d", got, res.Counts.SlackUnparsedBytes)
	}
	named := false
	for _, i := range indexes(readAudit(t, c), "analysis.warning") {
		d := readAudit(t, c)[i].Details
		if r := fmt.Sprint(d["reason"]); strings.Contains(r, "slack") && strings.Contains(r, "page 2") {
			named = true
		}
	}
	if !named {
		t.Error("no analysis.warning names the slack copy of a live cell on page 2")
	}
}

// TestSlackSecondPassDoesNotChangeTheFirstPass runs the same case with the
// slack carving switched off (the baseline) and on: apart from the slack rows
// and the slack counters, every count and every stored record is the same.
func TestSlackSecondPassDoesNotChangeTheFirstPass(t *testing.T) {
	envs := map[string]func(t *testing.T) (*runEnv, Inputs){
		"planted gap": func(t *testing.T) (*runEnv, Inputs) {
			c := recordstest.NewCase(t)
			db := addFile(t, c, "/data/slack.db", slackScenePlanted(t).b.Bytes())
			return &runEnv{c: c}, Inputs{DB: db.ID}
		},
		"wal and journal": func(t *testing.T) (*runEnv, Inputs) {
			e := newRunEnv(t)
			return e, e.in()
		},
	}
	for name, mk := range envs {
		t.Run(name, func(t *testing.T) {
			run := func(on bool) (Result, []records.Full) {
				defer func(old bool) { slackCarving = old }(slackCarving)
				slackCarving = on
				e, in := mk(t)
				return mustRun(t, e.c, in, Options{}), storedRecords(t, e.c)
			}
			base, baseRecs := run(false)
			with, withRecs := run(true)
			if base.Counts.ByMethod["sqlite-page-slack"] != 0 || base.Counts.SlackCopiesOfLive != 0 || base.Counts.SlackUnparsedBytes != 0 {
				t.Fatalf("the baseline run touched slack: %+v", base.Counts)
			}
			a, b := base.Counts, with.Counts
			slack := b.ByMethod["sqlite-page-slack"]
			delete(b.ByMethod, "sqlite-page-slack")
			delete(b.ConfidenceRange, "sqlite-page-slack")
			b.SlackCopiesOfLive, b.SlackUnparsedBytes, b.SlackDroppedCrossed = 0, 0, 0
			b.Written -= slack
			if name == "planted gap" && slack == 0 {
				t.Errorf("the planted cell was not carved")
			}
			b.RelationUnknown, a.RelationUnknown = 0, 0
			b.FitOnly, a.FitOnly = 0, 0
			if !reflect.DeepEqual(a, b) {
				t.Errorf("counts changed by the second pass:\n baseline %+v\n with     %+v", a, b)
			}
			have := map[string]bool{}
			for _, f := range withRecs {
				have[f.Method+"|"+f.Locator+"|"+f.Summary] = true
			}
			for _, f := range baseRecs {
				if !have[f.Method+"|"+f.Locator+"|"+f.Summary] {
					t.Errorf("a first-pass record is gone or changed: %s %s", f.Method, f.Locator)
				}
			}
			if int64(len(withRecs)-len(baseRecs)) != slack {
				t.Errorf("%d records more than the baseline, %d slack rows", len(withRecs)-len(baseRecs), slack)
			}
		})
	}
}
