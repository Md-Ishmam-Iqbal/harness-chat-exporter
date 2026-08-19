package pi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type nativeEntry struct {
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	ParentID      *string         `json:"parentId"`
	Timestamp     string          `json:"timestamp"`
	Message       json.RawMessage `json:"message"`
	Provider      string          `json:"provider"`
	ModelID       string          `json:"modelId"`
	ThinkingLevel string          `json:"thinkingLevel"`
	Summary       string          `json:"summary"`
	CustomType    string          `json:"customType"`
	Data          json.RawMessage `json:"data"`
	Content       json.RawMessage `json:"content"`
	Display       *bool           `json:"display"`
	TargetID      string          `json:"targetId"`
	Label         *string         `json:"label"`
	Name          string          `json:"name"`
}

type nativeMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Timestamp  json.RawMessage `json:"timestamp"`
	Provider   string          `json:"provider"`
	Model      string          `json:"model"`
	Usage      json.RawMessage `json:"usage"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
	CustomType string          `json:"customType"`
	Display    *bool           `json:"display"`
	Details    json.RawMessage `json:"details"`
	Command    string          `json:"command"`
	Output     string          `json:"output"`
}

type nativeContentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	MimeType  string          `json:"mimeType"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func mapEntry(raw []byte, node nodeMeta, line int64, version int, branch branchInfo) ([]domain.NativeEvent, error) {
	var entry nativeEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, err
	}
	if entry.Type == "" {
		return nil, fmt.Errorf("missing native entry type")
	}
	base := domain.NativeEvent{
		NativeEventID:       firstNonEmpty(node.id, entry.ID, sourceID(line)),
		ParentNativeEventID: node.parentID,
		SourcePosition:      domain.SourcePosition{Line: int64Pointer(line)},
		Timestamp:           parseTimestamp(entry.Timestamp),
		TimestampSource:     domain.TimestampNative, TimestampConfidence: domain.ConfidenceExact,
		Type: domain.EventMetadata, Role: domain.RoleHarness,
		NativeType: entry.Type, Native: cloneRaw(raw),
	}
	if base.Timestamp == nil {
		base.TimestampSource = domain.TimestampUnknown
		base.TimestampConfidence = domain.ConfidenceUnknown
	}
	if version >= 2 {
		base.Branch = makeBranchReference(branch)
	}

	switch entry.Type {
	case "message":
		return mapMessage(base, entry.Message)
	case "model_change":
		base.Type = domain.EventModelChange
		base.Model = &domain.ModelPayload{Provider: entry.Provider, Name: entry.ModelID}
		base.Content = rawContent("model_change", raw, "native")
	case "thinking_level_change":
		base.Content = textContent("thinking_level", entry.ThinkingLevel, raw, "native_setting")
	case "compaction":
		base.Type = domain.EventCompaction
		base.Content = textContent("compaction_summary", entry.Summary, raw, "native")
	case "branch_summary":
		base.Type = domain.EventBranch
		base.Content = textContent("branch_summary", entry.Summary, raw, "native")
	case "custom":
		base.Content = rawContent(firstNonEmpty(entry.CustomType, "custom"), firstRaw(entry.Data, raw), "extension_state_hidden")
	case "custom_message":
		visibility := displayVisibility(entry.Display)
		base.Content = parseContent(entry.Content, visibility)
		if len(base.Content) == 0 {
			base.Content = rawContent(firstNonEmpty(entry.CustomType, "custom_message"), raw, visibility)
		}
	case "label":
		base.Type = domain.EventBranch
		text := ""
		if entry.Label != nil {
			text = *entry.Label
		}
		base.Content = textContent("label", text, raw, "native")
	case "session_info":
		base.Content = textContent("session_info", entry.Name, raw, "native")
	default:
		base.Type = domain.EventUnknown
		base.Content = rawContent("unknown", raw, "native")
	}
	return []domain.NativeEvent{base}, nil
}

