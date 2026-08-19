package codex

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

func TestDetectDiscoverProbeAndParse(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	sessionDir := filepath.Join(root, "sessions", "2026", "08", "06")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(sessionDir, filepath.Base(fixture))
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New()
	environment := domain.Environment{HomeDir: home, Variables: map[string]string{}}
	detection := adapter.Detect(context.Background(), environment)
	if detection.Status != domain.DetectionDetected || len(detection.Roots) != 1 {
		t.Fatalf("unexpected detection: %#v", detection)
	}
	var references []domain.SessionReference
	err = adapter.Discover(context.Background(), domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
		references = append(references, reference)
		return nil
	})
	if err != nil || len(references) != 1 {
		t.Fatalf("discover: refs=%d err=%v", len(references), err)
	}
	probe := adapter.Probe(context.Background(), references[0])
	if !probe.Supported || probe.NativeSessionID != "019fd000-0000-7000-8000-000000000001" || probe.SourceVersion != "0.146.1" {
		t.Fatalf("unexpected probe: %#v", probe)
	}

	var events []domain.NativeEvent
	result := adapter.Parse(context.Background(), references[0], func(_ context.Context, event domain.NativeEvent) error {
		events = append(events, event)
		return nil
	})
	if result.Err != nil || !result.Partial || result.Malformed != 1 || len(events) != 11 {
		t.Fatalf("unexpected parse result=%#v events=%d", result, len(events))
	}
	allNative := ""
	counts := map[domain.EventType]int{}
	for _, event := range events {
		allNative += string(event.Native)
		counts[event.Type]++
	}
	if !strings.Contains(allNative, "sk-test-disclosure-canary") || !strings.Contains(allNative, "encrypted-reasoning-canary") || !strings.Contains(allNative, "future-secret-canary") {
		t.Fatal("full-disclosure canaries were not retained")
	}
	if counts[domain.EventUserMessage] != 1 || counts[domain.EventAssistantMessage] != 1 || counts[domain.EventCommand] != 1 || counts[domain.EventCommandResult] != 1 {
		t.Fatalf("unexpected event mapping counts: %#v", counts)
	}
}

func TestReadRecordEnforcesLogicalLimit(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 100)+"\n{}\n"), 8)
	value, large, err := readRecord(reader, 16)
	if err != nil || !large || len(value) != 0 {
		t.Fatalf("first record = %q large=%v err=%v", value, large, err)
	}
	value, large, err = readRecord(reader, 16)
	if err != nil || large || string(value) != "{}" {
		t.Fatalf("second record = %q large=%v err=%v", value, large, err)
	}
}

func TestRangeFixtureTimestamp(t *testing.T) {
	event, err := mapRecord([]byte(`{"timestamp":"2026-08-06T10:00:00+06:00","type":"world_state","payload":{"full":true}}`), 1)
	if err != nil || event.Timestamp == nil || !event.Timestamp.Equal(time.Date(2026, 8, 6, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("timestamp normalization failed: %#v %v", event.Timestamp, err)
	}
}

func TestProbePreservesSubagentRelationship(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := []byte(`{"timestamp":"2026-08-06T10:00:00Z","type":"session_meta","payload":{"id":"child","parent_thread_id":"parent","agent_nickname":"worker","agent_role":"explorer","agent_path":"worker.toml"}}` + "\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	probe := New().Probe(context.Background(), domain.SessionReference{CanonicalSourceIdentity: path})
	if !probe.Supported || probe.Metadata["parent_session_id"] != "parent" || probe.Metadata["subagent"] != "true" || probe.Metadata["agent_role"] != "explorer" {
		t.Fatalf("unexpected subagent metadata: %#v", probe)
	}
}

func TestFunctionArgumentsStringMapsCommandWithoutMisclassifyingOtherResults(t *testing.T) {
	call, err := mapRecord([]byte(`{"timestamp":"2026-08-06T10:00:00Z","type":"response_item","payload":{"type":"function_call","id":"call-item","call_id":"call-1","name":"exec_command","arguments":"{\"cmd\":\"go test ./...\",\"cwd\":\"/project\"}"}}`), 1)
	if err != nil {
		t.Fatal(err)
	}
	if call.Type != domain.EventCommand || call.Command == nil || call.Command.Text != "go test ./..." || call.Command.WorkingDirectory != "/project" {
		t.Fatalf("nested command arguments were not mapped: %#v", call)
	}
	result, err := mapRecord([]byte(`{"timestamp":"2026-08-06T10:00:01Z","type":"response_item","payload":{"type":"function_call_output","call_id":"non-command","output":"ok"}}`), 2)
	if err != nil {
		t.Fatal(err)
	}
	if result.Type != domain.EventToolResult || result.Command != nil {
		t.Fatalf("unlinked function result was misclassified as a command: %#v", result)
	}
}
