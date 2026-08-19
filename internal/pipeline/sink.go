package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/limits"
)

type SessionOptions struct {
	Reference            domain.SessionReference
	Session              domain.SessionRecord
	Range                domain.TimeRange
	Scope                domain.SessionScope
	MaxResponseBytes     int64
	MaxNativeRecordBytes int64
}

type MalformedRecord struct {
	Raw                 []byte
	SourcePosition      domain.SourcePosition
	Timestamp           *time.Time
	TimestampSource     domain.TimestampSource
	TimestampConfidence domain.TimestampConfidence
	NativeType          string
}

type eventIndex struct {
	nativeID     string
	parentNative string
	eventType    domain.EventType
	inRange      bool
	timestamped  bool
	branchID     string
	branchParent string
	callID       string
	filePath     string
	truncated    bool
}

type SessionSink struct {
	options     SessionOptions
	file        *os.File
	path        string
	closed      bool
	aborted     bool
	sequence    int64
	indexes     []eventIndex
	startedAt   *time.Time
	updatedAt   *time.Time
	matched     bool
	matchReason domain.MatchReason
	malformed   int64
	partial     bool
	warnings    []domain.Warning
	diagnostics []domain.DiagnosticError
	models      map[string]domain.ModelUsage
	nativeIDs   map[string]int
	branched    bool
	pending     *domain.NativeEvent
}

func NewSessionSink(workspace *Workspace, options SessionOptions) (*SessionSink, error) {
	if !options.Range.Valid() {
		return nil, errors.New("invalid export range")
	}
	if options.Scope == "" {
		options.Scope = domain.ScopeTouched
	}
	if options.Scope != domain.ScopeTouched && options.Scope != domain.ScopeEventsOnly {
		return nil, errors.New("invalid session scope")
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = limits.DefaultResponseBytes
	}
	if options.MaxNativeRecordBytes == 0 {
		options.MaxNativeRecordBytes = limits.DefaultNativeRecordBytes
	}
	if options.MaxResponseBytes < 0 || options.MaxNativeRecordBytes < 0 {
		return nil, errors.New("invalid session limits")
	}
	file, path, err := workspace.newSpool()
	if err != nil {
		return nil, err
	}
	models := map[string]domain.ModelUsage{}
	for _, model := range options.Session.ModelUsage {
		models[model.Provider+"\x00"+model.Model] = model
	}
	return &SessionSink{options: options, file: file, path: path, matchReason: domain.MatchNotMatched, models: models, nativeIDs: map[string]int{}}, nil
}

func (s *SessionSink) NativeSink() domain.NativeEventSink { return s.Add }

func (s *SessionSink) Add(ctx context.Context, candidate domain.NativeEvent) error {
	return s.add(ctx, candidate, false)
}

func (s *SessionSink) AddMalformed(ctx context.Context, malformed MalformedRecord) error {
	if int64(len(malformed.Raw)) > s.options.MaxNativeRecordBytes {
		s.recordSizeLimit(malformed.SourcePosition)
		return nil
	}
	s.malformed++
	s.partial = true
	event := domain.NativeEvent{
		SourcePosition: malformed.SourcePosition, Timestamp: malformed.Timestamp,
		TimestampSource:     malformed.TimestampSource,
		TimestampConfidence: malformed.TimestampConfidence, Type: domain.EventUnknown,
		Role: domain.RoleUnknown, NativeType: malformed.NativeType,
		Native: limits.EncodeMalformedRecord(malformed.Raw),
	}
	return s.add(ctx, event, true)
}

func (s *SessionSink) add(ctx context.Context, candidate domain.NativeEvent, alreadyMalformed bool) error {
	if err := ctx.Err(); err != nil {
		_ = s.Abort()
		return err
	}
	if s.closed || s.aborted {
		return errors.New("session spool is closed")
	}
	if alreadyMalformed {
		return nil
	}
	var keep bool
	candidate, keep = usageEvent(candidate)
	if !keep {
		return nil
	}
	if candidate.Type == domain.EventAssistantMessage {
		s.mergeAssistant(candidate)
		return nil
	}
	if err := s.flushAssistant(); err != nil {
		return err
	}
	return s.persist(candidate)
}

