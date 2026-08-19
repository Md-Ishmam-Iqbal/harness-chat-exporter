package domain

import (
	"encoding/json"
	"time"
)

const SchemaVersion = "1.0"

type RecordType string

const (
	RecordSession RecordType = "session"
	RecordEvent   RecordType = "event"
)

type TimestampSource string

const (
	TimestampNative     TimestampSource = "native"
	TimestampSession    TimestampSource = "session"
	TimestampFilesystem TimestampSource = "filesystem"
	TimestampInferred   TimestampSource = "inferred"
	TimestampUnknown    TimestampSource = "unknown"
)

type TimestampConfidence string

const (
	ConfidenceExact   TimestampConfidence = "exact"
	ConfidenceHigh    TimestampConfidence = "high"
	ConfidenceMedium  TimestampConfidence = "medium"
	ConfidenceLow     TimestampConfidence = "low"
	ConfidenceUnknown TimestampConfidence = "unknown"
)

type EventType string

const (
	EventUserMessage      EventType = "user_message"
	EventAssistantMessage EventType = "assistant_message"
	EventSystemMessage    EventType = "system_message"
	EventDeveloperMessage EventType = "developer_message"
	EventToolCall         EventType = "tool_call"
	EventToolResult       EventType = "tool_result"
	EventCommand          EventType = "command"
	EventCommandResult    EventType = "command_result"
	EventFileRead         EventType = "file_read"
	EventFileWrite        EventType = "file_write"
	EventFileChange       EventType = "file_change"
	EventApprovalRequest  EventType = "approval_request"
	EventApprovalResponse EventType = "approval_response"
	EventModelChange      EventType = "model_change"
	EventCompaction       EventType = "compaction"
	EventBranch           EventType = "branch"
	EventSubagentStart    EventType = "subagent_start"
	EventSubagentEnd      EventType = "subagent_end"
	EventError            EventType = "error"
	EventMetadata         EventType = "metadata"
	EventUnknown          EventType = "unknown"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
	RoleTool      Role = "tool"
	RoleHarness   Role = "harness"
	RoleUnknown   Role = "unknown"
)

type MatchReason string

const (
	MatchEventInRange MatchReason = "event_in_range"
	MatchSessionTime  MatchReason = "session_timestamp"
	MatchFilesystem   MatchReason = "filesystem_mtime"
	MatchNotMatched   MatchReason = "not_matched"
)

type IntegrityStatus string

const (
	IntegrityComplete  IntegrityStatus = "complete"
	IntegrityPartial   IntegrityStatus = "partial"
	IntegrityTruncated IntegrityStatus = "truncated"
	IntegrityUnknown   IntegrityStatus = "unknown"
)

type SessionRecord struct {
	RecordType      RecordType       `json:"record_type"`
	SchemaVersion   string           `json:"schema_version"`
	ID              string           `json:"id"`
	Harness         string           `json:"harness"`
	HarnessVersion  string           `json:"harness_version"`
	NativeSessionID string           `json:"native_session_id"`
	Title           string           `json:"title"`
	Summary         *string          `json:"summary"`
	Project         Project          `json:"project"`
	ModelUsage      []ModelUsage     `json:"model_usage"`
	StartedAt       *time.Time       `json:"started_at"`
	UpdatedAt       *time.Time       `json:"updated_at"`
	MatchedRange    bool             `json:"matched_range"`
	MatchReason     MatchReason      `json:"match_reason"`
	Source          Source           `json:"source"`
	Structure       SessionStructure `json:"structure"`
	Counts          RecordCounts     `json:"counts"`
	Integrity       Integrity        `json:"integrity"`
}

type Project struct {
	DisplayName              string `json:"display_name"`
	WorkingDirectory         string `json:"working_directory"`
	WorkingDirectoryRedacted bool   `json:"working_directory_redacted"`
	RepositoryRoot           string `json:"repository_root"`
	RepositoryName           string `json:"repository_name"`
	RemoteHost               string `json:"remote_host"`
	RemoteOwner              string `json:"remote_owner"`
	RemoteRepository         string `json:"remote_repository"`
	GitBranch                string `json:"git_branch"`
	GitCommit                string `json:"git_commit"`
	GitMetadataSource        string `json:"git_metadata_source"`
}

type ModelUsage struct {
	Provider    string     `json:"provider"`
	Model       string     `json:"model"`
	FirstSeenAt *time.Time `json:"first_seen_at"`
}

