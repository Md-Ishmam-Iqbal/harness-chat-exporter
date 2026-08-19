package claude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

type nativeRecord struct {
	UUID        string          `json:"uuid"`
	ParentUUID  string          `json:"parentUuid"`
	SessionID   string          `json:"sessionId"`
	Timestamp   string          `json:"timestamp"`
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Version     string          `json:"version"`
	CWD         string          `json:"cwd"`
	GitBranch   string          `json:"gitBranch"`
	IsSidechain bool            `json:"isSidechain"`
	AgentID     string          `json:"agentId"`
	Message     json.RawMessage `json:"message"`
	Summary     json.RawMessage `json:"summary"`
	Content     json.RawMessage `json:"content"`
}

type nativeMessage struct {
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   json.RawMessage `json:"usage"`
}

type contentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func mapNativeRecord(raw []byte, line int64) ([]domain.NativeEvent, error) {
	var record nativeRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, err
	}
	if record.Type == "" {
		return nil, fmt.Errorf("missing native type")
	}
	nativeType := record.Type
	if record.Subtype != "" {
		nativeType += "." + record.Subtype
	}
	base := domain.NativeEvent{
		NativeEventID:       firstNonEmpty(record.UUID, fmt.Sprintf("line:%d", line)),
		ParentNativeEventID: record.ParentUUID,
		SourcePosition:      domain.SourcePosition{Line: int64Pointer(line)},
		Timestamp:           parseTimestamp(record.Timestamp),
		TimestampSource:     domain.TimestampNative, TimestampConfidence: domain.ConfidenceExact,
		Type: domain.EventMetadata, Role: domain.RoleHarness,
		NativeType: nativeType,
		Native:     append(json.RawMessage(nil), raw...),
	}
	if base.Timestamp == nil {
		base.TimestampSource = domain.TimestampUnknown
		base.TimestampConfidence = domain.ConfidenceUnknown
	}
	switch record.Type {
	case "user", "assistant", "system":
		if record.Subtype == "compact_boundary" {
			base.Type = domain.EventCompaction
			base.Content = rawBlocks("compaction", firstRaw(record.Content, raw), "native")
			return []domain.NativeEvent{base}, nil
		}
		return mapMessageRecord(base, record)
	case "summary":
		base.Type = domain.EventCompaction
		base.Content = rawBlocks("summary", record.Summary, "native")
	case "system_message", "developer":
		base.Type = domain.EventDeveloperMessage
		base.Role = domain.RoleDeveloper
		base.Content = rawBlocks("message", record.Content, "native")
	default:
		if record.Subtype == "compact_boundary" || strings.Contains(record.Type, "compact") {
			base.Type = domain.EventCompaction
		} else {
			base.Type = domain.EventUnknown
		}
		base.Content = rawBlocks("native", raw, "native")
	}
	return []domain.NativeEvent{base}, nil
}

