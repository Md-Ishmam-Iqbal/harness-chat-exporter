package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

func TestGeneratedClaudeFixture(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-Users-example-private-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "..", "..", "testdata", "claude", "current", "session-00000000-0000-4000-8000-000000000001.jsonl")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(project, "00000000-0000-4000-8000-000000000001.jsonl")
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New()
	detection := adapter.Detect(context.Background(), domain.Environment{HomeDir: home, Variables: map[string]string{}})
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
	if !probe.Supported || probe.NativeSessionID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("unexpected probe: %#v", probe)
	}
	var events []domain.NativeEvent
	result := adapter.Parse(context.Background(), references[0], func(_ context.Context, event domain.NativeEvent) error {
		events = append(events, event)
		return nil
	})
	if result.Err != nil || !result.Partial || result.Malformed != 1 {
		t.Fatalf("unexpected parse result: %#v", result)
	}
	counts := map[domain.EventType]int{}
	native := ""
	for _, event := range events {
		counts[event.Type]++
		native += string(event.Native)
	}
	if counts[domain.EventUserMessage] != 1 || counts[domain.EventAssistantMessage] != 1 || counts[domain.EventToolCall] != 1 || counts[domain.EventToolResult] != 1 || counts[domain.EventCompaction] != 1 {
		t.Fatalf("unexpected event mapping: %#v", counts)
	}
	for _, canary := range []string{"anthropic-disclosure-canary", "private-thinking-canary", "future-claude-canary"} {
		if !strings.Contains(native, canary) {
			t.Fatalf("native disclosure did not preserve %q", canary)
		}
	}
}

func TestClaudeConfigDirPrecedence(t *testing.T) {
	explicit := t.TempDir()
	environmentRoot := t.TempDir()
	for _, root := range []string{explicit, environmentRoot} {
		if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	adapter := New(explicit)
	detection := adapter.Detect(context.Background(), domain.Environment{
		HomeDir: t.TempDir(), Variables: map[string]string{"CLAUDE_CONFIG_DIR": environmentRoot},
	})
	if len(detection.Roots) != 2 || detection.Roots[0].Origin != "explicit" || detection.Roots[1].Origin != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("unexpected precedence: %#v", detection.Roots)
	}
}