func mapMessage(base domain.NativeEvent, raw json.RawMessage) ([]domain.NativeEvent, error) {
	var message nativeMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return nil, err
	}
	if timestamp := parseMessageTimestamp(message.Timestamp); base.Timestamp == nil && timestamp != nil {
		base.Timestamp = timestamp
		base.TimestampSource = domain.TimestampNative
		base.TimestampConfidence = domain.ConfidenceExact
	}
	base.NativeType = "message." + firstNonEmpty(message.Role, "unknown")
	if message.Provider != "" || message.Model != "" {
		base.Model = &domain.ModelPayload{Provider: message.Provider, Name: message.Model}
	}
	base.Usage = parseUsage(message.Usage)

	switch message.Role {
	case "user":
		base.Type, base.Role = domain.EventUserMessage, domain.RoleUser
		base.Content = parseContent(message.Content, "visible")
	case "assistant":
		base.Type, base.Role = domain.EventAssistantMessage, domain.RoleAssistant
		return mapAssistantMessage(base, message.Content), nil
	case "toolResult", "tool_result":
		base.Type, base.Role = domain.EventToolResult, domain.RoleTool
		status := "completed"
		if message.IsError {
			status = "error"
		}
		base.Tool = &domain.ToolPayload{Name: message.ToolName, CallID: message.ToolCallID,
			Output: cloneRaw(message.Content), Status: status}
		base.Content = parseContent(message.Content, "visible")
		if strings.EqualFold(message.ToolName, "bash") {
			base.Type = domain.EventCommandResult
			base.Command = &domain.CommandPayload{ExitCode: parseExitCode(message.Details)}
		}
	case "custom", "hookMessage":
		base.Type, base.Role = domain.EventMetadata, domain.RoleHarness
		base.Content = parseContent(message.Content, displayVisibility(message.Display))
		if len(base.Content) == 0 {
			base.Content = rawContent(firstNonEmpty(message.CustomType, "custom_message"), raw, displayVisibility(message.Display))
		}
	case "branchSummary":
		base.Type, base.Role = domain.EventBranch, domain.RoleHarness
		base.Content = parseContent(message.Content, "native")
	case "compactionSummary":
		base.Type, base.Role = domain.EventCompaction, domain.RoleHarness
		base.Content = parseContent(message.Content, "native")
	case "bashExecution":
		base.Type, base.Role = domain.EventCommandResult, domain.RoleTool
		base.Command = &domain.CommandPayload{Text: message.Command}
		base.Content = textContent("command_output", message.Output, raw, "native")
	default:
		base.Type, base.Role = domain.EventUnknown, domain.RoleUnknown
		base.Content = rawContent("unknown_message", raw, "native")
	}
	return []domain.NativeEvent{base}, nil
}

func mapAssistantMessage(base domain.NativeEvent, raw json.RawMessage) []domain.NativeEvent {
	var parts []nativeContentPart
	if json.Unmarshal(raw, &parts) != nil {
		base.Content = parseContent(raw, "visible")
		return []domain.NativeEvent{base}
	}
	var content []domain.ContentBlock
	var calls []domain.NativeEvent
	for i, part := range parts {
		partRaw, _ := json.Marshal(part)
		switch part.Type {
		case "toolCall", "tool_call":
			call := base
			call.NativeEventID = fmt.Sprintf("%s:tool:%d", base.NativeEventID, i)
			call.ParentNativeEventID = base.NativeEventID
			call.Type = domain.EventToolCall
			call.Content = nil
			call.Usage = nil
			call.Tool = &domain.ToolPayload{Name: part.Name, CallID: part.ID, Input: cloneRaw(part.Arguments), Status: "requested"}
			if strings.EqualFold(part.Name, "bash") {
				call.Type = domain.EventCommand
				var arguments struct {
					Command string `json:"command"`
					CWD     string `json:"cwd"`
				}
				_ = json.Unmarshal(part.Arguments, &arguments)
				call.Command = &domain.CommandPayload{Text: arguments.Command, WorkingDirectory: arguments.CWD}
			}
			if call.Branch != nil {
				branch := *call.Branch
				branch.ParentID = stringPointer(base.NativeEventID)
				call.Branch = &branch
			}
			calls = append(calls, call)
		case "thinking":
			content = append(content, domain.ContentBlock{
				Type: "reasoning", Text: stringPointer(part.Thinking), Data: cloneRaw(partRaw),
				Visibility: stringPointer("harness_stored"),
			})
		default:
			block := domain.ContentBlock{Type: firstNonEmpty(part.Type, "content"), Data: cloneRaw(partRaw), Visibility: stringPointer("visible")}
			if part.Text != "" {
				block.Text = stringPointer(part.Text)
			}
			if part.MimeType != "" {
				block.MediaType = stringPointer(part.MimeType)
			}
			content = append(content, block)
		}
	}
	base.Content = content
	return append([]domain.NativeEvent{base}, calls...)
}

func makeBranchReference(info branchInfo) *domain.BranchReference {
	return &domain.BranchReference{BranchID: info.id, ActivePath: info.active, ParentID: info.parentID, Label: info.label}
}

func headerEvent(raw []byte, header sessionHeader, line int64) domain.NativeEvent {
	event := domain.NativeEvent{
		NativeEventID:   "header:" + firstNonEmpty(header.ID, sourceID(line)),
		SourcePosition:  domain.SourcePosition{Line: int64Pointer(line)},
		Timestamp:       parseTimestamp(header.Timestamp),
		TimestampSource: domain.TimestampNative, TimestampConfidence: domain.ConfidenceExact,
		Type: domain.EventMetadata, Role: domain.RoleHarness, NativeType: "session", Native: cloneRaw(raw),
		Content: rawContent("session_header", raw, "native"),
	}
	if event.Timestamp == nil {
		event.TimestampSource = domain.TimestampUnknown
		event.TimestampConfidence = domain.ConfidenceUnknown
	}
	return event
}

