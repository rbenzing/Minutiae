package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
)

// scanPfx is a case with one glob-matched file ("a") and one relocated file no glob matches.
func scanPfx(t *testing.T, remote string) *pfx {
	t.Helper()
	p := newPfx(t, "a")
	p.edit(func(c *evidence.Case) {
		p.recs["moved"] = pcapture(t, c, "files/old/copy.db", evidence.Source{Kind: "file", RemotePath: remote}, pData)
	})
	return p
}

func TestParsePlanScanAppendsNothing(t *testing.T) {
	p := scanPfx(t, "/sdcard/old/copy.db")
	reg := registry(t, "", good("a"))
	before, lines := dirDigest(t, p.dir, true), auditLineCount(t, p.dir)
	code, out, errOut := runP(t, reg, "parse", "plan", "--scan", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "candidate") || !strings.Contains(out, "[scan]") || !strings.Contains(out, p.recs["moved"].ID) {
		t.Errorf("the relocated file is not listed as a scan candidate:\n%s", out)
	}
	if got := auditLineCount(t, p.dir); got != lines+1 {
		t.Errorf("audit grew by %d lines, want exactly 1 (case.open)", got-lines)
	}
	if after := dirDigest(t, p.dir, true); after != before {
		t.Error("parse plan --scan changed an artifact, the manifest or artifacts.db")
	}
}

func TestParsePlanScanRefusesArtifact(t *testing.T) {
	p := scanPfx(t, "/sdcard/old/copy.db")
	code, out, errOut := runP(t, registry(t, "", good("a")), "parse", "plan", "--scan", "--artifact", p.recs["a"].ID, "--case", p.dir)
	if code != 2 || !strings.Contains(out+errOut, "--scan") {
		t.Errorf("exit %d, want 2 naming --scan: %s %s", code, out, errOut)
	}
}

func TestParsePlanScanJSONShape(t *testing.T) {
	p := scanPfx(t, "/sdcard/old/copy.db")
	reg := registry(t, "", good("a"))
	type shape struct {
		Rows []struct {
			Scan bool `json:"scan"`
		} `json:"rows"`
		Counts struct {
			Scan map[string]any `json:"scan"`
		} `json:"counts"`
		ScanFindings []map[string]any `json:"scan_findings"`
	}
	var raw map[string]json.RawMessage
	code, out, errOut := runP(t, reg, "parse", "plan", "--case", p.dir, "--json")
	if err := json.Unmarshal([]byte(out), &raw); code != 0 || err != nil {
		t.Fatalf("exit %d err %v: %s %s", code, err, out, errOut)
	}
	if _, ok := raw["scan_findings"]; !ok || string(raw["scan_findings"]) != "null" {
		t.Errorf("without --scan scan_findings must be present and null: %s", raw["scan_findings"])
	}
	var s shape
	if err := json.Unmarshal([]byte(out), &s); err != nil || s.Counts.Scan != nil || len(s.Rows) != 1 || s.Rows[0].Scan {
		t.Errorf("without --scan: counts.scan %v rows %+v err %v", s.Counts.Scan, s.Rows, err)
	}

	reg = registry(t, "", good("a"), cliParser{
		WellBehaved: good("u").WellBehaved,
		probe: func(context.Context, *parse.Input) (parse.Applicability, error) {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "no table"}, nil
		},
		glob: "android:**/parsertest/never.dat",
	})
	code, out, errOut = runP(t, reg, "parse", "plan", "--scan", "--case", p.dir, "--json")
	s = shape{}
	if err := json.Unmarshal([]byte(out), &s); code != 0 || err != nil {
		t.Fatalf("exit %d err %v: %s %s", code, err, out, errOut)
	}
	for _, k := range []string{"considered", "probed", "candidates", "not_applicable", "too_large", "companions", "not_probed", "pairs_not_probed", "findings_dropped", "stopped"} {
		if _, ok := s.Counts.Scan[k]; !ok {
			t.Errorf("counts.scan lacks %q: %v", k, s.Counts.Scan)
		}
	}
	if len(s.Counts.Scan) != 10 || s.Counts.Scan["stopped"] != "" || s.Counts.Scan["considered"] != float64(1) || s.Counts.Scan["candidates"] != float64(1) {
		t.Errorf("counts.scan %v", s.Counts.Scan)
	}
	if len(s.Rows) != 2 || s.Rows[0].Scan || !s.Rows[1].Scan {
		t.Errorf("rows %+v", s.Rows)
	}
	if len(s.ScanFindings) != 1 {
		t.Fatalf("findings %v", s.ScanFindings)
	}
	f := s.ScanFindings[0]
	if len(f) != 5 || f["artifact_id"] != p.recs["moved"].ID || f["parser"] != "u" || f["status"] != parse.UnsupportedSchema || f["reason"] != "no table" || f["path"] == "" {
		t.Errorf("finding %v", f)
	}
}