func (s *SessionSink) persist(candidate domain.NativeEvent) error {
	if int64(len(candidate.Native)) > s.options.MaxNativeRecordBytes {
		s.recordSizeLimit(candidate.SourcePosition)
		return nil
	}
	if len(candidate.Native) > 0 && !json.Valid(candidate.Native) {
		candidate.Native = limits.EncodeMalformedRecord(candidate.Native)
		candidate.Type = domain.EventUnknown
		candidate.Role = domain.RoleUnknown
		s.malformed++
		s.partial = true
	}

	record, truncations, err := s.normalize(candidate)
	if err != nil {
		_ = s.Abort()
		return err
	}
	line, err := json.Marshal(record)
	if err != nil {
		_ = s.Abort()
		return errors.New("encode normalized event")
	}
	line = append(line, '\n')
	if err := writeAll(s.file, line); err != nil {
		_ = s.Abort()
		return errors.New("write session spool")
	}
	if candidate.NativeEventID != "" {
		s.nativeIDs[candidate.NativeEventID]++
	}

	inRange := record.Timestamp != nil && s.options.Range.Contains(*record.Timestamp)
	index := eventIndex{nativeID: candidate.NativeEventID,
		parentNative: candidate.ParentNativeEventID, eventType: record.Type,
		inRange: inRange, timestamped: record.Timestamp != nil}
	if record.Branch.BranchID != nil {
		index.branchID = *record.Branch.BranchID
	}
	if record.Branch.ParentID != nil {
		index.branchParent = *record.Branch.ParentID
	}
	if record.Tool != nil {
		index.callID = record.Tool.CallID
	}
	if record.File != nil {
		index.filePath = record.File.Path
	}
	index.truncated = len(truncations) > 0
	s.indexes = append(s.indexes, index)
	s.sequence++
	if len(candidate.Warnings) > 0 {
		s.warnings = append(s.warnings, normalizedWarnings(candidate.Warnings, len(s.warnings))...)
	}
	s.observeTimestamp(record.Timestamp)
	if record.Branch.BranchID != nil {
		s.branched = true
	}
	if record.Model != nil && (record.Model.Provider != "" || record.Model.Name != "") {
		key := record.Model.Provider + "\x00" + record.Model.Name
		if _, exists := s.models[key]; !exists {
			s.models[key] = domain.ModelUsage{Provider: record.Model.Provider, Model: record.Model.Name, FirstSeenAt: record.Timestamp}
		}
	}
	if inRange && meaningful(record.Type) {
		s.matched = true
		s.matchReason = domain.MatchEventInRange
		if record.TimestampSource == domain.TimestampFilesystem {
			s.matchReason = domain.MatchFilesystem
		}
	}
	return nil
}

func (s *SessionSink) mergeAssistant(candidate domain.NativeEvent) {
	if s.pending == nil {
		pending := candidate
		s.pending = &pending
		return
	}
	left := *s.pending.Content[0].Text
	right := *candidate.Content[0].Text
	joined := left + "\n\n" + right
	s.pending.Content[0].Text = &joined
	if candidate.Timestamp != nil {
		s.pending.Timestamp = candidate.Timestamp
		s.pending.TimestampSource = candidate.TimestampSource
		s.pending.TimestampConfidence = candidate.TimestampConfidence
	}
	s.pending.Warnings = append(s.pending.Warnings, candidate.Warnings...)
}

func (s *SessionSink) flushAssistant() error {
	if s.pending == nil {
		return nil
	}
	pending := *s.pending
	s.pending = nil
	return s.persist(pending)
}

