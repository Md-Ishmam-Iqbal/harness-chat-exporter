package opencode

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func metadataEvent(id, nativeType string, native json.RawMessage, timestamp *time.Time, index int64) domain.NativeEvent {
	return domain.NativeEvent{
		NativeEventID: id, SourcePosition: domain.SourcePosition{Index: int64Pointer(index)},
		Timestamp: timestamp, TimestampSource: timestampSource(timestamp), TimestampConfidence: timestampConfidence(timestamp),
		Type: domain.EventMetadata, Role: domain.RoleHarness, NativeType: nativeType,
		Native: cloneRaw(native), Content: []domain.ContentBlock{{Type: "metadata", Data: cloneRaw(native), Visibility: stringPointer("native")}},
	}
}

func mapMessageMetadata(row map[string]any, messageID string, native json.RawMessage, index int64) domain.NativeEvent {
	data, _ := rowDataObject(row)
	timestamp := firstTime(row, "time_created", "created_at", "created")
	if timestamp == nil {
		timestamp = nestedTime(data, []string{"time", "created"}, []string{"time", "start"})
	}
	event := metadataEvent(messageID+":metadata", "message", native, timestamp, index)
	event.Role = parseRole(valueAt(data, "role"))
	event.Content[0].Type = "message_metadata"
	if valueAt(data, "error") != nil {
		event.Type = domain.EventError
		event.Content[0].Type = "message_error"
	}
	event.Model = modelFrom(data)
	event.Usage = usageFrom(data)
	return event
}

func mapPart(messageRow, partRow, sessionRow map[string]any, index int64, generation string) ([]domain.NativeEvent, []domain.Warning) {
	data, dataErr := rowDataObject(partRow)
	partID := firstNonEmpty(valueString(partRow["id"]), valueString(data["id"]), fmt.Sprintf("part:%d", index))
	messageID := firstNonEmpty(valueString(messageRow["id"]), valueString(data["messageID"]), valueString(data["message_id"]))
	native := marshalNative(map[string]any{
		"session": sessionRow, "message": messageRow, "part": partRow, "storage_generation": generation,
	})
	if dataErr != nil {
		event := basePartEvent(partID, messageID, "part.malformed_data", native, nil, index, domain.RoleUnknown)
		event.Type = domain.EventUnknown
		event.Content = []domain.ContentBlock{{Type: "malformed_part_data", Data: cloneRaw(native), Visibility: stringPointer("native")}}
		return []domain.NativeEvent{event}, []domain.Warning{warn("opencode_malformed_part_data", "malformed_record", "part data is not valid JSON")}
	}

	partType := valueString(data["type"])
	messageData, _ := rowDataObject(messageRow)
	role := parseRole(valueAt(messageData, "role"))
	timestamp := firstTime(partRow, "time_created", "created_at", "created")
	if timestamp == nil {
		timestamp = nestedTime(data, []string{"time", "start"}, []string{"time", "created"}, []string{"time", "end"})
	}
	if timestamp == nil {
		timestamp = firstTime(messageRow, "time_created", "created_at", "created")
	}
	event := basePartEvent(partID, messageID, "part."+partType, native, timestamp, index, role)
	event.Model = firstModel(modelFrom(data), modelFrom(messageData))
	event.Usage = firstUsage(usageFrom(data), usageFrom(messageData))
	dataRaw := rawObject(data)

	switch partType {
	case "text":
		event.Type = messageType(role)
		event.Content = textContent("text", valueString(data["text"]), dataRaw, visibility(data))
	case "reasoning", "thinking":
		event.Type = domain.EventMetadata
		event.Role = domain.RoleAssistant
		event.Content = textContent("reasoning", valueString(data["text"]), dataRaw, visibility(data))
	case "tool":
		return mapToolPart(event, data, dataRaw), nil
	case "patch":
		event.Type = domain.EventFileChange
		event.File = patchFile(data)
		event.Content = rawBlock("patch", dataRaw, visibility(data))
	case "file":
		event.Type = domain.EventMetadata
		event.File = &domain.FilePayload{Path: firstNonEmpty(valueString(data["filename"]), valueString(data["url"])), Operation: "reference"}
		event.Content = rawBlock("file", dataRaw, visibility(data))
	case "compaction":
		event.Type = domain.EventCompaction
		event.Content = rawBlock("compaction", dataRaw, visibility(data))
	case "subtask", "agent":
		event.Type = domain.EventSubagentStart
		event.Content = rawBlock(partType, dataRaw, visibility(data))
	case "retry":
		event.Type = domain.EventError
		event.Content = rawBlock("retry", dataRaw, visibility(data))
	case "step-start", "step-finish", "snapshot":
		event.Type = domain.EventMetadata
		event.Content = rawBlock(partType, dataRaw, visibility(data))
	case "":
		event.Type = domain.EventUnknown
		event.NativeType = "part.unknown"
		event.Content = rawBlock("unknown", dataRaw, visibility(data))
		return []domain.NativeEvent{event}, []domain.Warning{warn("opencode_part_type_missing", "unsupported_format", "part data does not declare a type")}
	default:
		event.Type = domain.EventUnknown
		event.Content = rawBlock("unknown_part", dataRaw, visibility(data))
		return []domain.NativeEvent{event}, []domain.Warning{warn("opencode_unknown_part_type", "unsupported_format", "unknown OpenCode part type was preserved")}
	}
	return []domain.NativeEvent{event}, nil
}

