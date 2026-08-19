package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

func TestUsageProjectionTruncationAndCleanup(t *testing.T) {
	workspace, sink, now := testSink(t, domain.ScopeTouched, 8, 1024)
	workspacePath := workspace.Path()
	assertMode(t, workspacePath, 0o700)
	spoolPath := sink.path
	assertMode(t, spoolPath, 0o600)

	environment := "<environment_context><cwd>/private/project</cwd></environment_context>"
	if err := sink.Add(context.Background(), domain.NativeEvent{
		Timestamp: &now, Type: domain.EventUserMessage, Role: domain.RoleUser,
		Content: []domain.ContentBlock{{Type: "input_text", Text: &environment}},
	}); err != nil {
		t.Fatal(err)
	}
	userText := "full user prompt"
	imageOpen, imageClose := `<image name="large.png">`, "</image>"
	if err := sink.Add(context.Background(), domain.NativeEvent{
		NativeEventID: "user-1", Timestamp: &now, TimestampSource: domain.TimestampNative,
		TimestampConfidence: domain.ConfidenceExact, Type: domain.EventUserMessage,
		Role: domain.RoleUser, Content: []domain.ContentBlock{
			{Type: "input_text", Text: &imageOpen},
			{Type: "input_image", Data: json.RawMessage(`{"image_url":"iVBOR-binary-canary"}`)},
			{Type: "input_text", Text: &imageClose},
			{Type: "text", Text: &userText},
		},
		NativeType: "message", Native: json.RawMessage(`{"metadata":"must-not-survive"}`),
	}); err != nil {
		t.Fatal(err)
	}
	closing := "final response tail"
	if err := sink.Add(context.Background(), domain.NativeEvent{
		NativeEventID: "assistant-2", Timestamp: &now, Type: domain.EventAssistantMessage, Role: domain.RoleAssistant,
		Content: []domain.ContentBlock{{Type: "output_text", Text: &closing}},
	}); err != nil {
		t.Fatal(err)
	}
	longResponse := "0123456789abcdefghijklmnopqrstuvwxyz"
	reasoning := "private-reasoning-canary"
	if err := sink.Add(context.Background(), domain.NativeEvent{
		NativeEventID: "assistant-1", Timestamp: &now, TimestampSource: domain.TimestampNative,
		TimestampConfidence: domain.ConfidenceExact, Type: domain.EventAssistantMessage, Role: domain.RoleAssistant,
		Content: []domain.ContentBlock{{Type: "reasoning", Text: &reasoning}, {Type: "output_text", Text: &longResponse}},
		Native:  json.RawMessage(`{"base_config":"must-not-survive"}`),
	}); err != nil {
		t.Fatal(err)
	}
	toolOutput := json.RawMessage(`"tool-output-canary"`)
	if err := sink.Add(context.Background(), domain.NativeEvent{Timestamp: &now, Type: domain.EventToolResult, Role: domain.RoleTool, Tool: &domain.ToolPayload{Output: toolOutput}}); err != nil {
		t.Fatal(err)
	}

	finalized, err := sink.Finalize(context.Background(), domain.ParseResult{})
	if err != nil {
		t.Fatal(err)
	}
	defer finalized.Close()
	if !finalized.Included || finalized.Session.Counts.Events != 2 || finalized.Session.Counts.UserMessages != 1 || finalized.Session.Counts.AssistantMessages != 1 || finalized.Session.Counts.ToolResults != 0 {
		t.Fatalf("incorrect touched counts: %#v", finalized.Session)
	}
	if finalized.Session.Integrity.Status != domain.IntegrityTruncated || finalized.Session.Integrity.TruncatedEvents != 1 {
		t.Fatalf("incorrect truncation aggregate: %#v", finalized.Session.Integrity)
	}
	var events []domain.EventRecord
	if err := finalized.Replay(context.Background(), func(event domain.EventRecord) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 0 || events[1].Sequence != 1 {
		t.Fatalf("source sequence was not preserved: %#v", events)
	}
	if events[0].Content[0].Text == nil || *events[0].Content[0].Text != "[Image attached]\n\n"+userText {
		t.Fatalf("user content changed: %#v", events[0])
	}
	if events[1].Content[0].Text == nil || strings.Contains(*events[1].Content[0].Text, reasoning) || !strings.Contains(*events[1].Content[0].Text, "[TRUNCATED:") || len(events[1].Truncations) != 1 {
		t.Fatalf("assistant preview was not filtered and capped: %#v", events[1])
	}
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		for _, omitted := range []string{"must-not-survive", "tool-output-canary", reasoning, "environment_context", "iVBOR-binary-canary"} {
			if bytes.Contains(encoded, []byte(omitted)) {
				t.Fatalf("usage event retained %q: %s", omitted, encoded)
			}
		}
	}

	var jsonl bytes.Buffer
	if err := finalized.WriteJSONL(context.Background(), &jsonl); err != nil {
		t.Fatal(err)
	}
	firstLine := bytes.SplitN(jsonl.Bytes(), []byte{'\n'}, 2)[0]
	var firstRecord struct {
		RecordType domain.RecordType `json:"record_type"`
	}
	if err := json.Unmarshal(firstLine, &firstRecord); err != nil || firstRecord.RecordType != domain.RecordSession {
		t.Fatalf("session was not emitted first: %s (%v)", firstLine, err)
	}

	if err := finalized.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("spool survived result cleanup: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspacePath); !os.IsNotExist(err) {
		t.Fatalf("workspace survived cleanup: %v", err)
	}
}