func (s *SessionSink) normalize(candidate domain.NativeEvent) (domain.EventRecord, []domain.Truncation, error) {
	sessionID := s.sessionID()
	identity := candidate.NativeEventID
	if identity == "" {
		identity = "source\x00" + sourceIdentity(candidate.SourcePosition, s.sequence)
	} else if s.nativeIDs[identity] > 0 {
		identity += "\x00" + sourceIdentity(candidate.SourcePosition, s.sequence)
	}
	digest := domain.EventDigest(sessionID, identity)
	eventID := "hh_evt_" + digest[:20]

	content := append([]domain.ContentBlock(nil), candidate.Content...)
	truncations := []domain.Truncation{}
	for index := range content {
		if content[index].Data != nil && !json.Valid(content[index].Data) {
			content[index].Data = limits.EncodeMalformedRecord(content[index].Data)
		}
	}
	if candidate.Type == domain.EventAssistantMessage {
		for index := range content {
			if content[index].Text != nil {
				capped, err := limits.TruncateUTF8(*content[index].Text, s.options.MaxResponseBytes)
				if err != nil {
					return domain.EventRecord{}, nil, errors.New("apply response limit")
				}
				if capped.Truncated {
					content[index].Text = &capped.Text
					truncations = append(truncations, truncation(fmt.Sprintf("content[%d].text", index), capped))
				}
			}
		}
	}

	branch := domain.BranchReference{}
	if candidate.Branch != nil {
		branch = *candidate.Branch
	}
	var parentID *string
	if candidate.ParentNativeEventID != "" {
		value := "hh_evt_" + domain.EventDigest(sessionID, candidate.ParentNativeEventID)[:20]
		parentID = &value
	}
	if content == nil {
		content = []domain.ContentBlock{}
	}
	return domain.EventRecord{
		RecordType: domain.RecordEvent, SchemaVersion: domain.SchemaVersion,
		ID: eventID, SessionID: sessionID, NativeID: candidate.NativeEventID,
		ParentID: parentID, Sequence: s.sequence, Branch: branch,
		Timestamp: utcTime(candidate.Timestamp), TimestampSource: candidate.TimestampSource,
		TimestampConfidence: candidate.TimestampConfidence, Type: candidate.Type,
		Role: candidate.Role, Content: content, Tool: nil, File: nil,
		Command: nil, Model: nil, Usage: nil,
		Redactions: []domain.Redaction{}, Truncations: truncations,
		Source: domain.EventSource{NativeType: candidate.NativeType, Position: candidate.SourcePosition},
		Native: json.RawMessage("null"),
	}, truncations, nil
}

func usageEvent(candidate domain.NativeEvent) (domain.NativeEvent, bool) {
	if candidate.Type != domain.EventUserMessage && candidate.Type != domain.EventAssistantMessage {
		return domain.NativeEvent{}, false
	}
	if candidate.Type == domain.EventUserMessage && candidate.Role != domain.RoleUser {
		return domain.NativeEvent{}, false
	}
	if candidate.Type == domain.EventAssistantMessage && candidate.Role != domain.RoleAssistant {
		return domain.NativeEvent{}, false
	}
	parts := make([]string, 0, len(candidate.Content))
	for _, block := range candidate.Content {
		kind := strings.ToLower(strings.TrimSpace(block.Type))
		media := ""
		if block.MediaType != nil {
			media = strings.ToLower(*block.MediaType)
		}
		if strings.Contains(kind, "image") || strings.HasPrefix(media, "image/") {
			parts = append(parts, "[Image attached]")
			continue
		}
		if strings.Contains(kind, "audio") || strings.HasPrefix(media, "audio/") {
			parts = append(parts, "[Audio attached]")
			continue
		}
		if kind == "reasoning" || kind == "thinking" || kind == "redacted_thinking" {
			continue
		}
		if block.Text == nil {
			continue
		}
		text := strings.TrimSpace(*block.Text)
		if text == "" || harnessWrapper(text) {
			continue
		}
		parts = append(parts, text)
	}
	if len(parts) == 0 {
		return domain.NativeEvent{}, false
	}
	text := strings.Join(parts, "\n\n")
	candidate.Content = []domain.ContentBlock{{Type: "text", Text: &text}}
	candidate.Native = nil
	candidate.NativeType = ""
	candidate.Tool = nil
	candidate.File = nil
	candidate.Command = nil
	candidate.Model = nil
	candidate.Usage = nil
	candidate.Branch = nil
	candidate.ParentNativeEventID = ""
	return candidate, true
}