func basePartEvent(partID, messageID, nativeType string, native json.RawMessage, timestamp *time.Time, index int64, role domain.Role) domain.NativeEvent {
	return domain.NativeEvent{
		NativeEventID: partID, ParentNativeEventID: messageID + ":metadata",
		SourcePosition: domain.SourcePosition{Index: int64Pointer(index)}, Timestamp: timestamp,
		TimestampSource: timestampSource(timestamp), TimestampConfidence: timestampConfidence(timestamp),
		Type: domain.EventMetadata, Role: role, NativeType: nativeType, Native: cloneRaw(native),
	}
}

func mapToolPart(base domain.NativeEvent, data map[string]any, dataRaw json.RawMessage) []domain.NativeEvent {
	state, _ := asObject(data["state"])
	status := valueString(state["status"])
	name := firstNonEmpty(valueString(data["tool"]), valueString(data["name"]))
	callID := firstNonEmpty(valueString(data["callID"]), valueString(data["call_id"]), base.NativeEventID)
	input := marshalNullable(state["input"])

	call := base
	call.NativeEventID = base.NativeEventID + ":call"
	call.Type = domain.EventToolCall
	call.Role = domain.RoleAssistant
	call.Tool = &domain.ToolPayload{Name: name, CallID: callID, Input: input, Status: status}
	call.Content = rawBlock("tool_call", dataRaw, visibility(data))
	addCommandPayload(&call, name, state, false)
	if status != "completed" && status != "error" && status != "failed" {
		return []domain.NativeEvent{call}
	}
	// OpenCode stores a completed call and its result in one part. Keep the
	// complete native part on the result event (where the shared result limit is
	// enforced), but do not duplicate an unbounded output onto the call event.
	callProjection := marshalNative(map[string]any{
		"source_part_id": base.NativeEventID,
		"tool_call":      map[string]any{"tool": name, "callID": callID, "status": status, "input": state["input"]},
	})
	call.Native = callProjection
	call.Content = rawBlock("tool_call", callProjection, visibility(data))

	outputValue := state["output"]
	if outputValue == nil {
		outputValue = state["error"]
	}
	result := base
	result.NativeEventID = base.NativeEventID + ":result"
	result.ParentNativeEventID = call.NativeEventID
	result.Type = domain.EventToolResult
	result.Role = domain.RoleTool
	result.Tool = &domain.ToolPayload{Name: name, CallID: callID, Input: input, Output: marshalNullable(outputValue), Status: status}
	result.Content = toolResultContent(outputValue, dataRaw)
	// The output wrapper lets the shared native-result limiter cap every field
	// of a harness-native result, including legacy error/attachment spellings.
	result.Native = marshalNative(map[string]any{"native_tool_result": map[string]any{"output": json.RawMessage(base.Native)}})
	addCommandPayload(&result, name, state, true)
	return []domain.NativeEvent{call, result}
}