type Source struct {
	Path          string     `json:"path"`
	PathRedacted  bool       `json:"path_redacted"`
	Format        string     `json:"format"`
	FormatVersion string     `json:"format_version"`
	SizeBytes     int64      `json:"size_bytes"`
	ModifiedAt    *time.Time `json:"modified_at"`
}

type SessionStructure struct {
	Linear          bool    `json:"linear"`
	Branched        bool    `json:"branched"`
	ParentSessionID *string `json:"parent_session_id"`
	Subagent        bool    `json:"subagent"`
}

type RecordCounts struct {
	Events            int64 `json:"events"`
	UserMessages      int64 `json:"user_messages"`
	AssistantMessages int64 `json:"assistant_messages"`
	ToolCalls         int64 `json:"tool_calls"`
	ToolResults       int64 `json:"tool_results"`
	FileChanges       int64 `json:"file_changes"`
	FilesReferenced   int64 `json:"files_referenced"`
	UnknownEvents     int64 `json:"unknown_events"`
}

type Integrity struct {
	Status                 IntegrityStatus `json:"status"`
	MalformedNativeRecords int64           `json:"malformed_native_records"`
	TruncatedEvents        int64           `json:"truncated_events"`
	ConcurrentlyModified   bool            `json:"concurrently_modified"`
	Warnings               []Warning       `json:"warnings"`
}

type EventRecord struct {
	RecordType          RecordType          `json:"record_type"`
	SchemaVersion       string              `json:"schema_version"`
	ID                  string              `json:"id"`
	SessionID           string              `json:"session_id"`
	NativeID            string              `json:"native_id"`
	ParentID            *string             `json:"parent_id"`
	Sequence            int64               `json:"sequence"`
	Branch              BranchReference     `json:"branch"`
	Timestamp           *time.Time          `json:"timestamp"`
	TimestampSource     TimestampSource     `json:"timestamp_source"`
	TimestampConfidence TimestampConfidence `json:"timestamp_confidence"`
	Type                EventType           `json:"type"`
	Role                Role                `json:"role"`
	Content             []ContentBlock      `json:"content"`
	Tool                *ToolPayload        `json:"tool"`
	File                *FilePayload        `json:"file"`
	Command             *CommandPayload     `json:"command"`
	Model               *ModelPayload       `json:"model"`
	Usage               *UsagePayload       `json:"usage"`
	Redactions          []Redaction         `json:"redactions"`
	Truncations         []Truncation        `json:"truncations"`
	Source              EventSource         `json:"source"`
	Native              json.RawMessage     `json:"native"`
}

type BranchReference struct {
	BranchID   *string `json:"branch_id"`
	ActivePath *bool   `json:"active_path"`
	ParentID   *string `json:"parent_id,omitempty"`
	Label      *string `json:"label,omitempty"`
}

type ContentBlock struct {
	Type       string          `json:"type"`
	Text       *string         `json:"text"`
	MediaType  *string         `json:"media_type"`
	Data       json.RawMessage `json:"data"`
	Visibility *string         `json:"visibility"`
}

type ToolPayload struct {
	Name   string          `json:"name"`
	CallID string          `json:"call_id"`
	Input  json.RawMessage `json:"input"`
	Output json.RawMessage `json:"output"`
	Status string          `json:"status"`
}

type FilePayload struct {
	Path         string `json:"path"`
	Operation    string `json:"operation"`
	LinesAdded   *int64 `json:"lines_added"`
	LinesRemoved *int64 `json:"lines_removed"`
	Diff         string `json:"diff"`
}

type CommandPayload struct {
	Shell            string `json:"shell"`
	Text             string `json:"text"`
	WorkingDirectory string `json:"working_directory"`
	ExitCode         *int   `json:"exit_code"`
}

type ModelPayload struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

type UsagePayload struct {
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_write_tokens"`
	Cost             *float64 `json:"cost"`
	Currency         *string  `json:"currency"`
}

type Truncation struct {
	FieldPath     string `json:"field_path"`
	OriginalBytes int64  `json:"original_bytes"`
	RetainedBytes int64  `json:"retained_bytes"`
	SHA256        string `json:"sha256,omitempty"`
}

// Redaction is retained only for schema compatibility. This release always
// emits an empty list and performs no content redaction.
type Redaction struct {
	FieldPath string `json:"field_path"`
	Rule      string `json:"rule"`
}

type EventSource struct {
	NativeType string         `json:"native_type"`
	Position   SourcePosition `json:"position"`
}

type SourcePosition struct {
	Line   *int64 `json:"line"`
	Offset *int64 `json:"offset"`
	Index  *int64 `json:"index"`
}
