package domain

import "fmt"

type WarningSeverity string

const (
	SeverityInfo    WarningSeverity = "info"
	SeverityWarning WarningSeverity = "warning"
	SeverityError   WarningSeverity = "error"
)

type Warning struct {
	ID        string          `json:"id"`
	Code      string          `json:"code"`
	Severity  WarningSeverity `json:"severity"`
	Category  string          `json:"category"`
	Message   string          `json:"message"`
	SessionID *string         `json:"session_id,omitempty"`
	EventID   *string         `json:"event_id,omitempty"`
}

type DiagnosticError struct {
	Code       string `json:"code"`
	Category   string `json:"category"`
	Message    string `json:"message"`
	Harness    string `json:"harness,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	SourceLine int64  `json:"source_line,omitempty"`
}

func (e DiagnosticError) Error() string {
	location := ""
	if e.SourceLine > 0 {
		location = fmt.Sprintf(" at source line %d", e.SourceLine)
	}
	return e.Code + location + ": " + e.Message
}

const (
	ExitSuccess              = 0
	ExitWarnings             = 1
	ExitInvalidConfiguration = 2
	ExitNoHarnesses          = 3
	ExitNoSessions           = 4
	ExitSourceReadFailure    = 5
	ExitOutputFailure        = 6
	ExitStrictFailure        = 7
	ExitCancelled            = 8
)
