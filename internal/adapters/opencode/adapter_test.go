package opencode

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

type fixtureData struct {
	Sessions []map[string]any `json:"sessions"`
	Messages []map[string]any `json:"messages"`
	Parts    []map[string]any `json:"parts"`
}

func TestSQLiteFileURLUsesWindowsDriveAsPath(t *testing.T) {
	u := sqliteFileURL(`C:\Users\runner admin\AppData\Local\opencode.db`)
	query := u.Query()
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()

	parsed, err := url.Parse(u.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != "/C:/Users/runner admin/AppData/Local/opencode.db" {
		t.Fatalf("invalid Windows SQLite URI: %q (%#v)", u.String(), parsed)
	}
}

func TestDetectAndNormalizeAllOpenCodeGenerations(t *testing.T) {
	root := t.TempDir()
	copyTree(t, fixturePath(t, "legacy-root", "storage"), filepath.Join(root, "storage"))
	createLegacyProjectFixture(t, root)
	databasePath := filepath.Join(root, "opencode.db")
	createFixtureDatabase(t, databasePath, false)

	adapter := New(root)
	detection := adapter.Detect(context.Background(), domain.Environment{HomeDir: t.TempDir(), Variables: map[string]string{}})
	if detection.Status != domain.DetectionDetected {
		t.Fatalf("unexpected detection: %#v", detection)
	}
	if got, want := len(detection.Roots), 3; got != want {
		t.Fatalf("detected roots = %d, want %d: %#v", got, want, detection.Roots)
	}

	var references []domain.SessionReference
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		references = append(references, reference)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := len(references), 4; got != want { // two SQLite sessions plus one in each legacy generation
		t.Fatalf("references = %d, want %d", got, want)
	}

	expected := map[domain.EventType]int{
		domain.EventUserMessage: 1, domain.EventAssistantMessage: 1,
		domain.EventToolCall: 3, domain.EventToolResult: 2,
		domain.EventFileChange: 1, domain.EventUnknown: 1,
	}
	seenKinds := map[string]bool{}
	for _, reference := range references {
		probe := adapter.Probe(context.Background(), reference)
		if !probe.Supported || probe.Err != nil {
			t.Fatalf("probe %s: %#v", reference.SourceKind, probe)
		}
		if reference.NativeSessionID == "ses_child" {
			if reference.Metadata["parent_id"] != "ses_parent" || reference.Metadata["parent_session_id"] != "ses_parent" || reference.Metadata["subagent"] != "true" {
				t.Fatalf("child relationship not retained: %#v", reference.Metadata)
			}
			if reference.Metadata["time_archived"] == "" {
				t.Fatalf("archived-session metadata not retained: %#v", reference.Metadata)
			}
			continue
		}
		seenKinds[reference.SourceKind] = true
		events, result := parseEvents(t, adapter, reference)
		if result.Err != nil {
			t.Fatalf("parse %s: %#v", reference.SourceKind, result)
		}
		counts := eventCounts(events)
		for eventType, want := range expected {
			if got := counts[eventType]; got != want {
				t.Fatalf("%s %s count=%d want=%d; all=%#v", reference.SourceKind, eventType, got, want, counts)
			}
		}
		allNative := nativeText(events)
		for _, canary := range []string{"session-disclosure-canary", "usage-study-canary", "private-reasoning-canary", "tool-output-canary", "future-part-canary"} {
			if !strings.Contains(allNative, canary) {
				t.Fatalf("%s native disclosure omitted %q", reference.SourceKind, canary)
			}
		}
		assertToolPair(t, events)
		for _, event := range events {
			if event.Type == domain.EventToolCall && event.Tool != nil && event.Tool.CallID == "call_fixture" && strings.Contains(string(event.Native), "tool-output-canary") {
				t.Fatalf("%s duplicated an unbounded completed result onto its call event", reference.SourceKind)
			}
		}
	}
	for _, kind := range []string{sourceSQLite, sourceLegacyRoot, sourceLegacyLocal} {
		if !seenKinds[kind] {
			t.Fatalf("generation %s was not discovered", kind)
		}
	}
}

func TestSQLiteSchemaVariantAndSourceImmutability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode-next.db")
	createFixtureDatabase(t, path, true)
	before := fileDigest(t, path)
	adapter := New(path)
	detection := adapter.Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	var references []domain.SessionReference
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		references = append(references, reference)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(references) != 2 || references[0].SourceVersion != "sqlite-session-message-part" {
		t.Fatalf("plural schema discovery: %#v", references)
	}
	for _, reference := range references {
		_, result := parseEvents(t, adapter, reference)
		if result.Err != nil {
			t.Fatalf("parse: %#v", result)
		}
	}
	after := fileDigest(t, path)
	if before != after {
		t.Fatal("read-only adapter modified the SQLite source")
	}
}

