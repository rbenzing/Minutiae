package androidwhatsapp_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/recrun"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidwhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// recoverVariants are the fixtures that hold history in the claimed tables, with a recover oracle (plan 4I task 14).
var recoverVariants = []string{"android-modern-2026", "android-transition-legacydata", "android-modern-migrated"}

// Spec section 14, D8: the claimed tables map their recovered rows, checked against an oracle that was computed by an
// independent page walker from the committed files and the scenario's own record of what it wrote. A variant where
// nothing was deleted or edited has no oracle file: it is checked against the empty set, which is a real claim (a typed
// record there would be invented). The recovered row's joins read the LIVE side tables; no job set is evaluated (4I-R4a).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			want := parsertest.RecoverExpect{Variant: v, Claimed: androidwhatsapp.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, androidwhatsapp.Parser{}, b, want)
		})
	}
}

// The claims are exactly the claimable (total) tables, all of them tables of one mapping. message_add_on is mapped but
// not claimed: its rows depend on a parent row, and a recovered reaction would name a parent that may be gone.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := androidwhatsapp.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := androidwhatsapp.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"message", "messages", "call_log"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [message messages call_log]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// The oracles are not vacuous: they list what the scenarios were built to leave behind, and the typed records carry the
// rows' own tables.
func TestRecoverOracleShape(t *testing.T) {
	tables := func(x parsertest.RecoverExpect) map[string]int {
		out := map[string]int{}
		for _, r := range x.Records {
			out[r.Table]++
			if r.Record.Type == "" || r.Rowid == nil {
				t.Fatalf("%s lists %+v with no type or rowid", x.Variant, r)
			}
		}
		return out
	}
	mig := tables(parsertest.LoadRecoverExpect(t, "testdata", "android-modern-migrated"))
	if mig["message"] < 30 || mig["call_log"] < 15 || mig["messages"] != 0 || len(mig) != 2 {
		t.Errorf("android-modern-migrated lists %v: want typed message and call_log rows and none of messages (R9)", mig)
	}
	leg := parsertest.LoadRecoverExpect(t, "testdata", "android-transition-legacydata")
	if n := tables(leg); n["messages"] < 10 || len(n) != 1 {
		t.Errorf("android-transition-legacydata lists %v", n)
	}
	for _, r := range leg.Records {
		// a freelist cannot attribute a legacy row (messages_quotes has the same columns): the WAL scenario, basis schema
		if r.Method != "sqlite-wal-prior" || r.Basis != "schema" || r.Confidence != 75 || *r.Rowid < 100 || *r.Rowid > 109 {
			t.Fatalf("android-transition-legacydata lists %+v, want a row (100 to 109, not the sentinel 150) the WAL deleted, basis schema, 75", r)
		}
	}
	if len(leg.Unmapped) != 1 || leg.Unmapped[0].Table != "messages" || leg.Unmapped[0].Rowid == nil || *leg.Unmapped[0].Rowid != 150 || !strings.Contains(leg.Unmapped[0].Contains, "sentinel") {
		t.Errorf("android-transition-legacydata unmapped %+v, want the sentinel row 150", leg.Unmapped)
	}
	wal := parsertest.LoadRecoverExpect(t, "testdata", "android-modern-2026")
	ids := map[string]map[int64]bool{}
	for _, r := range wal.Records {
		if r.Method != "sqlite-wal-prior" || r.Basis != "schema" || r.Confidence != 75 || r.Relation != "absent-from-live" {
			t.Fatalf("android-modern-2026 lists %+v, want a version the WAL deleted, basis schema, 75", r)
		}
		if ids[r.Table] == nil {
			ids[r.Table] = map[int64]bool{}
		}
		ids[r.Table][*r.Rowid] = true
	}
	if !ids["message"][90] || !ids["message"][91] || !ids["call_log"][90] || len(ids["message"]) != 2 || len(ids["call_log"]) != 1 {
		t.Errorf("android-modern-2026 lists %v, want messages 90 and 91 and call 90 (deleted in the WAL)", ids)
	}
	for _, x := range []parsertest.RecoverExpect{leg, wal} {
		if len(x.Digests) != len(x.Records) {
			t.Errorf("%s: %d digests for %d records: every want has a digest", x.Variant, len(x.Digests), len(x.Records))
		}
	}
}

