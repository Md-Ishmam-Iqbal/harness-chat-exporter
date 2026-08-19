package config

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func TestDefaultRollingSevenDays(t *testing.T) {
	now := time.Date(2026, 8, 6, 18, 30, 0, 0, time.FixedZone("+06", 6*60*60))
	options, err := parseExport(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := options.Range.To.Sub(options.Range.From); got != 7*24*time.Hour {
		t.Fatalf("default duration = %s", got)
	}
	if options.MaxResponseBytes != 512 {
		t.Fatalf("unexpected defaults: %#v", options)
	}
	if options.SessionScope != ScopeEventsOnly || options.OutputFormat != FormatZIP {
		t.Fatalf("unexpected usage defaults: %#v", options)
	}
}

func TestNoCommandAndFlagOnlyFormsDefaultToExport(t *testing.T) {
	command, options, err := Parse(nil)
	if err != nil || command != CommandExport || options.OutputFormat != FormatZIP {
		t.Fatalf("no-argument parse = %s %#v, %v", command, options, err)
	}

	command, options, err = Parse([]string{"--since", "3d", "--output", "usage.md"})
	if err != nil || command != CommandExport || options.OutputFormat != FormatMarkdown {
		t.Fatalf("flag-only parse = %s %#v, %v", command, options, err)
	}
	if got := options.Range.To.Sub(options.Range.From); got != 3*24*time.Hour {
		t.Fatalf("flag-only duration = %s", got)
	}
}

func TestRollingDurationUnits(t *testing.T) {
	now := time.Date(2026, 8, 6, 18, 30, 0, 0, time.UTC)
	for input, want := range map[string]time.Duration{
		"10m": 10 * time.Minute,
		"1h":  time.Hour,
		"3d":  3 * 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"1w":  7 * 24 * time.Hour,
	} {
		r, err := ResolveRange(now, input, "", "")
		if err != nil {
			t.Fatalf("ResolveRange(%q): %v", input, err)
		}
		if got := r.To.Sub(r.From); got != want {
			t.Fatalf("ResolveRange(%q) duration = %s, want %s", input, got, want)
		}
	}
}

func TestExplicitDateRangeUsesLocalMidnight(t *testing.T) {
	location := time.FixedZone("+06", 6*60*60)
	now := time.Date(2026, 8, 6, 18, 30, 0, 0, location)
	r, err := ResolveRange(now, "7d", "2026-08-01", "2026-08-06")
	if err != nil {
		t.Fatal(err)
	}
	if r.From.Hour() != 0 || r.To.Hour() != 0 || r.From.Location() != location {
		t.Fatalf("date-only range was not local midnight: %#v", r)
	}
}

func TestConflictingFlags(t *testing.T) {
	_, err := parseExport([]string{"--strict", "--best-effort"}, time.Now())
	if err == nil {
		t.Fatal("expected strict/best-effort conflict")
	}
}

func TestSinceConflictsWithFromEvenAtDefaultValue(t *testing.T) {
	_, err := parseExport([]string{"--since", "1w", "--from", "2026-08-01"}, time.Now())
	if err == nil {
		t.Fatal("expected since/from conflict")
	}
}

func TestParseSizes(t *testing.T) {
	for input, want := range map[string]int64{"2MB": 2 << 20, "1GiB": 1 << 30, "1024": 1024} {
		got, err := ParseSize(input)
		if err != nil || got != want {
			t.Fatalf("ParseSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
}

func TestRejectsUnsafeFilenameAndHarness(t *testing.T) {
	if _, err := parseExport([]string{"--file-name", "../bad.zip"}, time.Now()); err == nil {
		t.Fatal("expected unsafe filename rejection")
	}
	if _, err := parseExport([]string{"--harness", "codex,other"}, time.Now()); err == nil {
		t.Fatal("expected unsupported harness rejection")
	}
}

func TestOutputPathInfersFormat(t *testing.T) {
	for path, want := range map[string]OutputFormat{
		"report.zip": FormatZIP, "reports/week.md": FormatMarkdown,
		"usage.jsonl": FormatJSONL, "usage.csv": FormatCSV,
	} {
		options, err := parseExport([]string{"--output", path}, time.Now())
		if err != nil {
			t.Fatalf("output %q: %v", path, err)
		}
		if options.OutputFormat != want || options.FileName != filepath.Base(path) || options.OutputDirectory != filepath.Dir(path) {
			t.Fatalf("output %q resolved to %#v", path, options)
		}
	}
}

func TestFullSessionsAliasAndOutputConflicts(t *testing.T) {
	options, err := parseExport([]string{"--full-sessions"}, time.Now())
	if err != nil || options.SessionScope != ScopeTouched {
		t.Fatalf("full sessions = %#v, %v", options, err)
	}
	for _, args := range [][]string{
		{"--full-sessions", "--session-scope", "events-only"},
		{"--output", "report.md", "--format", "zip"},
		{"--output", "report.zip", "--file-name", "other.zip"},
	} {
		if _, err := parseExport(args, time.Now()); err == nil {
			t.Fatalf("expected conflict for %v", args)
		}
	}
}

func TestPreviewUsesExportOptions(t *testing.T) {
	command, options, err := Parse([]string{"preview", "--since", "3d", "--output", "preview.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	if command != CommandPreview || options.OutputFormat != FormatJSONL || options.SessionScope != ScopeEventsOnly {
		t.Fatalf("unexpected preview parse: %s %#v", command, options)
	}
}

func TestDefaultFileName(t *testing.T) {
	r := domain.TimeRange{
		From: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
	}
	if got, want := DefaultFileName(r), "harness-chat-exporter_2026-08-01_2026-08-06.zip"; got != want {
		t.Fatalf("DefaultFileName() = %q, want %q", got, want)
	}
}

func TestAdapterPathsAreRepeatable(t *testing.T) {
	options, err := parseExport([]string{"--opencode-path", "/one", "--opencode-path", "/two"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := options.AdapterRoots["opencode"]; len(got) != 2 || got[0] != "/one" || got[1] != "/two" {
		t.Fatalf("unexpected roots: %#v", got)
	}
}
