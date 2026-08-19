package render

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func TestDirectFormatsUseFocusedConversationData(t *testing.T) {
	now := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	user, assistant := "hello", "short[TRUNCATED]tail"
	session := domain.SessionRecord{
		RecordType: domain.RecordSession, SchemaVersion: domain.SchemaVersion,
		ID: "hh_ses_0123456789abcdefabcd", Harness: "codex",
		Counts:    domain.RecordCounts{Events: 2, UserMessages: 1, AssistantMessages: 1},
		Integrity: domain.Integrity{Status: domain.IntegrityTruncated, TruncatedEvents: 1},
	}
	events := []domain.EventRecord{
		{RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion, ID: "hh_evt_0123456789abcdefabcd", SessionID: session.ID, Timestamp: &now, Sequence: 0, Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Text: &user}}},
		{RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion, ID: "hh_evt_1123456789abcdefabcd", SessionID: session.ID, Timestamp: &now, Sequence: 1, Type: domain.EventAssistantMessage, Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Text: &assistant}}, Truncations: []domain.Truncation{{OriginalBytes: 100, RetainedBytes: 9}}},
	}
	conversation := Conversation{Session: session, Events: eventStream(events)}

	var markdown bytes.Buffer
	if err := WriteMarkdown(context.Background(), &markdown, []Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markdown.String(), "### User") || !strings.Contains(markdown.String(), assistant) {
		t.Fatalf("unexpected Markdown: %s", markdown.String())
	}

	var jsonl bytes.Buffer
	if err := WriteJSONL(context.Background(), &jsonl, []Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(jsonl.String(), `"record_type":"session"`) || !strings.Contains(jsonl.String(), user) {
		t.Fatalf("unexpected JSONL: %s", jsonl.String())
	}

	var metrics bytes.Buffer
	if err := WriteCSV(context.Background(), &metrics, []Conversation{conversation}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metrics.String(), user) || strings.Contains(metrics.String(), assistant) || !strings.Contains(metrics.String(), ",assistant,100,9,") {
		t.Fatalf("unexpected metrics: %s", metrics.String())
	}
}

func eventStream(events []domain.EventRecord) EventStream {
	return func(_ context.Context, yield func(domain.EventRecord) error) error {
		for _, event := range events {
			if err := yield(event); err != nil {
				return err
			}
		}
		return nil
	}
}
