package render

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

// Conversation is the common, replayable input used by direct output formats.
type Conversation struct {
	Session domain.SessionRecord
	Events  EventStream
}

func WriteMarkdown(ctx context.Context, w io.Writer, conversations []Conversation) error {
	if _, err := io.WriteString(w, "# Conversations\n\n"); err != nil {
		return err
	}
	for index, conversation := range conversations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "## Conversation %d\n\nHarness: `%s`\n\n", index+1, escapeCode(conversation.Session.Harness)); err != nil {
			return err
		}
		if err := writeConversationEvents(ctx, w, conversation.Events, "###"); err != nil {
			return err
		}
	}
	return nil
}

type usageSessionRecord struct {
	RecordType    domain.RecordType   `json:"record_type"`
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	Harness       string              `json:"harness"`
	Counts        domain.RecordCounts `json:"counts"`
	Integrity     domain.Integrity    `json:"integrity"`
}

type usageEventRecord struct {
	RecordType    domain.RecordType   `json:"record_type"`
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	SessionID     string              `json:"session_id"`
	Sequence      int64               `json:"sequence"`
	Type          domain.EventType    `json:"type"`
	Role          domain.Role         `json:"role"`
	Content       []usageContentBlock `json:"content"`
	Truncations   []domain.Truncation `json:"truncations,omitempty"`
}

type usageContentBlock struct {
	Text string `json:"text"`
}

func WriteJSONL(ctx context.Context, w io.Writer, conversations []Conversation) error {
	encoder := json.NewEncoder(w)
	for _, conversation := range conversations {
		session := conversation.Session
		if err := encoder.Encode(usageSessionRecord{
			RecordType: session.RecordType, SchemaVersion: session.SchemaVersion,
			ID: session.ID, Harness: session.Harness, Counts: session.Counts, Integrity: session.Integrity,
		}); err != nil {
			return err
		}
		if err := conversation.Events(ctx, func(event domain.EventRecord) error {
			content := make([]usageContentBlock, 0, len(event.Content))
			for _, block := range event.Content {
				if block.Text != nil {
					content = append(content, usageContentBlock{Text: *block.Text})
				}
			}
			return encoder.Encode(usageEventRecord{
				RecordType: event.RecordType, SchemaVersion: event.SchemaVersion,
				ID: event.ID, SessionID: event.SessionID, Sequence: event.Sequence,
				Type: event.Type, Role: event.Role, Content: content, Truncations: event.Truncations,
			})
		}); err != nil {
			return err
		}
	}
	return nil
}

// WriteCSV emits content-free usage metrics, one row per selected message.
func WriteCSV(ctx context.Context, w io.Writer, conversations []Conversation) error {
	writer := csv.NewWriter(w)
	if err := writer.Write([]string{"timestamp", "date", "harness", "session_id", "role", "original_bytes", "retained_bytes", "preview_characters", "truncated"}); err != nil {
		return err
	}
	for _, conversation := range conversations {
		if err := conversation.Events(ctx, func(event domain.EventRecord) error {
			var content strings.Builder
			for _, block := range event.Content {
				if block.Text == nil {
					continue
				}
				if content.Len() > 0 {
					content.WriteString("\n\n")
				}
				content.WriteString(*block.Text)
			}
			timestamp, date := "", ""
			if event.Timestamp != nil {
				timestamp = event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
				date = event.Timestamp.UTC().Format("2006-01-02")
			}
			text := content.String()
			originalBytes := int64(len([]byte(text)))
			retainedBytes := originalBytes
			if len(event.Truncations) > 0 {
				originalBytes = event.Truncations[0].OriginalBytes
				retainedBytes = event.Truncations[0].RetainedBytes
			}
			return writer.Write([]string{
				timestamp, date, conversation.Session.Harness, conversation.Session.ID, string(event.Role),
				strconv.FormatInt(originalBytes, 10), strconv.FormatInt(retainedBytes, 10),
				strconv.Itoa(utf8.RuneCountInString(text)), strconv.FormatBool(len(event.Truncations) > 0),
			})
		}); err != nil {
			return err
		}
	}
	writer.Flush()
	return writer.Error()
}