func addCommandPayload(event *domain.NativeEvent, name string, state map[string]any, result bool) {
	lower := strings.ToLower(name)
	if !strings.Contains(lower, "bash") && !strings.Contains(lower, "shell") && !strings.Contains(lower, "command") && !strings.Contains(lower, "exec") {
		return
	}
	input, _ := asObject(state["input"])
	command := firstNonEmpty(valueString(input["command"]), valueString(input["cmd"]), valueString(input["text"]))
	workingDirectory := firstNonEmpty(valueString(input["cwd"]), valueString(input["working_directory"]))
	commandPayload := &domain.CommandPayload{Shell: name, Text: command, WorkingDirectory: workingDirectory}
	if metadata, ok := asObject(state["metadata"]); ok {
		if value, ok := integerValue(firstNonNil(metadata["exitCode"], metadata["exit_code"])); ok {
			integer := int(value)
			commandPayload.ExitCode = &integer
		}
	}
	event.Command = commandPayload
	if result {
		event.Type = domain.EventCommandResult
	} else {
		event.Type = domain.EventCommand
	}
}

func patchFile(data map[string]any) *domain.FilePayload {
	path := valueString(data["path"])
	if path == "" {
		if files, ok := data["files"].([]any); ok && len(files) > 0 {
			path = valueString(files[0])
		}
	}
	return &domain.FilePayload{Path: path, Operation: "modify", Diff: valueString(data["diff"])}
}

func modelFrom(data map[string]any) *domain.ModelPayload {
	provider := firstNonEmpty(valueString(data["providerID"]), valueString(data["provider_id"]), valueString(data["provider"]))
	model := firstNonEmpty(valueString(data["modelID"]), valueString(data["model_id"]), valueString(data["model"]))
	if object, ok := asObject(data["model"]); ok {
		provider = firstNonEmpty(provider, valueString(object["providerID"]), valueString(object["provider_id"]), valueString(object["provider"]))
		model = firstNonEmpty(valueString(object["modelID"]), valueString(object["model_id"]), valueString(object["id"]), model)
	}
	if provider == "" && model == "" {
		return nil
	}
	return &domain.ModelPayload{Provider: provider, Name: model}
}

func usageFrom(data map[string]any) *domain.UsagePayload {
	tokens, _ := asObject(data["tokens"])
	if len(tokens) == 0 {
		tokens, _ = asObject(data["usage"])
	}
	cache, _ := asObject(tokens["cache"])
	input, hasInput := integerValue(firstNonNil(tokens["input"], tokens["input_tokens"], data["input_tokens"]))
	output, hasOutput := integerValue(firstNonNil(tokens["output"], tokens["output_tokens"], data["output_tokens"]))
	cacheRead, hasCacheRead := integerValue(firstNonNil(cache["read"], tokens["cache_read"], tokens["cache_read_tokens"], data["cache_read_tokens"]))
	cacheWrite, hasCacheWrite := integerValue(firstNonNil(cache["write"], tokens["cache_write"], tokens["cache_write_tokens"], data["cache_write_tokens"]))
	cost, hasCost := floatValue(data["cost"])
	if !hasInput && !hasOutput && !hasCacheRead && !hasCacheWrite && !hasCost {
		return nil
	}
	usage := &domain.UsagePayload{}
	if hasInput {
		usage.InputTokens = &input
	}
	if hasOutput {
		usage.OutputTokens = &output
	}
	if hasCacheRead {
		usage.CacheReadTokens = &cacheRead
	}
	if hasCacheWrite {
		usage.CacheWriteTokens = &cacheWrite
	}
	if hasCost {
		usage.Cost = &cost
	}
	if currency := valueString(data["currency"]); currency != "" {
		usage.Currency = stringPointer(currency)
	}
	return usage
}