func unknownEvent(raw []byte, node nodeMeta, line int64, kind string) domain.NativeEvent {
	return domain.NativeEvent{
		NativeEventID: firstNonEmpty(node.id, sourceID(line)), ParentNativeEventID: node.parentID,
		SourcePosition:  domain.SourcePosition{Line: int64Pointer(line)},
		TimestampSource: domain.TimestampUnknown, TimestampConfidence: domain.ConfidenceUnknown,
		Type: domain.EventUnknown, Role: domain.RoleHarness, NativeType: kind,
		Native: cloneRaw(raw), Content: rawContent(kind, raw, "native"),
	}
}

func malformedEvent(raw []byte, line int64) domain.NativeEvent {
	data := string(raw)
	encoding := "utf8"
	if !utf8.Valid(raw) {
		data = base64.StdEncoding.EncodeToString(raw)
		encoding = "base64"
	}
	native, _ := json.Marshal(map[string]string{"encoding": encoding, "data": data})
	return domain.NativeEvent{
		NativeEventID: sourceID(line), SourcePosition: domain.SourcePosition{Line: int64Pointer(line)},
		TimestampSource: domain.TimestampUnknown, TimestampConfidence: domain.ConfidenceUnknown,
		Type: domain.EventUnknown, Role: domain.RoleHarness, NativeType: "malformed",
		Native: native, Content: rawContent("malformed", native, "native"),
	}
}

func parseContent(raw json.RawMessage, visibility string) []domain.ContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []domain.ContentBlock{{Type: "text", Text: stringPointer(text), Data: cloneRaw(raw), Visibility: stringPointer(visibility)}}
	}
	var parts []nativeContentPart
	if json.Unmarshal(raw, &parts) == nil {
		blocks := make([]domain.ContentBlock, 0, len(parts))
		for _, part := range parts {
			partRaw, _ := json.Marshal(part)
			block := domain.ContentBlock{Type: firstNonEmpty(part.Type, "content"), Data: partRaw, Visibility: stringPointer(visibility)}
			if part.Text != "" {
				block.Text = stringPointer(part.Text)
			}
			if part.Thinking != "" {
				block.Type = "reasoning"
				block.Text = stringPointer(part.Thinking)
				block.Visibility = stringPointer("harness_stored")
			}
			if part.MimeType != "" {
				block.MediaType = stringPointer(part.MimeType)
			}
			blocks = append(blocks, block)
		}
		return blocks
	}
	return rawContent("content", raw, visibility)
}

func textContent(kind, value string, raw json.RawMessage, visibility string) []domain.ContentBlock {
	block := domain.ContentBlock{Type: kind, Data: cloneRaw(raw), Visibility: stringPointer(visibility)}
	if value != "" {
		block.Text = stringPointer(value)
	}
	return []domain.ContentBlock{block}
}

func rawContent(kind string, raw json.RawMessage, visibility string) []domain.ContentBlock {
	return []domain.ContentBlock{{Type: kind, Data: cloneRaw(raw), Visibility: stringPointer(visibility)}}
}

func displayVisibility(display *bool) string {
	if display != nil && *display {
		return "displayed"
	}
	return "hidden"
}

func parseUsage(raw json.RawMessage) *domain.UsagePayload {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value struct {
		Input        *int64   `json:"input"`
		Output       *int64   `json:"output"`
		CacheRead    *int64   `json:"cacheRead"`
		CacheWrite   *int64   `json:"cacheWrite"`
		InputTokens  *int64   `json:"inputTokens"`
		OutputTokens *int64   `json:"outputTokens"`
		Cost         *float64 `json:"cost"`
		Currency     *string  `json:"currency"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	return &domain.UsagePayload{
		InputTokens: firstInt64(value.Input, value.InputTokens), OutputTokens: firstInt64(value.Output, value.OutputTokens),
		CacheReadTokens: value.CacheRead, CacheWriteTokens: value.CacheWrite, Cost: value.Cost, Currency: value.Currency,
	}
}

func parseExitCode(raw json.RawMessage) *int {
	var value struct {
		ExitCode *int `json:"exitCode"`
	}
	if json.Unmarshal(raw, &value) == nil {
		return value.ExitCode
	}
	return nil
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

func parseMessageTimestamp(raw json.RawMessage) *time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		milliseconds, err := strconv.ParseInt(number.String(), 10, 64)
		if err == nil {
			parsed := time.UnixMilli(milliseconds).UTC()
			return &parsed
		}
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return parseTimestamp(value)
	}
	return nil
}

func firstInt64(values ...*int64) *int64 {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstRaw(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 && string(value) != "null" {
			return value
		}
	}
	return nil
}

func sourceID(line int64) string            { return fmt.Sprintf("line:%d", line) }
func int64Pointer(value int64) *int64       { return &value }
func stringPointer(value string) *string    { return &value }
func cloneRaw(value []byte) json.RawMessage { return append(json.RawMessage(nil), value...) }