func mapMessageRecord(base domain.NativeEvent, record nativeRecord) ([]domain.NativeEvent, error) {
	var message nativeMessage
	if err := json.Unmarshal(record.Message, &message); err != nil {
		if len(record.Message) == 0 {
			base.Type = domain.EventMetadata
			base.Content = rawBlocks("native", base.Native, "native")
			return []domain.NativeEvent{base}, nil
		}
		return nil, err
	}
	base.Role = claudeRole(firstNonEmpty(message.Role, record.Type))
	base.Type = claudeMessageType(base.Role)
	if message.Model != "" {
		base.Model = &domain.ModelPayload{Provider: "anthropic", Name: message.Model}
	}
	base.Content = parseClaudeContent(message.Content)

	var parts []contentPart
	if json.Unmarshal(message.Content, &parts) != nil {
		return []domain.NativeEvent{base}, nil
	}
	events := []domain.NativeEvent{}
	visible := []domain.ContentBlock{}
	for index, part := range parts {
		nativeID := fmt.Sprintf("%s:block:%d", base.NativeEventID, index)
		switch part.Type {
		case "tool_use":
			event := childEvent(base, nativeID)
			event.Type = domain.EventToolCall
			event.Role = domain.RoleAssistant
			event.Tool = &domain.ToolPayload{Name: part.Name, CallID: part.ID, Input: cloneRaw(part.Input), Status: "requested"}
			events = append(events, event)
		case "tool_result":
			event := childEvent(base, nativeID)
			event.Type = domain.EventToolResult
			event.Role = domain.RoleTool
			status := "completed"
			if part.IsError {
				status = "error"
			}
			event.Tool = &domain.ToolPayload{CallID: part.ToolUseID, Output: cloneRaw(part.Content), Status: status}
			events = append(events, event)
		case "thinking", "redacted_thinking":
			event := childEvent(base, nativeID)
			event.Type = domain.EventMetadata
			event.Role = domain.RoleAssistant
			blockRaw, _ := json.Marshal(part)
			event.Content = rawBlocks("reasoning", blockRaw, "native")
			events = append(events, event)
		default:
			blockRaw, _ := json.Marshal(part)
			block := domain.ContentBlock{Type: firstNonEmpty(part.Type, "unknown"), Data: blockRaw, Visibility: stringPointer("native")}
			if part.Text != "" {
				block.Text = stringPointer(part.Text)
			}
			visible = append(visible, block)
		}
	}
	if len(visible) > 0 || len(events) == 0 {
		base.Content = visible
		events = append([]domain.NativeEvent{base}, events...)
	} else {
		// When a native message contains only structured blocks, keep its UUID on
		// the first normalized event so later native parentUuid references remain
		// resolvable without inventing a synthetic container event.
		events[0].NativeEventID = base.NativeEventID
	}
	return events, nil
}

func childEvent(base domain.NativeEvent, nativeID string) domain.NativeEvent {
	event := base
	event.NativeEventID = nativeID
	event.Content = nil
	event.Tool = nil
	event.File = nil
	event.Command = nil
	event.Usage = nil
	return event
}

func parseClaudeContent(raw json.RawMessage) []domain.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []domain.ContentBlock{{Type: "text", Text: stringPointer(text), Visibility: stringPointer("native")}}
	}
	return rawBlocks("content", raw, "native")
}

func rawBlocks(kind string, raw json.RawMessage, visibility string) []domain.ContentBlock {
	return []domain.ContentBlock{{Type: kind, Data: cloneRaw(raw), Visibility: stringPointer(visibility)}}
}

func malformedNativeEvent(raw []byte, line int64) domain.NativeEvent {
	data := string(raw)
	encoding := "utf8"
	if !utf8.Valid(raw) {
		data = base64.StdEncoding.EncodeToString(raw)
		encoding = "base64"
	}
	native, _ := json.Marshal(map[string]string{"encoding": encoding, "data": data})
	return domain.NativeEvent{
		NativeEventID: fmt.Sprintf("line:%d", line), SourcePosition: domain.SourcePosition{Line: int64Pointer(line)},
		TimestampSource: domain.TimestampUnknown, TimestampConfidence: domain.ConfidenceUnknown,
		Type: domain.EventUnknown, Role: domain.RoleHarness, NativeType: "malformed",
		Native: native, Content: rawBlocks("malformed", native, "native"),
	}
}

func claudeRole(value string) domain.Role {
	switch value {
	case "user":
		return domain.RoleUser
	case "assistant":
		return domain.RoleAssistant
	case "system":
		return domain.RoleSystem
	case "developer":
		return domain.RoleDeveloper
	default:
		return domain.RoleUnknown
	}
}

func claudeMessageType(value domain.Role) domain.EventType {
	switch value {
	case domain.RoleUser:
		return domain.EventUserMessage
	case domain.RoleAssistant:
		return domain.EventAssistantMessage
	case domain.RoleSystem:
		return domain.EventSystemMessage
	case domain.RoleDeveloper:
		return domain.EventDeveloperMessage
	default:
		return domain.EventUnknown
	}
}

func parseTimestamp(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func int64Pointer(value int64) *int64                { return &value }
func stringPointer(value string) *string             { return &value }
func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}