func dataObject(value any) (map[string]any, error) {
	switch item := value.(type) {
	case json.RawMessage:
		var result map[string]any
		decoderErr := json.Unmarshal(item, &result)
		return result, decoderErr
	case []byte:
		var result map[string]any
		decoderErr := json.Unmarshal(item, &result)
		return result, decoderErr
	case string:
		var result map[string]any
		decoderErr := json.Unmarshal([]byte(item), &result)
		return result, decoderErr
	case map[string]any:
		return item, nil
	case nil:
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("unsupported JSON value %T", value)
	}
}

func rowDataObject(row map[string]any) (map[string]any, error) {
	if value, exists := row["data"]; exists {
		return dataObject(value)
	}
	return row, nil
}

func rawObject(value map[string]any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func rawBlock(kind string, data json.RawMessage, visibilityValue string) []domain.ContentBlock {
	return []domain.ContentBlock{{Type: kind, Data: cloneRaw(data), Visibility: stringPointer(visibilityValue)}}
}

func textContent(kind, text string, data json.RawMessage, visibilityValue string) []domain.ContentBlock {
	block := domain.ContentBlock{Type: kind, Data: cloneRaw(data), Visibility: stringPointer(visibilityValue)}
	block.Text = stringPointer(text)
	return []domain.ContentBlock{block}
}

func toolResultContent(output any, data json.RawMessage) []domain.ContentBlock {
	block := domain.ContentBlock{Type: "tool_result", Data: cloneRaw(data), Visibility: stringPointer("native")}
	if text, ok := output.(string); ok {
		block.Text = stringPointer(text)
	}
	return []domain.ContentBlock{block}
}

func visibility(data map[string]any) string {
	return firstNonEmpty(valueString(data["visibility"]), "native")
}

func nestedTime(data map[string]any, paths ...[]string) *time.Time {
	for _, path := range paths {
		var value any = data
		for _, key := range path {
			object, ok := value.(map[string]any)
			if !ok {
				value = nil
				break
			}
			value = object[key]
		}
		if parsed := parseTime(value); parsed != nil {
			return parsed
		}
	}
	return nil
}

func valueAt(data map[string]any, key string) any { return data[key] }

func asObject(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func parseRole(value any) domain.Role {
	switch strings.ToLower(valueString(value)) {
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

func messageType(role domain.Role) domain.EventType {
	switch role {
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

func timestampSource(value *time.Time) domain.TimestampSource {
	if value == nil {
		return domain.TimestampUnknown
	}
	return domain.TimestampNative
}

func timestampConfidence(value *time.Time) domain.TimestampConfidence {
	if value == nil {
		return domain.ConfidenceUnknown
	}
	return domain.ConfidenceExact
}

func marshalNullable(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func cloneRaw(value json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), value...) }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func integerValue(value any) (int64, bool) {
	switch item := value.(type) {
	case int64:
		return item, true
	case int:
		return int64(item), true
	case float64:
		return int64(item), true
	case json.Number:
		result, err := item.Int64()
		return result, err == nil
	default:
		return 0, false
	}
}

func floatValue(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, true
	case int64:
		return float64(item), true
	case json.Number:
		result, err := item.Float64()
		return result, err == nil
	default:
		return 0, false
	}
}

func firstModel(values ...*domain.ModelPayload) *domain.ModelPayload {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstUsage(values ...*domain.UsagePayload) *domain.UsagePayload {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