func TestEveryRecoverOracleHasAFixture(t *testing.T) {
	for _, v := range recoverVariants {
		if !slices.Contains(variants, v) {
			t.Errorf("%s has a recover oracle but is not a fixture variant", v)
		}
		parsertest.LoadRecoverExpect(t, "testdata", v) // also checks it was computed from the committed files
	}
}

// collector is the emitter of a typed recovery run: it keeps what the runner hands over and refuses a live Emit.
type collector struct {
	typed    []parse.RecoveredFacts
	unmapped []parse.RecoveredFacts
	reasons  []string
}

func (*collector) Emit(context.Context, records.Record) error {
	return errors.New("the typed pass called Emit: a recovered row must never become a live record")
}
func (*collector) Warn(context.Context, string, string) error { return nil }
func (*collector) Note(string, string)                        {}
func (*collector) Progress(int64, int64)                      {}
func (c *collector) EmitRecovered(_ context.Context, _ records.Record, f parse.RecoveredFacts) error {
	c.typed = append(c.typed, f)
	return nil
}

func (c *collector) Unmapped(_ context.Context, f parse.RecoveredFacts, reason string) error {
	c.unmapped = append(c.unmapped, f)
	c.reasons = append(c.reasons, reason)
	return nil
}

// R9: a modern-only store claims nothing of messages. android-modern-migrated holds a legacy messages table that was
// created, filled and dropped, so its pages are on the freelist; the typed pass gives typed records for message and
// call_log rows only, the recover report lists messages as absent from the live schema, and the freed pages stay with the
// generic run (no row is delivered for messages).
func TestWhatsAppModernOnlyClaimsNoMessagesRows(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	b := parsertest.LoadFixture(t, "testdata", "android-modern-migrated").Bundle(h, remoteFor("android-modern-migrated"))
	var c collector
	rep, err := recrun.Run(t.Context(), b.Input(h, p, false), &c, p.Mappings(), p.Meta().Claims)
	if err != nil {
		t.Fatalf("the typed recovery pass: %v", err)
	}
	if len(c.typed) == 0 {
		t.Fatal("the typed pass delivered nothing: the check proves nothing")
	}
	seen := map[string]int{}
	for _, f := range c.typed {
		seen[f.Table]++
	}
	if seen["messages"] != 0 || seen["message"] == 0 || seen["call_log"] == 0 || len(seen) != 2 {
		t.Errorf("typed records by table %v, want message and call_log only", seen)
	}
	for _, f := range c.unmapped {
		if strings.EqualFold(f.Table, "messages") {
			t.Errorf("a recovered messages row was delivered to the typed pass: %+v", f)
		}
	}
	var absent bool
	for _, tc := range rep.Tables {
		if tc.Table == "messages" {
			absent = tc.Absent
			if tc.Delivered != 0 || tc.Mapped != 0 {
				t.Errorf("messages: %+v, want no row delivered", tc)
			}
		}
	}
	if !absent {
		t.Errorf("the recover report does not list messages as absent from the live schema: %+v", rep.Tables)
	}
	if _, ok := parsertest.FirstClaimedRow(t, h, p, b, "messages", nil); ok {
		t.Error("FirstClaimedRow delivers a messages row on a modern-only store: its freed pages belong to the generic run")
	}
	if _, ok := parsertest.FirstClaimedRow(t, h, p, b, "message", nil); !ok {
		t.Error("FirstClaimedRow delivers no message row: the scenario holds none")
	}
}

// A recovered legacy sentinel row (key_remote_jid '-1') is a counted unmapped row, never a record.
func TestWhatsAppSentinelRowIsCountedNotEmitted(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	b := parsertest.LoadFixture(t, "testdata", "android-transition-legacydata").Bundle(h, remoteFor("android-transition-legacydata"))
	var c collector
	if _, err := recrun.Run(t.Context(), b.Input(h, p, false), &c, p.Mappings(), p.Meta().Claims); err != nil {
		t.Fatal(err)
	}
	var sentinels int
	for i, f := range c.unmapped {
		if strings.EqualFold(f.Table, "messages") && strings.Contains(c.reasons[i], "sentinel") {
			sentinels++
		}
	}
	if sentinels != 1 {
		t.Fatalf("%d recovered sentinel rows reported unmapped (%v), want 1", sentinels, c.reasons)
	}
	if len(c.typed) < 10 {
		t.Errorf("%d typed records: the sentinel check proves nothing without the other rows", len(c.typed))
	}
}