func TestEventsOnlyIncludesOnlyInRangeMessages(t *testing.T) {
	workspace, sink, now := testSink(t, domain.ScopeEventsOnly, 128, 1024)
	defer workspace.Close()
	after := now.Add(2 * time.Hour)
	inRange := "in range"
	outRange := "out of range"
	for _, event := range []domain.NativeEvent{
		{NativeEventID: "inside", Timestamp: &now, Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: &inRange}}},
		{NativeEventID: "outside", Timestamp: &after, Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: &outRange}}},
		{NativeEventID: "tool", Timestamp: &now, Type: domain.EventToolCall, Role: domain.RoleAssistant},
	} {
		event.TimestampSource = domain.TimestampNative
		event.TimestampConfidence = domain.ConfidenceExact
		if err := sink.Add(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	finalized, err := sink.Finalize(context.Background(), domain.ParseResult{})
	if err != nil {
		t.Fatal(err)
	}
	defer finalized.Close()
	if finalized.Session.Counts.Events != 1 || finalized.Session.Counts.UserMessages != 1 || finalized.Session.Counts.ToolCalls != 0 {
		t.Fatalf("events-only counts do not match selected stream: %#v", finalized.Session.Counts)
	}
	var IDs []string
	if err := finalized.Replay(context.Background(), func(event domain.EventRecord) error {
		IDs = append(IDs, event.NativeID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(IDs, ",") != "inside" {
		t.Fatalf("unexpected events-only selection: %v", IDs)
	}
}

func TestMalformedRawEncodingSizeLimitAndContentFreeDiagnostics(t *testing.T) {
	workspace, sink, now := testSink(t, domain.ScopeTouched, 128, 32)
	defer workspace.Close()
	if err := sink.Add(context.Background(), domain.NativeEvent{
		NativeEventID: "match", Timestamp: &now, Type: domain.EventUserMessage,
		Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: stringPointer("match")}}, Native: json.RawMessage(`{"text":"match"}`),
	}); err != nil {
		t.Fatal(err)
	}
	line := int64(7)
	binary := []byte{0xff, 0x00, 0xfe, 'x'}
	if err := sink.AddMalformed(context.Background(), MalformedRecord{Raw: binary, SourcePosition: domain.SourcePosition{Line: &line}}); err != nil {
		t.Fatal(err)
	}
	const canary = "DIAGNOSTIC-CANARY-MUST-NOT-LEAK"
	if err := sink.AddMalformed(context.Background(), MalformedRecord{Raw: []byte(strings.Repeat(canary, 2)), SourcePosition: domain.SourcePosition{Line: &line}}); err != nil {
		t.Fatal(err)
	}
	finalized, err := sink.Finalize(context.Background(), domain.ParseResult{})
	if err != nil {
		t.Fatal(err)
	}
	defer finalized.Close()
	if finalized.Session.Integrity.Status != domain.IntegrityPartial || finalized.Session.Integrity.MalformedNativeRecords != 1 {
		t.Fatalf("malformed/size-limit integrity is incorrect: %#v", finalized.Session.Integrity)
	}
	diagnostics := finalized.Diagnostics()
	if len(diagnostics) != 2 || diagnostics[0].Code != "size_limit" || diagnostics[1].Code != "malformed_record" || strings.Contains(diagnostics[0].Error()+diagnostics[1].Error(), canary) {
		t.Fatalf("diagnostics leaked raw content: %#v", diagnostics)
	}
	var events []domain.EventRecord
	if err := finalized.Replay(context.Background(), func(event domain.EventRecord) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.EventUserMessage {
		t.Fatalf("malformed records entered usage transcript: %#v", events)
	}
}

func TestRepeatedNativeEventIDsRemainUnique(t *testing.T) {
	workspace, sink, now := testSink(t, domain.ScopeTouched, 128, 1024)
	defer workspace.Close()
	for line := int64(1); line <= 2; line++ {
		line := line
		if err := sink.Add(context.Background(), domain.NativeEvent{
			NativeEventID: "turn", SourcePosition: domain.SourcePosition{Line: &line},
			Timestamp: &now, Type: domain.EventUserMessage, Role: domain.RoleUser,
			Content: []domain.ContentBlock{{Type: "text", Text: stringPointer("hello")}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	finalized, err := sink.Finalize(context.Background(), domain.ParseResult{})
	if err != nil {
		t.Fatal(err)
	}
	defer finalized.Close()
	ids := map[string]bool{}
	if err := finalized.Replay(context.Background(), func(event domain.EventRecord) error {
		if ids[event.ID] {
			t.Fatalf("duplicate event ID %s", event.ID)
		}
		ids[event.ID] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("events=%d", len(ids))
	}
}

func TestCancellationAndWriteFailureRemoveSpool(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		workspace, sink, _ := testSink(t, domain.ScopeTouched, 128, 1024)
		defer workspace.Close()
		path := sink.path
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := sink.Add(ctx, domain.NativeEvent{})
		if err != context.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cancelled spool survived: %v", err)
		}
	})
	t.Run("write failure", func(t *testing.T) {
		workspace, sink, now := testSink(t, domain.ScopeTouched, 128, 1024)
		defer workspace.Close()
		path := sink.path
		if err := sink.file.Close(); err != nil {
			t.Fatal(err)
		}
		err := sink.Add(context.Background(), domain.NativeEvent{Timestamp: &now, Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: stringPointer("hello")}}})
		if err == nil || strings.Contains(err.Error(), "DISCLOSURE") {
			t.Fatalf("expected content-free write error, got %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed spool survived: %v", err)
		}
	})
}

func TestReplayFailureClosesAndRemovesSpool(t *testing.T) {
	workspace, sink, now := testSink(t, domain.ScopeTouched, 128, 1024)
	if err := sink.Add(context.Background(), domain.NativeEvent{
		NativeEventID: "user", Timestamp: &now, Type: domain.EventUserMessage,
		Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: stringPointer("hello")}},
	}); err != nil {
		t.Fatal(err)
	}
	finalized, err := sink.Finalize(context.Background(), domain.ParseResult{})
	if err != nil {
		t.Fatal(err)
	}
	spoolPath := finalized.SpoolPath()
	if err := finalized.Replay(context.Background(), func(domain.EventRecord) error {
		return errors.New("stop replay")
	}); err == nil {
		t.Fatal("expected replay failure")
	}
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("failed replay left its spool behind: %v", err)
	}
	if err := workspace.Close(); err != nil {
		t.Fatal(err)
	}
}

func stringPointer(value string) *string { return &value }

func testSink(t *testing.T, scope domain.SessionScope, toolLimit, recordLimit int64) (*Workspace, *SessionSink, time.Time) {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.FixedZone("test", 6*60*60))
	workspace, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewSessionSink(workspace, SessionOptions{
		Reference: domain.SessionReference{
			HarnessID: "codex", NativeSessionID: "native-session", CanonicalSourceRoot: "/canonical/root",
			DisplayPath: "/full/source/path/session.jsonl", SourceKind: "jsonl", SourceVersion: "1", SizeBytes: 99,
		},
		Session: domain.SessionRecord{Project: domain.Project{WorkingDirectory: "/private/project", WorkingDirectoryRedacted: false}},
		Range:   domain.TimeRange{From: now.Add(-time.Hour), To: now.Add(time.Hour)}, Scope: scope,
		MaxResponseBytes: toolLimit, MaxNativeRecordBytes: recordLimit,
	})
	if err != nil {
		workspace.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sink.Abort()
		_ = workspace.Close()
	})
	return workspace, sink, now
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}