func harnessWrapper(text string) bool {
	if strings.HasPrefix(text, "<environment_context>") && strings.HasSuffix(text, "</environment_context>") {
		return true
	}
	if strings.HasPrefix(text, "<image ") && strings.HasSuffix(text, ">") {
		return true
	}
	return text == "</image>"
}

func (s *SessionSink) recordSizeLimit(position domain.SourcePosition) {
	s.partial = true
	line := sourceLine(position)
	s.diagnostics = append(s.diagnostics, domain.DiagnosticError{
		Code: "size_limit", Category: "limit", Message: "native record exceeded the configured safety ceiling", SourceLine: line,
	})
	s.warnings = append(s.warnings, domain.Warning{
		ID: fmt.Sprintf("size_limit_%d", len(s.warnings)+1), Code: "size_limit",
		Severity: domain.SeverityError, Category: "limit",
		Message: "a native record exceeded the configured safety ceiling and was skipped",
	})
}

func (s *SessionSink) observeTimestamp(value *time.Time) {
	if value == nil {
		return
	}
	value = utcTime(value)
	if s.startedAt == nil || value.Before(*s.startedAt) {
		t := *value
		s.startedAt = &t
	}
	if s.updatedAt == nil || value.After(*s.updatedAt) {
		t := *value
		s.updatedAt = &t
	}
}

func (s *SessionSink) sessionID() string {
	digest := domain.SessionReferenceDigest(s.options.Reference)
	return "hh_ses_" + digest[:20]
}

func (s *SessionSink) Abort() error {
	if s == nil || s.aborted {
		return nil
	}
	s.aborted = true
	s.closed = true
	if s.file != nil {
		_ = s.file.Close()
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return errors.New("remove session spool")
	}
	return nil
}

func writeAll(writer io.Writer, value []byte) error {
	for len(value) > 0 {
		n, err := writer.Write(value)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		value = value[n:]
	}
	return nil
}

func truncation(path string, value limits.TruncatedText) domain.Truncation {
	return domain.Truncation{FieldPath: path, OriginalBytes: value.OriginalBytes, RetainedBytes: value.RetainedBytes, SHA256: value.SHA256}
}

func meaningful(value domain.EventType) bool {
	return value == domain.EventUserMessage || value == domain.EventAssistantMessage
}

func sourceIdentity(position domain.SourcePosition, sequence int64) string {
	if position.Line != nil {
		return "line:" + strconv.FormatInt(*position.Line, 10)
	}
	if position.Offset != nil {
		return "offset:" + strconv.FormatInt(*position.Offset, 10)
	}
	if position.Index != nil {
		return "index:" + strconv.FormatInt(*position.Index, 10)
	}
	return "sequence:" + strconv.FormatInt(sequence, 10)
}

func sourceLine(position domain.SourcePosition) int64 {
	if position.Line != nil {
		return *position.Line
	}
	return 0
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func normalizedWarnings(values []domain.Warning, offset int) []domain.Warning {
	result := make([]domain.Warning, 0, len(values))
	for index, value := range values {
		severity := value.Severity
		if severity != domain.SeverityInfo && severity != domain.SeverityWarning && severity != domain.SeverityError {
			severity = domain.SeverityWarning
		}
		code := value.Code
		if code == "" {
			code = "adapter_warning"
		}
		category := value.Category
		if category == "" {
			category = "parse"
		}
		message := value.Message
		if message == "" {
			message = "adapter reported a source warning"
		}
		baseID := value.ID
		if baseID == "" {
			baseID = code
		}
		value.ID = fmt.Sprintf("%s_%d", baseID, offset+index+1)
		value.Code = code
		value.Severity = severity
		value.Category = category
		value.Message = message
		result = append(result, value)
	}
	return result
}
