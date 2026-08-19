package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/validation"
)

func TestBuildProducesCompleteDeterministicVerifiedArchive(t *testing.T) {
	directory := t.TempDir()
	input := fixtureInput(t)
	first := filepath.Join(directory, "first.zip")
	result, err := (Builder{}).Build(context.Background(), first, input)
	if err != nil {
		t.Fatal(err)
	}
	if err := validation.Archive(first); err != nil {
		t.Fatalf("published archive failed validation: %v", err)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %o, want 0600", info.Mode().Perm())
	}
	wantEntries := []string{
		"ALL_CONVERSATIONS.md", "CHECKSUMS.sha256", "README.md", "REVIEW_GUIDE.md", "SUMMARY.md",
		"conversations/codex/hh_ses_0123456789abcdefabcd.md", "errors.jsonl", "manifest.json", "redactions.jsonl", "schemas/event.schema.json",
		"schemas/manifest.schema.json", "schemas/session.schema.json",
		"sessions.jsonl",
	}
	gotEntries := zipNames(t, first)
	if strings.Join(gotEntries, "\n") != strings.Join(wantEntries, "\n") {
		t.Fatalf("entries mismatch\ngot:  %q\nwant: %q", gotEntries, wantEntries)
	}
	if result.Manifest.Totals.Sessions != 1 || result.Manifest.Totals.Events != 2 || result.Manifest.Totals.Errors != 0 || result.Manifest.Totals.Truncations != 1 {
		t.Fatalf("manifest did not reconcile: %#v", result.Manifest.Totals)
	}
	second := filepath.Join(directory, "second.zip")
	if _, err := (Builder{}).Build(context.Background(), second, fixtureInput(t)); err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := os.ReadFile(first)
	secondBytes, _ := os.ReadFile(second)
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Fatal("equivalent input did not produce byte-stable ZIP output")
	}
}

func TestBuildNeverOverwritesAndCleansCancellation(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "existing.zip")
	if err := os.WriteFile(destination, []byte("keep me"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Builder{}).Build(context.Background(), destination, fixtureInput(t)); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("collision error = %v", err)
	}
	content, _ := os.ReadFile(destination)
	if string(content) != "keep me" {
		t.Fatal("existing destination was changed")
	}

	cancelledDestination := filepath.Join(directory, "cancelled.zip")
	ctx, cancel := context.WithCancel(context.Background())
	input := fixtureInput(t)
	input.Sessions[0].Events = func(context.Context, func(domain.EventRecord) error) error {
		cancel()
		return ctx.Err()
	}
	if _, err := (Builder{}).Build(ctx, cancelledDestination, input); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
	if _, err := os.Lstat(cancelledDestination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancellation left a final-looking archive")
	}
	matches, err := filepath.Glob(filepath.Join(directory, ".hce-work-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("workspace cleanup failed: %v %q", err, matches)
	}
}

func TestStrictIncompleteExportPublishesNothing(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "strict.zip")
	input := fixtureInput(t)
	input.Manifest.Options.Strict = true
	input.Sessions[0].Record.Integrity.Status = domain.IntegrityPartial
	if _, err := (Builder{}).Build(context.Background(), destination, input); !errors.Is(err, ErrStrictIncomplete) {
		t.Fatalf("strict incomplete error = %v", err)
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("strict failure published an archive")
	}
}

func TestCombinedMarkdownLimitAndTamperDetection(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "limited.zip")
	result, err := (Builder{CombinedMarkdownLimit: 1}).Build(context.Background(), destination, fixtureInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Options.CombinedMarkdown || contains(zipNames(t, destination), "ALL_CONVERSATIONS.md") {
		t.Fatal("oversized combined Markdown was not omitted")
	}
	if len(result.Manifest.Exclusions) == 0 || result.Manifest.Exclusions[len(result.Manifest.Exclusions)-1].Code != "combined_markdown_size_limit" {
		t.Fatal("combined Markdown omission was not disclosed")
	}

	tampered := filepath.Join(directory, "tampered.zip")
	rewriteZIP(t, destination, tampered, "SUMMARY.md")
	if err := validation.Archive(tampered); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tamper validation error = %v", err)
	}
}

