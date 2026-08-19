package render

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func TestSessionRendersOnlyConversationText(t *testing.T) {
	user := "hello </details><script>alert(1)</script> and ``` fence"
	assistant := "short answer"
	session := domain.SessionRecord{
		ID: "hh_ses_0123456789abcdefabcd", Harness: "codex", NativeSessionID: "native-canary",
		Title: "metadata-canary", Summary: stringPointer("summary-canary"),
	}
	events := []domain.EventRecord{
		{Type: domain.EventUserMessage, Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: &user}}},
		{Type: domain.EventToolCall, Role: domain.RoleAssistant, Native: []byte(`{"tool":"canary"}`)},
		{Type: domain.EventAssistantMessage, Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: &assistant}}},
	}
	var output bytes.Buffer
	err := WriteSession(context.Background(), &output, session, func(_ context.Context, yield func(domain.EventRecord) error) error {
		for _, event := range events {
			if err := yield(event); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "# Conversation\n\n## User\n\n````text\n" + user + "\n````\n\n## Assistant\n\n```text\n" + assistant + "\n```\n\n"
	if output.String() != want {
		t.Fatalf("render mismatch\n--- got ---\n%s\n--- want ---\n%s", output.String(), want)
	}
	for _, omitted := range []string{"native-canary", "metadata-canary", "summary-canary", "tool-canary"} {
		if strings.Contains(output.String(), omitted) {
			t.Fatalf("render leaked %q", omitted)
		}
	}
}

func TestSessionPathRejectsTranscriptControlledComponents(t *testing.T) {
	valid := domain.SessionRecord{Harness: "codex", ID: "hh_ses_0123456789abcdefabcd"}
	name, err := SessionPath(valid)
	if err != nil || name != "conversations/codex/hh_ses_0123456789abcdefabcd.md" {
		t.Fatalf("unexpected path %q: %v", name, err)
	}
	for _, bad := range []domain.SessionRecord{
		{Harness: "../codex", ID: valid.ID},
		{Harness: "Codex", ID: valid.ID},
		{Harness: "codex", ID: "../../escape"},
	} {
		if _, err := SessionPath(bad); err == nil {
			t.Fatalf("accepted unsafe session path input: %#v", bad)
		}
	}
}

func stringPointer(value string) *string { return &value }
