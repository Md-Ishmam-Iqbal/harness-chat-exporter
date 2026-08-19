package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID             string `json:"id"`
	SessionID      string `json:"session_id"`
	ForkedFromID   string `json:"forked_from_id"`
	ParentThreadID string `json:"parent_thread_id"`
	Timestamp      string `json:"timestamp"`
	CWD            string `json:"cwd"`
	CLIVersion     string `json:"cli_version"`
	ModelProvider  string `json:"model_provider"`
	AgentNickname  string `json:"agent_nickname"`
	AgentRole      string `json:"agent_role"`
	AgentPath      string `json:"agent_path"`
}

type typedPayload struct {
	Type string `json:"type"`
}

type responseItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	Summary          json.RawMessage `json:"summary"`
	EncryptedContent json.RawMessage `json:"encrypted_content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Input            json.RawMessage `json:"input"`
	Arguments        json.RawMessage `json:"arguments"`
	Output           json.RawMessage `json:"output"`
	Status           string          `json:"status"`
}

func mapRecord(raw []byte, line int64) (domain.NativeEvent, error) {
	var record envelope
	if err := json.Unmarshal(raw, &record); err != nil {
		return domain.NativeEvent{}, err
	}
	if record.Type == "" {
		return domain.NativeEvent{}, fmt.Errorf("missing native type")
	}
	timestamp := parseTimestamp(record.Timestamp)
	event := domain.NativeEvent{
		SourcePosition:  domain.SourcePosition{Line: int64Pointer(line)},
		Timestamp:       timestamp,
		TimestampSource: domain.TimestampNative, TimestampConfidence: domain.ConfidenceExact,
		Type: domain.EventMetadata, Role: domain.RoleHarness,
		NativeType: record.Type, Native: append(json.RawMessage(nil), raw...),
	}
	if timestamp == nil {
		event.TimestampSource = domain.TimestampUnknown
		event.TimestampConfidence = domain.ConfidenceUnknown
	}

	switch record.Type {
	case "session_meta":
		var payload sessionMeta
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return domain.NativeEvent{}, err
		}
		event.NativeEventID = firstNonEmpty(payload.ID, payload.SessionID, sourceID(line))
		if event.Timestamp == nil {
			event.Timestamp = parseTimestamp(payload.Timestamp)
			if event.Timestamp != nil {
				event.TimestampSource = domain.TimestampNative
				event.TimestampConfidence = domain.ConfidenceExact
			}
		}
		event.Content = rawContent("metadata", record.Payload, "native")
	case "turn_context":
		var payload struct {
			TurnID string `json:"turn_id"`
			Model  string `json:"model"`
		}
		if err := json.Unmarshal(record.Payload, &payload); err != nil {
			return domain.NativeEvent{}, err
		}
		event.NativeEventID = firstNonEmpty(payload.TurnID, sourceID(line))
		if payload.Model != "" {
			event.Model = &domain.ModelPayload{Name: payload.Model, Provider: "openai"}
		}
		event.Content = rawContent("turn_context", record.Payload, "native")
	case "world_state", "inter_agent_communication_metadata":
		event.NativeEventID = sourceID(line)
		event.Content = rawContent(record.Type, record.Payload, "native")
	case "response_item":
		return mapResponseItem(event, record.Payload, line)
	case "event_msg":
		return mapEventMessage(event, record.Payload, line)
	case "compacted":
		event.NativeEventID = sourceID(line)
		event.Type = domain.EventCompaction
		event.Role = domain.RoleHarness
		event.Content = rawContent("compaction", record.Payload, "native")
	default:
		event.NativeEventID = sourceID(line)
		event.Type = domain.EventUnknown
		event.Content = rawContent("unknown", record.Payload, "native")
	}
	return event, nil
}

func mapResponseItem(event domain.NativeEvent, raw json.RawMessage, line int64) (domain.NativeEvent, error) {
	var item responseItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return domain.NativeEvent{}, err
	}
	event.NativeType = "response_item." + item.Type
	event.NativeEventID = firstNonEmpty(item.ID, item.CallID, sourceID(line))
	switch item.Type {
	case "message":
		event.Role = role(item.Role)
		event.Type = messageType(event.Role)
		event.Content = parseContent(item.Content, "message")
	case "agent_message":
		event.Role = domain.RoleAssistant
		event.Type = domain.EventAssistantMessage
		event.Content = parseContent(item.Content, "message")
	case "reasoning":
		event.Role = domain.RoleAssistant
		event.Type = domain.EventMetadata
		event.Content = append(parseContent(item.Summary, "reasoning_summary"), parseContent(item.EncryptedContent, "encrypted_reasoning")...)
	case "custom_tool_call", "function_call":
		event.Role = domain.RoleAssistant
		event.Type = domain.EventToolCall
		input := item.Input
		if len(input) == 0 {
			input = item.Arguments
		}
		event.Tool = &domain.ToolPayload{Name: item.Name, CallID: item.CallID,
			Input: cloneRaw(input), Status: item.Status}
		addCommand(&event, item.Name, input, nil)
	case "custom_tool_call_output", "function_call_output":
		event.Role = domain.RoleTool
		event.Type = domain.EventToolResult
		event.Tool = &domain.ToolPayload{CallID: item.CallID, Output: cloneRaw(item.Output), Status: "completed"}
	default:
		event.Type = domain.EventUnknown
		event.Content = rawContent("unknown_response_item", raw, "native")
	}
	return event, nil
}

func mapEventMessage(event domain.NativeEvent, raw json.RawMessage, line int64) (domain.NativeEvent, error) {
	var payload typedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return domain.NativeEvent{}, err
	}
	event.NativeType = "event_msg." + payload.Type
	event.NativeEventID = sourceID(line)
	event.Content = rawContent(payload.Type, raw, "native")
	switch payload.Type {
	case "context_compacted":
		event.Type = domain.EventCompaction
	case "patch_apply_end":
		event.Type = domain.EventFileChange
	case "turn_aborted":
		event.Type = domain.EventError
	case "sub_agent_activity":
		event.Type = domain.EventMetadata
	case "user_message", "agent_message":
		// Response items are the authoritative visible message. Retain this
		// lifecycle copy as native metadata rather than duplicating prose.
		event.Type = domain.EventMetadata
	default:
		event.Type = domain.EventMetadata
	}
	return event, nil
}

func malformedEvent(raw []byte, line int64) domain.NativeEvent {
	data := string(raw)
	typeName := "malformed_utf8"
	if !utf8.Valid(raw) {
		data = base64.StdEncoding.EncodeToString(raw)
		typeName = "malformed_base64"
	}
	encoded, _ := json.Marshal(map[string]string{"encoding": typeName, "data": data})
	return domain.NativeEvent{
		NativeEventID: sourceID(line), SourcePosition: domain.SourcePosition{Line: int64Pointer(line)},
		Type: domain.EventUnknown, Role: domain.RoleHarness, NativeType: "malformed",
		TimestampSource: domain.TimestampUnknown, TimestampConfidence: domain.ConfidenceUnknown,
		Native: encoded, Content: rawContent("malformed", encoded, "native"),
	}
}

func parseContent(raw json.RawMessage, fallbackType string) []domain.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []domain.ContentBlock{{Type: fallbackType, Text: stringPointer(text), Visibility: stringPointer("native")}}
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) == nil {
		result := make([]domain.ContentBlock, 0, len(parts))
		for _, partRaw := range parts {
			var part struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(partRaw, &part) != nil {
				result = append(result, domain.ContentBlock{Type: fallbackType, Data: cloneRaw(partRaw), Visibility: stringPointer("native")})
				continue
			}
			blockType := firstNonEmpty(part.Type, fallbackType)
			block := domain.ContentBlock{Type: blockType, Data: cloneRaw(partRaw), Visibility: stringPointer("native")}
			if part.Text != "" {
				block.Text = stringPointer(part.Text)
			}
			result = append(result, block)
		}
		return result
	}
	return rawContent(fallbackType, raw, "native")
}

func rawContent(kind string, raw json.RawMessage, visibility string) []domain.ContentBlock {
	return []domain.ContentBlock{{Type: kind, Data: cloneRaw(raw), Visibility: stringPointer(visibility)}}
}

func addCommand(event *domain.NativeEvent, name string, input, output json.RawMessage) {
	lower := strings.ToLower(name)
	if name != "" && !strings.Contains(lower, "exec") && !strings.Contains(lower, "shell") && !strings.Contains(lower, "command") {
		return
	}
	var value struct {
		Cmd     string `json:"cmd"`
		Command string `json:"command"`
		CWD     string `json:"cwd"`
	}
	if len(input) > 0 {
		decoded := input
		var nested string
		if json.Unmarshal(input, &nested) == nil && json.Valid([]byte(nested)) {
			decoded = json.RawMessage(nested)
		}
		_ = json.Unmarshal(decoded, &value)
	}
	if value.Cmd == "" {
		value.Cmd = value.Command
	}
	if value.Cmd != "" || output != nil {
		event.Command = &domain.CommandPayload{Text: value.Cmd, WorkingDirectory: value.CWD}
		if event.Type == domain.EventToolCall {
			event.Type = domain.EventCommand
		} else if event.Type == domain.EventToolResult {
			event.Type = domain.EventCommandResult
		}
	}
}

func role(value string) domain.Role {
	switch value {
	case "user":
		return domain.RoleUser
	case "assistant":
		return domain.RoleAssistant
	case "system":
		return domain.RoleSystem
	case "developer":
		return domain.RoleDeveloper
	case "tool":
		return domain.RoleTool
	default:
		return domain.RoleUnknown
	}
}

func messageType(value domain.Role) domain.EventType {
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
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func sourceID(line int64) string                     { return fmt.Sprintf("line:%d", line) }
func int64Pointer(value int64) *int64                { return &value }
func stringPointer(value string) *string             { return &value }
func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