func TestParsePlanScanFindingsAreAnEmptyArrayNotNull(t *testing.T) {
	p := scanPfx(t, "/sdcard/old/copy.db")
	_, out, _ := runP(t, registry(t, "", good("a")), "parse", "plan", "--scan", "--case", p.dir, "--json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil || string(raw["scan_findings"]) != "[]" {
		t.Errorf("with --scan and no finding, scan_findings must be []: %s (%v)", raw["scan_findings"], err)
	}
}

func TestParsePlanScanIntegrityExits4(t *testing.T) {
	p := scanPfx(t, "/sdcard/old/copy.db")
	p.flip("moved")
	code, out, errOut := runP(t, registry(t, "", good("a")), "parse", "plan", "--scan", "--case", p.dir)
	if code != 4 {
		t.Errorf("exit %d, want 4: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "refused:integrity") {
		t.Errorf("the tampered candidate is not listed as refused:\n%s", out)
	}
}

func TestParsePlanScanEscapesDeviceText(t *testing.T) {
	evil := "/sdcard/\x1b[31mred\u202eevil\u0085.db"
	p := scanPfx(t, evil)
	reg := registry(t, "", good("a"), cliParser{
		WellBehaved: good("u").WellBehaved,
		probe: func(context.Context, *parse.Input) (parse.Applicability, error) {
			return parse.Applicability{Status: parse.UnsupportedSchema, Reason: "line one\nline\x1b[2J two"}, nil
		},
		glob: "android:**/parsertest/never.dat",
	})
	code, out, errOut := runP(t, reg, "parse", "plan", "--scan", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if !strings.Contains(out, "scan: ") {
		t.Errorf("no finding line:\n%s", out)
	}
	for _, s := range []string{out, errOut} {
		if r, bad := rawControl(s); bad {
			t.Errorf("raw %U reached the terminal:\n%q", r, s)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "scan: ") && strings.Contains(line, "unsupported-schema") && !strings.Contains(line, "two") {
			t.Errorf("a newline in the reason split the finding line: %q", line)
		}
	}
	if !strings.Contains(out, "scan: 1 considered, 1 probed, 1 candidates, 0 artifact x parser probes not applicable, 0 over 1 GiB, 0 companions, 0 not probed") {
		t.Errorf("no scan summary line:\n%s", out)
	}
}

func TestParsePlanScanFindingsListIsBounded(t *testing.T) {
	p := newPfx(t)
	p.edit(func(c *evidence.Case) {
		for i := 0; i < 25; i++ {
			pcapture(t, c, "files/f"+string(rune('a'+i))+".bin", evidence.Source{Kind: "file", RemotePath: "/sdcard/f" + string(rune('a'+i)) + ".bin"}, pData)
		}
	})
	reg := registry(t, "", cliParser{
		WellBehaved: good("u").WellBehaved,
		probe: func(context.Context, *parse.Input) (parse.Applicability, error) {
			return parse.Applicability{Status: parse.Encrypted, Reason: "locked"}, nil
		},
		glob: "android:**/parsertest/never.dat",
	})
	code, out, errOut := runP(t, reg, "parse", "plan", "--scan", "--case", p.dir)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, errOut)
	}
	if n := strings.Count(out, "\n  scan: ") - 0; n != 20 {
		t.Errorf("%d findings printed, want 20", n)
	}
	if !strings.Contains(out, "... 5 more") {
		t.Errorf("no overflow line:\n%s", out)
	}
}