// 4I-R4a: the mappings a typed pass uses carry no job set, so a recovered row (and a live row mapped by the same
// mapping) states no pending purge and is never presented as live-with-a-verdict: the host stamps its recovery.
func TestWhatsAppRecoveredRowsStateNoPendingPurge(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	fx := parsertest.LoadFixture(t, "testdata", "android-modern-2026")
	b := fx.Bundle(h, remoteFor("android-modern-2026"))
	live := h.RunLenient(p, b.Input(h, p, false))
	var purged int
	for _, r := range live.Records {
		raw, _ := r.Payload["raw"].(map[string]any)
		if raw["pending_purge"] != nil || raw["pending_purge_jobs"] != nil {
			purged++
		}
	}
	if purged == 0 {
		t.Fatal("the live parse states no pending purge in android-modern-2026: the check proves nothing")
	}
	res := h.RunLenient(mappingsOnly{p}, b.Input(h, p, false))
	if res.Err != nil || len(res.Records) == 0 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	for _, r := range res.Records {
		raw, _ := r.Payload["raw"].(map[string]any)
		if _, has := raw["pending_purge"]; has {
			t.Errorf("%s: pending_purge %v from a mapping with no job set", r.Locator, raw["pending_purge"])
		}
		del, _ := r.Payload["deleted"].(map[string]any)
		if _, has := raw["pending_purge_jobs"]; has || del["source"] == "deleted_chat_job" {
			t.Errorf("%s: a cleared-chat verdict from a mapping with no job set", r.Locator)
		}
	}
}

// mappingsOnly runs the parser's Mappings exactly as the typed pass does (a nil job set), through the live runner.
type mappingsOnly struct{ androidwhatsapp.Parser }

func (m mappingsOnly) Parse(ctx context.Context, in *parse.Input, out parse.Emitter) error {
	var steps []rowrun.Step
	for _, mp := range m.Mappings() {
		steps = append(steps, rowrun.Step{Mapping: mp, Optional: true})
	}
	_, err := rowrun.Run(ctx, in, out, steps)
	return err
}

// A backup copy is never typed-recovered: Probe says unsupported-schema (4I-R3, never applicable), Parse refuses it
// (ErrNotLiveDatabase), and the typed pass has nothing to map in a copy that holds no history.
func TestWhatsAppBackupCopyClaimsNothing(t *testing.T) {
	h := parsertest.NewHarness(t)
	p := androidwhatsapp.Parser{}
	fx := parsertest.LoadFixture(t, "testdata", "android-backup-plain")
	b := fx.Bundle(h, remoteFor("android-backup-plain"))
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.HasPrefix(got.Reason, "backup-copy:") {
		t.Fatalf("Probe = %+v, %v", got, err)
	}
	res := h.RunLenient(p, b.Input(h, p, false))
	if !errors.Is(res.Err, androidwhatsapp.ErrNotLiveDatabase) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records", res.Err, len(res.Records))
	}
	parsertest.AssertClaimedTablesMapRecoveredRows(t, h, p, b, parsertest.RecoverExpect{Variant: "android-backup-plain", Claimed: androidwhatsapp.ClaimableTables()})
}

// Spec 12.1: the same row gives the same record whether it arrives from Live() or rebuilt as a recovered row, for each
// claimed table. This is what makes the mappings reusable by the typed recovery pass.
func TestMapperIsSourceAgnostic(t *testing.T) {
	covered := map[string]bool{}
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remoteFor(v))
			parsertest.AssertMapperSourceAgnostic(t, h, androidwhatsapp.Parser{}, b, 200)
		})
		for _, tbl := range parsertest.LoadFixture(t, "testdata", v).Expect.Records {
			covered[tbl.Table] = true
		}
	}
	for _, tbl := range []string{"messages", "message", "call_log"} {
		if !covered[tbl] {
			t.Errorf("no variant holds a %s row: the source-agnostic check does not cover it", tbl)
		}
	}
}
