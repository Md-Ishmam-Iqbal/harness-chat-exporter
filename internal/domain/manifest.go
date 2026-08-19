package domain

import "time"

type Manifest struct {
	ArchiveFormat      string            `json:"archive_format"`
	ArchiveVersion     string            `json:"archive_version"`
	CommandVersion     string            `json:"command_version"`
	ExportedAt         time.Time         `json:"exported_at"`
	Range              ManifestRange     `json:"range"`
	Timezone           string            `json:"timezone"`
	DisclosurePolicy   string            `json:"disclosure_policy"`
	ReasoningPolicy    string            `json:"reasoning_policy"`
	RedactionEnabled   bool              `json:"redaction_enabled"`
	Options            ManifestOptions   `json:"options"`
	Adapters           []AdapterReport   `json:"adapters"`
	Totals             ManifestTotals    `json:"totals"`
	Exclusions         []ManifestNotice  `json:"exclusions"`
	Deduplications     []ManifestNotice  `json:"deduplications"`
	Errors             []DiagnosticError `json:"errors"`
	Truncations        []Truncation      `json:"truncations"`
	IncompleteCoverage bool              `json:"incomplete_coverage"`
	Files              []ManifestFile    `json:"files"`
}

type ManifestRange struct {
	From  time.Time    `json:"from"`
	To    time.Time    `json:"to"`
	Scope SessionScope `json:"scope"`
}

type ManifestOptions struct {
	Harnesses         []string `json:"harnesses"`
	ResponseLimit     int64    `json:"response_limit_bytes"`
	SessionSoftLimit  int64    `json:"session_soft_limit_bytes"`
	TotalExportLimit  int64    `json:"total_export_limit_bytes"`
	NativeRecordLimit int64    `json:"native_record_limit_bytes"`
	Strict            bool     `json:"strict"`
	Reproducible      bool     `json:"reproducible"`
	CombinedMarkdown  bool     `json:"combined_markdown"`
}

type AdapterReport struct {
	ID       string          `json:"id"`
	Status   DetectionStatus `json:"status"`
	Version  *string         `json:"version"`
	Roots    []string        `json:"roots"`
	Warnings []string        `json:"warning_ids"`
}

type ManifestTotals struct {
	Sources     int64 `json:"sources"`
	Sessions    int64 `json:"sessions"`
	Events      int64 `json:"events"`
	Warnings    int64 `json:"warnings"`
	Errors      int64 `json:"errors"`
	Truncations int64 `json:"truncations"`
}

type ManifestNotice struct {
	Code    string `json:"code"`
	Count   int64  `json:"count"`
	Message string `json:"message"`
}

type ManifestFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}
