package domain

import (
	"context"
	"encoding/json"
	"time"
)

type Adapter interface {
	ID() string
	DisplayName() string
	Detect(context.Context, Environment) DetectionResult
	Discover(context.Context, DiscoveryOptions, func(SessionReference) error) error
	Probe(context.Context, SessionReference) ProbeResult
	Parse(context.Context, SessionReference, NativeEventSink) ParseResult
}

type Environment struct {
	HomeDir        string
	Variables      map[string]string
	CommandStarted time.Time
	LocalTimezone  string
}

type DetectionStatus string

const (
	DetectionNotDetected DetectionStatus = "not_detected"
	DetectionDetected    DetectionStatus = "detected"
	DetectionDegraded    DetectionStatus = "degraded"
)

type DetectionResult struct {
	Status   DetectionStatus `json:"status"`
	Roots    []DetectedRoot  `json:"roots,omitempty"`
	Version  string          `json:"version,omitempty"`
	Warnings []Warning       `json:"warnings,omitempty"`
}

type DetectedRoot struct {
	Path       string `json:"path"`
	Canonical  string `json:"canonical"`
	Origin     string `json:"origin"`
	Precedence int    `json:"precedence"`
	Readable   bool   `json:"readable"`
}

type DiscoveryOptions struct {
	Roots []DetectedRoot
}

type SessionReference struct {
	HarnessID               string
	CanonicalSourceRoot     string
	CanonicalSourceIdentity string
	DisplayPath             string
	NativeSessionID         string
	SizeBytes               int64
	SourceModifiedAt        *time.Time
	CandidateStartedAt      *time.Time
	CandidateUpdatedAt      *time.Time
	SourceKind              string
	SourceVersion           string
	NativeRecordLimit       int64
	Metadata                map[string]string
}

type ProbeResult struct {
	Supported       bool
	NativeSessionID string
	SourceKind      string
	SourceVersion   string
	StartedAt       *time.Time
	UpdatedAt       *time.Time
	Metadata        map[string]string
	Warnings        []Warning
	Err             *DiagnosticError
}

type NativeEvent struct {
	NativeEventID       string
	SourcePosition      SourcePosition
	Timestamp           *time.Time
	TimestampSource     TimestampSource
	TimestampConfidence TimestampConfidence
	Type                EventType
	Role                Role
	Branch              *BranchReference
	ParentNativeEventID string
	Content             []ContentBlock
	Tool                *ToolPayload
	File                *FilePayload
	Command             *CommandPayload
	Model               *ModelPayload
	Usage               *UsagePayload
	NativeType          string
	Native              json.RawMessage
	Warnings            []Warning
}

type NativeEventSink func(context.Context, NativeEvent) error

type ParseResult struct {
	Malformed     int64
	Partial       bool
	SourceChanged bool
	Warnings      []Warning
	Err           *DiagnosticError
}