func TestSQLiteReadTransactionUsesOneSnapshotDuringWALWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	createFixtureDatabase(t, path, false)
	adapter := New(path)
	detection := adapter.Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	var parent domain.SessionReference
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		if reference.NativeSessionID == "ses_parent" {
			parent = reference
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	writer, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	wrote := false
	var events []domain.NativeEvent
	result := adapter.Parse(context.Background(), parent, func(_ context.Context, event domain.NativeEvent) error {
		if !wrote {
			wrote = true
			data := `{"type":"text","text":"concurrent-write-canary"}`
			_, err := writer.Exec(`INSERT INTO part(id,message_id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?,?)`,
				"prt_concurrent", "msg_assistant", "ses_parent", int64(1786000008000), int64(1786000008000), data)
			if err != nil {
				return err
			}
		}
		events = append(events, event)
		return nil
	})
	if result.Err != nil {
		t.Fatalf("parse: %#v", result)
	}
	if strings.Contains(nativeText(events), "concurrent-write-canary") {
		t.Fatal("read transaction mixed a later WAL write into its snapshot")
	}
}

func TestMalformedAndMissingLegacyRecordsArePartialAndPreserved(t *testing.T) {
	root := t.TempDir()
	copyTree(t, fixturePath(t, "legacy-root", "storage"), filepath.Join(root, "storage"))
	malformedPath := filepath.Join(root, "storage", "part", "msg_assistant", "prt_malformed.json")
	if err := os.WriteFile(malformedPath, []byte(`{"type":"text","text":"malformed-canary"`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "storage", "part", "msg_user")
	if err := os.RemoveAll(missing); err != nil {
		t.Fatal(err)
	}
	adapter := New(root)
	detection := adapter.Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	var reference domain.SessionReference
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(value domain.SessionReference) error {
		reference = value
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events, result := parseEvents(t, adapter, reference)
	if !result.Partial || result.Malformed != 1 {
		t.Fatalf("malformed/missing legacy data looked complete: %#v", result)
	}
	if !strings.Contains(nativeText(events), "malformed-canary") {
		t.Fatal("malformed legacy bytes were not preserved")
	}
	if eventCounts(events)[domain.EventUnknown] < 2 { // known future type plus malformed record
		t.Fatalf("unknown records were omitted: %#v", eventCounts(events))
	}
}

func TestInvalidSQLiteHeaderIsDetectedAsDegraded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	if err := os.WriteFile(path, []byte("not-a-sqlite-database"), 0o600); err != nil {
		t.Fatal(err)
	}
	detection := New(path).Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	if detection.Status != domain.DetectionDegraded || len(detection.Warnings) != 1 || len(detection.Roots) != 1 {
		t.Fatalf("invalid header did not produce degraded coverage: %#v", detection)
	}
}

func TestDetectionPrecedence(t *testing.T) {
	explicit := t.TempDir()
	environment := t.TempDir()
	createFixtureDatabase(t, filepath.Join(explicit, "opencode.db"), false)
	createFixtureDatabase(t, filepath.Join(environment, "configured.db"), false)
	detection := New(explicit).Detect(context.Background(), domain.Environment{
		HomeDir: t.TempDir(), Variables: map[string]string{"OPENCODE_DB": filepath.Join(environment, "configured.db")},
	})
	if len(detection.Roots) != 2 || detection.Roots[0].Origin != "explicit" || detection.Roots[1].Origin != "OPENCODE_DB" {
		t.Fatalf("unexpected precedence: %#v", detection.Roots)
	}
}

func TestLocalOpenCodeCompatibility(t *testing.T) {
	root := os.Getenv("HCE_TEST_LOCAL_OPENCODE")
	if root == "" {
		t.Skip("set HCE_TEST_LOCAL_OPENCODE to intentionally smoke-test a local store")
	}
	adapter := New(root)
	detection := adapter.Detect(context.Background(), domain.Environment{Variables: map[string]string{}})
	if detection.Status != domain.DetectionDetected && detection.Status != domain.DetectionDegraded {
		t.Fatalf("local OpenCode store not detected: status=%s", detection.Status)
	}
	var selected *domain.SessionReference
	count := 0
	if err := adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		count++
		if selected == nil && reference.SourceKind == sourceSQLite {
			copy := reference
			selected = &copy
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count == 0 || selected == nil {
		t.Fatalf("local store yielded no SQLite sessions (discovered=%d)", count)
	}
	infoBefore, err := os.Stat(selected.Metadata["database_path"])
	if err != nil {
		t.Fatal(err)
	}
	emitted := int64(0)
	result := adapter.Parse(context.Background(), *selected, func(_ context.Context, _ domain.NativeEvent) error {
		emitted++
		return nil
	})
	if result.Err != nil || emitted == 0 {
		t.Fatalf("local SQLite parse failed without exposing content: emitted=%d partial=%t error=%v", emitted, result.Partial, result.Err)
	}
	infoAfter, err := os.Stat(selected.Metadata["database_path"])
	if err != nil {
		t.Fatal(err)
	}
	if infoBefore.Size() != infoAfter.Size() || !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
		t.Fatal("local OpenCode database metadata changed during read-only smoke test")
	}
}

func parseEvents(t *testing.T, adapter *Adapter, reference domain.SessionReference) ([]domain.NativeEvent, domain.ParseResult) {
	t.Helper()
	var events []domain.NativeEvent
	result := adapter.Parse(context.Background(), reference, func(_ context.Context, event domain.NativeEvent) error {
		events = append(events, event)
		return nil
	})
	return events, result
}

func eventCounts(events []domain.NativeEvent) map[domain.EventType]int {
	result := map[domain.EventType]int{}
	for _, event := range events {
		result[event.Type]++
	}
	return result
}

func nativeText(events []domain.NativeEvent) string {
	var builder strings.Builder
	for _, event := range events {
		builder.Write(event.Native)
	}
	return builder.String()
}

func assertToolPair(t *testing.T, events []domain.NativeEvent) {
	t.Helper()
	calls := map[string]string{}
	results := 0
	for _, event := range events {
		if event.Type == domain.EventToolCall && event.Tool != nil {
			calls[event.Tool.CallID] = event.NativeEventID
		}
		if event.Type == domain.EventToolResult && event.Tool != nil {
			results++
			if calls[event.Tool.CallID] == "" || event.ParentNativeEventID != calls[event.Tool.CallID] {
				t.Fatalf("tool result %s is not linked to its call", event.Tool.CallID)
			}
		}
	}
	if len(calls) != 3 || results != 2 {
		t.Fatalf("tool transitions not retained: calls=%#v results=%d", calls, results)
	}
}

func createFixtureDatabase(t *testing.T, path string, plural bool) {
	t.Helper()
	raw, err := os.ReadFile(fixturePath(t, "sqlite", "current.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture fixtureData
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	sessionTable, messageTable, partTable := "session", "message", "part"
	if plural {
		sessionTable, messageTable, partTable = "sessions", "messages", "parts"
	}
	database, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, project_id TEXT, parent_id TEXT, slug TEXT, directory TEXT, title TEXT, version TEXT, time_created INTEGER, time_updated INTEGER, time_archived INTEGER, metadata TEXT)`, sessionTable),
		fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`, messageTable),
		fmt.Sprintf(`CREATE TABLE %s (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT)`, partTable),
	}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range fixture.Sessions {
		_, err := database.Exec(fmt.Sprintf(`INSERT INTO %s(id,project_id,parent_id,slug,directory,title,version,time_created,time_updated,time_archived,metadata) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, sessionTable),
			row["id"], row["project_id"], row["parent_id"], row["slug"], row["directory"], row["title"], row["version"], int64(row["time_created"].(float64)), int64(row["time_updated"].(float64)), nullableFixtureInt(row["time_archived"]), row["metadata"])
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range fixture.Messages {
		data, _ := json.Marshal(row["data"])
		_, err := database.Exec(fmt.Sprintf(`INSERT INTO %s(id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?)`, messageTable),
			row["id"], row["session_id"], int64(row["time_created"].(float64)), int64(row["time_updated"].(float64)), string(data))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range fixture.Parts {
		data, _ := json.Marshal(row["data"])
		_, err := database.Exec(fmt.Sprintf(`INSERT INTO %s(id,message_id,session_id,time_created,time_updated,data) VALUES(?,?,?,?,?,?)`, partTable),
			row["id"], row["message_id"], row["session_id"], int64(row["time_created"].(float64)), int64(row["time_updated"].(float64)), string(data))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
}

func createLegacyProjectFixture(t *testing.T, root string) {
	t.Helper()
	source := fixturePath(t, "legacy-root", "storage")
	target := filepath.Join(root, "project", "project_fixture", "storage", "session")
	if err := os.MkdirAll(filepath.Join(target, "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	copyTree(t, filepath.Join(source, "session", "project_fixture", "ses_parent.json"), filepath.Join(target, "info", "ses_parent.json"))
	copyTree(t, filepath.Join(source, "message", "ses_parent"), filepath.Join(target, "message", "ses_parent"))
	for _, messageID := range []string{"msg_user", "msg_assistant"} {
		copyTree(t, filepath.Join(source, "part", messageID), filepath.Join(target, "part", "ses_parent", messageID))
	}
}

func fixturePath(t *testing.T, parts ...string) string {
	t.Helper()
	values := append([]string{"..", "..", "..", "testdata", "opencode"}, parts...)
	return filepath.Join(values...)
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func nullableFixtureInt(value any) any {
	if value == nil {
		return nil
	}
	return int64(value.(float64))
}
