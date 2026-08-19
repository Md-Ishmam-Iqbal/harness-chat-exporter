package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type FinalizedSession struct {
	Session     domain.SessionRecord
	Included    bool
	path        string
	selected    []bool
	diagnostics []domain.DiagnosticError
	closed      bool
}

func (s *SessionSink) Finalize(ctx context.Context, parsed domain.ParseResult) (*FinalizedSession, error) {
	if err := ctx.Err(); err != nil {
		_ = s.Abort()
		return nil, err
	}
	if s.closed || s.aborted {
		return nil, errors.New("session spool is closed")
	}
	if err := s.flushAssistant(); err != nil {
		_ = s.Abort()
		return nil, err
	}
	if err := s.file.Sync(); err != nil {
		_ = s.Abort()
		return nil, errors.New("sync session spool")
	}
	if err := s.file.Close(); err != nil {
		_ = s.Abort()
		return nil, errors.New("close session spool")
	}
	s.closed = true

	matched := s.matched
	matchReason := s.matchReason
	if !matched && !hasTimestampedMeaningful(s.indexes) {
		matched, matchReason = candidateMatch(s.options.Reference, s.options.Range)
	}
	selected := s.selection(matched)
	record := s.sessionRecord(parsed, matched, matchReason, selected)
	return &FinalizedSession{
		Session: record, Included: matched, path: s.path, selected: selected,
		diagnostics: append([]domain.DiagnosticError(nil), s.diagnostics...),
	}, nil
}

func (s *SessionSink) selection(matched bool) []bool {
	selected := make([]bool, len(s.indexes))
	if !matched {
		return selected
	}
	if s.options.Scope == domain.ScopeTouched {
		for index := range s.indexes {
			selected[index] = true
		}
		return selected
	}

	for index, item := range s.indexes {
		selected[index] = item.inRange
	}
	return selected
}

func (s *SessionSink) sessionRecord(parsed domain.ParseResult, matched bool, reason domain.MatchReason, selected []bool) domain.SessionRecord {
	record := s.options.Session
	reference := s.options.Reference
	record.RecordType = domain.RecordSession
	record.SchemaVersion = domain.SchemaVersion
	record.ID = s.sessionID()
	if record.Harness == "" {
		record.Harness = reference.HarnessID
	}
	if record.NativeSessionID == "" {
		record.NativeSessionID = reference.NativeSessionID
	}
	if record.Source.Path == "" {
		record.Source.Path = reference.DisplayPath
	}
	record.Source.PathRedacted = false
	if record.Source.Format == "" {
		record.Source.Format = reference.SourceKind
	}
	if record.Source.FormatVersion == "" {
		record.Source.FormatVersion = reference.SourceVersion
	}
	if record.Source.SizeBytes == 0 {
		record.Source.SizeBytes = reference.SizeBytes
	}
	record.MatchedRange = matched
	record.MatchReason = reason
	if s.startedAt != nil {
		record.StartedAt = utcTime(s.startedAt)
	} else if record.StartedAt == nil {
		record.StartedAt = utcTime(reference.CandidateStartedAt)
	}
	if s.updatedAt != nil {
		record.UpdatedAt = utcTime(s.updatedAt)
	} else if record.UpdatedAt == nil {
		record.UpdatedAt = utcTime(reference.CandidateUpdatedAt)
	}
	record.Structure.Branched = record.Structure.Branched || s.branched
	if !record.Structure.Branched && !record.Structure.Subagent {
		record.Structure.Linear = true
	}
	record.Counts = countSelected(s.indexes, selected)
	record.ModelUsage = sortedModels(s.models)
	malformed := s.malformed
	if parsed.Malformed > malformed {
		malformed = parsed.Malformed
	}
	if malformed > 0 {
		s.diagnostics = append(s.diagnostics, domain.DiagnosticError{
			Code: "malformed_record", Category: "parse",
			Message: "one or more native records could not be parsed; valid records were retained",
		})
	}
	warnings := append([]domain.Warning{}, record.Integrity.Warnings...)
	warnings = append(warnings, s.warnings...)
	warnings = append(warnings, normalizedWarnings(parsed.Warnings, len(warnings))...)
	if parsed.Err != nil {
		s.diagnostics = append(s.diagnostics, domain.DiagnosticError{
			Code: "parse_error", Category: "parse", Message: "adapter reported a source parse error", SourceLine: parsed.Err.SourceLine,
		})
	}
	partial := s.partial || parsed.Partial || parsed.SourceChanged || parsed.Err != nil || malformed > 0
	status := domain.IntegrityComplete
	if partial {
		status = domain.IntegrityPartial
	} else if selectedTruncations(s.indexes, selected) > 0 {
		status = domain.IntegrityTruncated
	}
	record.Integrity = domain.Integrity{
		Status: status, MalformedNativeRecords: malformed,
		TruncatedEvents:      selectedTruncations(s.indexes, selected),
		ConcurrentlyModified: parsed.SourceChanged, Warnings: warnings,
	}
	return record
}

