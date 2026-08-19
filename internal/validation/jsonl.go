package validation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type RecordStats struct {
	Sessions   int64
	Events     int64
	Warnings   int64
	Truncation int64
	Sources    map[string]struct{}
	Records    []domain.SessionRecord
}

func SessionsJSONL(r io.Reader) (RecordStats, error) {
	stats := RecordStats{Sources: map[string]struct{}{}}
	reader := bufio.NewReader(r)
	var current *domain.SessionRecord
	var currentCounts domain.RecordCounts
	currentFiles := map[string]struct{}{}
	var lastSequence int64
	seenSessions := map[string]bool{}
	eventIDs := map[string]bool{}
	var parentIDs []string
	lineNumber := int64(0)
	finish := func() error {
		currentCounts.FilesReferenced = int64(len(currentFiles))
		if current != nil && current.Counts != currentCounts {
			return fmt.Errorf("session %s declared counts do not match its events", current.ID)
		}
		for _, parentID := range parentIDs {
			if !eventIDs[parentID] {
				return fmt.Errorf("session %s references absent parent event %s", current.ID, parentID)
			}
		}
		return nil
	}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(line) == 0 {
				return stats, fmt.Errorf("empty JSONL line %d", lineNumber)
			}
			var header struct {
				RecordType domain.RecordType `json:"record_type"`
			}
			if decodeErr := decodeOne(line, &header); decodeErr != nil {
				return stats, fmt.Errorf("JSONL line %d: %w", lineNumber, decodeErr)
			}
			switch header.RecordType {
			case domain.RecordSession:
				if finishErr := finish(); finishErr != nil {
					return stats, finishErr
				}
				var session domain.SessionRecord
				if decodeErr := decodeOne(line, &session); decodeErr != nil {
					return stats, fmt.Errorf("session line %d: %w", lineNumber, decodeErr)
				}
				if session.SchemaVersion != domain.SchemaVersion || !normalizedID(session.ID, "hh_ses_") || !knownHarness(session.Harness) {
					return stats, fmt.Errorf("session line %d has invalid identity/schema", lineNumber)
				}
				if seenSessions[session.ID] {
					return stats, fmt.Errorf("session line %d duplicates session %s", lineNumber, session.ID)
				}
				seenSessions[session.ID] = true
				current = &session
				currentCounts = domain.RecordCounts{}
				currentFiles = map[string]struct{}{}
				lastSequence = -1
				eventIDs = map[string]bool{}
				parentIDs = nil
				stats.Sessions++
				stats.Warnings += int64(len(session.Integrity.Warnings))
				if session.Source.Path != "" {
					stats.Sources[session.Source.Path] = struct{}{}
				}
				stats.Records = append(stats.Records, session)
			case domain.RecordEvent:
				if current == nil {
					return stats, fmt.Errorf("event line %d has no preceding session", lineNumber)
				}
				var event domain.EventRecord
				if decodeErr := decodeOne(line, &event); decodeErr != nil {
					return stats, fmt.Errorf("event line %d: %w", lineNumber, decodeErr)
				}
				if event.SchemaVersion != domain.SchemaVersion || event.SessionID != current.ID ||
					!normalizedID(event.ID, "hh_evt_") || !knownUsageEventType(event.Type) || len(event.Redactions) != 0 ||
					(len(event.Native) > 0 && !json.Valid(event.Native)) {
					return stats, fmt.Errorf("event line %d is not grouped under session %s", lineNumber, current.ID)
				}
				if event.ParentID != nil && !normalizedID(*event.ParentID, "hh_evt_") {
					return stats, fmt.Errorf("event line %d has invalid parent ID", lineNumber)
				}
				if event.Sequence <= lastSequence {
					return stats, fmt.Errorf("event line %d sequence regressed", lineNumber)
				}
				if event.ID == "" || eventIDs[event.ID] {
					return stats, fmt.Errorf("event line %d has missing or duplicate ID", lineNumber)
				}
				eventIDs[event.ID] = true
				if event.ParentID != nil {
					parentIDs = append(parentIDs, *event.ParentID)
				}
				lastSequence = event.Sequence
				currentCounts.Events++
				if event.File != nil && event.File.Path != "" {
					currentFiles[event.File.Path] = struct{}{}
				}
				switch event.Type {
				case domain.EventUserMessage:
					currentCounts.UserMessages++
				case domain.EventAssistantMessage:
					currentCounts.AssistantMessages++
				case domain.EventToolCall, domain.EventCommand:
					currentCounts.ToolCalls++
				case domain.EventToolResult, domain.EventCommandResult:
					currentCounts.ToolResults++
				case domain.EventFileChange, domain.EventFileWrite:
					currentCounts.FileChanges++
				case domain.EventUnknown:
					currentCounts.UnknownEvents++
				}
				stats.Events++
				stats.Truncation += int64(len(event.Truncations))
			default:
				return stats, fmt.Errorf("JSONL line %d has unknown record_type %q", lineNumber, header.RecordType)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return stats, err
		}
	}
	if err := finish(); err != nil {
		return stats, err
	}
	return stats, nil
}

func normalizedID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	hex := strings.TrimPrefix(value, prefix)
	if len(hex) < 20 || len(hex) > 64 {
		return false
	}
	for _, character := range hex {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return false
		}
	}
	return true
}

func knownHarness(value string) bool {
	switch value {
	case "claude", "codex", "opencode", "pi":
		return true
	default:
		return false
	}
}

func knownEventType(value domain.EventType) bool {
	switch value {
	case domain.EventUserMessage, domain.EventAssistantMessage, domain.EventSystemMessage,
		domain.EventDeveloperMessage, domain.EventToolCall, domain.EventToolResult,
		domain.EventCommand, domain.EventCommandResult, domain.EventFileRead,
		domain.EventFileWrite, domain.EventFileChange, domain.EventApprovalRequest,
		domain.EventApprovalResponse, domain.EventModelChange, domain.EventCompaction,
		domain.EventBranch, domain.EventSubagentStart, domain.EventSubagentEnd,
		domain.EventError, domain.EventMetadata, domain.EventUnknown:
		return true
	default:
		return false
	}
}

func knownUsageEventType(value domain.EventType) bool {
	return value == domain.EventUserMessage || value == domain.EventAssistantMessage
}

func JSONL(r io.Reader) (int64, error) {
	reader := bufio.NewReader(r)
	var count int64
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			count++
			line = bytes.TrimSpace(line)
			if len(line) == 0 || !json.Valid(line) {
				return count, fmt.Errorf("invalid JSON on line %d", count)
			}
		}
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}
	}
}

func decodeOne(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("multiple JSON values")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