func TestMaliciousTranscriptMetadataCannotControlEntryPaths(t *testing.T) {
	input := fixtureInput(t)
	input.Sessions[0].Record.Title = "../../escape.md<script>alert(1)</script>"
	input.Sessions[0].Record.Source.Path = "../../preserved/source"
	destination := filepath.Join(t.TempDir(), "safe.zip")
	if _, err := (Builder{}).Build(context.Background(), destination, input); err != nil {
		t.Fatal(err)
	}
	for _, name := range zipNames(t, destination) {
		if err := validation.ArchivePath(name); err != nil || strings.Contains(name, "escape") {
			t.Fatalf("unsafe transcript-controlled entry %q: %v", name, err)
		}
	}
}

func fixtureInput(t *testing.T) Input {
	t.Helper()
	timestamp := time.Date(2026, 8, 1, 2, 3, 4, 0, time.UTC)
	sessionID := "hh_ses_0123456789abcdefabcd"
	text := "hello <script>not executable</script>"
	session := domain.SessionRecord{
		RecordType: domain.RecordSession, SchemaVersion: domain.SchemaVersion,
		ID: sessionID, Harness: "codex", NativeSessionID: "native", Title: "Fixture",
		Project:   domain.Project{DisplayName: "project", WorkingDirectory: "/work/project", RepositoryRoot: "/work/project"},
		StartedAt: &timestamp, UpdatedAt: &timestamp, MatchedRange: true, MatchReason: domain.MatchEventInRange,
		Source:    domain.Source{Path: "/source/session.jsonl", Format: "codex-jsonl", SizeBytes: 100},
		Structure: domain.SessionStructure{Linear: true}, Counts: domain.RecordCounts{Events: 2, UserMessages: 1, AssistantMessages: 1},
		Integrity: domain.Integrity{Status: domain.IntegrityComplete},
	}
	events := []domain.EventRecord{
		{RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion, ID: "hh_evt_0123456789abcdefabcd", SessionID: sessionID, Sequence: 0, Timestamp: &timestamp, Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: &text}}, Redactions: []domain.Redaction{}, Native: []byte(`{"type":"message"}`)},
		{RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion, ID: "hh_evt_1123456789abcdefabcd", SessionID: sessionID, Sequence: 1, Timestamp: &timestamp, Type: domain.EventAssistantMessage, Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: &text}}, Redactions: []domain.Redaction{}, Truncations: []domain.Truncation{{FieldPath: "content[0].text", OriginalBytes: 100, RetainedBytes: 10}}},
	}
	return Input{
		Manifest: domain.Manifest{
			CommandVersion: "test", ExportedAt: timestamp,
			Range:    domain.ManifestRange{From: timestamp.Add(-time.Hour), To: timestamp.Add(time.Hour), Scope: domain.ScopeTouched},
			Timezone: "UTC", Options: domain.ManifestOptions{Harnesses: []string{"codex"}},
		},
		Sessions: []FinalizedSession{{Record: session, Events: func(_ context.Context, yield func(domain.EventRecord) error) error {
			for _, event := range events {
				if err := yield(event); err != nil {
					return err
				}
			}
			return nil
		}}},
		Schemas:                 readSchemas(t),
		IncludeCombinedMarkdown: true,
	}
}

func readSchemas(t *testing.T) map[string][]byte {
	t.Helper()
	result := map[string][]byte{}
	for _, name := range []string{"event.schema.json", "manifest.schema.json", "session.schema.json"} {
		content, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
		if err != nil {
			t.Fatal(err)
		}
		result["schemas/"+name] = content
	}
	return result
}

func zipNames(t *testing.T, filename string) []string {
	t.Helper()
	reader, err := zip.OpenReader(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	result := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		result = append(result, file.Name)
	}
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func rewriteZIP(t *testing.T, source, destination, tamperName string) {
	t.Helper()
	reader, err := zip.OpenReader(source)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	files := append([]*zip.File(nil), reader.File...)
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	for _, file := range files {
		header := file.FileHeader
		target, err := writer.CreateHeader(&header)
		if err != nil {
			t.Fatal(err)
		}
		sourceReader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(target, sourceReader); err != nil {
			t.Fatal(err)
		}
		sourceReader.Close()
		if file.Name == tamperName {
			if _, err := io.WriteString(target, "tampered\n"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}