func (f *FinalizedSession) Diagnostics() []domain.DiagnosticError {
	return append([]domain.DiagnosticError(nil), f.diagnostics...)
}

func (f *FinalizedSession) SpoolPath() string { return f.path }

func (f *FinalizedSession) Replay(ctx context.Context, emit func(domain.EventRecord) error) error {
	if f == nil || f.closed {
		return errors.New("session result is closed")
	}
	file, err := os.Open(f.path)
	if err != nil {
		return errors.New("open session spool")
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	decoder := json.NewDecoder(reader)
	index := 0
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return err
		}
		var event domain.EventRecord
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil || index >= len(f.selected) {
			_ = f.Close()
			return errors.New("decode session spool")
		}
		if f.Included && f.selected[index] {
			if err := emit(event); err != nil {
				_ = f.Close()
				return errors.New("emit normalized event")
			}
		}
		index++
	}
	if index != len(f.selected) {
		_ = f.Close()
		return errors.New("incomplete session spool")
	}
	return nil
}

func (f *FinalizedSession) WriteJSONL(ctx context.Context, writer io.Writer) error {
	if !f.Included {
		return nil
	}
	line, err := json.Marshal(f.Session)
	if err != nil {
		return errors.New("encode normalized session")
	}
	if err := writeAll(writer, append(line, '\n')); err != nil {
		return errors.New("write normalized session")
	}
	return f.Replay(ctx, func(event domain.EventRecord) error {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		return writeAll(writer, append(line, '\n'))
	})
}

func (f *FinalizedSession) Close() error {
	if f == nil || f.closed {
		return nil
	}
	f.closed = true
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return errors.New("remove session spool")
	}
	return nil
}

func countSelected(indexes []eventIndex, selected []bool) domain.RecordCounts {
	counts := domain.RecordCounts{}
	files := map[string]struct{}{}
	for index, item := range indexes {
		if index >= len(selected) || !selected[index] {
			continue
		}
		counts.Events++
		if item.filePath != "" {
			files[item.filePath] = struct{}{}
		}
		switch item.eventType {
		case domain.EventUserMessage:
			counts.UserMessages++
		case domain.EventAssistantMessage:
			counts.AssistantMessages++
		case domain.EventToolCall, domain.EventCommand:
			counts.ToolCalls++
		case domain.EventToolResult, domain.EventCommandResult:
			counts.ToolResults++
		case domain.EventFileChange, domain.EventFileWrite:
			counts.FileChanges++
		case domain.EventUnknown:
			counts.UnknownEvents++
		}
	}
	counts.FilesReferenced = int64(len(files))
	return counts
}

func selectedTruncations(indexes []eventIndex, selected []bool) int64 {
	var result int64
	for index, item := range indexes {
		if index < len(selected) && selected[index] && item.truncated {
			result++
		}
	}
	return result
}

func sortedModels(models map[string]domain.ModelUsage) []domain.ModelUsage {
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]domain.ModelUsage, 0, len(keys))
	for _, key := range keys {
		result = append(result, models[key])
	}
	return result
}

func hasTimestampedMeaningful(indexes []eventIndex) bool {
	for _, item := range indexes {
		if meaningful(item.eventType) && item.timestamped {
			return true
		}
	}
	return false
}

func candidateMatch(reference domain.SessionReference, interval domain.TimeRange) (bool, domain.MatchReason) {
	start, end := reference.CandidateStartedAt, reference.CandidateUpdatedAt
	if start == nil {
		start = end
	}
	if end == nil {
		end = start
	}
	if start == nil || end == nil {
		return false, domain.MatchNotMatched
	}
	if end.Before(interval.From) || start.After(interval.To) {
		return false, domain.MatchNotMatched
	}
	if reference.Metadata["updated_at_source"] == "filesystem" {
		return true, domain.MatchFilesystem
	}
	return true, domain.MatchSessionTime
}
