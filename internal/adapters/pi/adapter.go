package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

const (
	defaultRecordLimit = int64(64 << 20)
	probeRecordLimit   = int64(4 << 20)
)

type Adapter struct {
	ExplicitRoots []string
	afterIndex    func() // deterministic concurrent-append injection in package tests
}

func New(explicitRoots ...string) *Adapter {
	return &Adapter{ExplicitRoots: append([]string(nil), explicitRoots...)}
}

func (*Adapter) ID() string          { return "pi" }
func (*Adapter) DisplayName() string { return "Pi Coding Agent" }

func (a *Adapter) Detect(ctx context.Context, env domain.Environment) domain.DetectionResult {
	result := domain.DetectionResult{Status: domain.DetectionNotDetected}
	candidates, settingsWarnings := a.candidateRoots(env)
	result.Warnings = append(result.Warnings, settingsWarnings...)
	seen := map[string]bool{}
	for index, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			result.Status = domain.DetectionDegraded
			result.Warnings = append(result.Warnings, warning("pi_detection_cancelled", "cancelled", 0, "detection was cancelled"))
			return result
		}
		if candidate.origin == "default" {
			if info, err := os.Lstat(candidate.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		info, err := os.Stat(candidate.path)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(candidate.path)
		if err != nil {
			continue
		}
		canonical = filepath.Clean(canonical)
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		result.Roots = append(result.Roots, domain.DetectedRoot{
			Path: candidate.path, Canonical: canonical, Origin: candidate.origin,
			Precedence: index, Readable: true,
		})
	}
	if len(result.Roots) > 0 {
		result.Status = domain.DetectionDetected
	}
	if len(settingsWarnings) > 0 && result.Status == domain.DetectionDetected {
		result.Status = domain.DetectionDegraded
	}
	return result
}

func (*Adapter) Discover(ctx context.Context, options domain.DiscoveryOptions, emit func(domain.SessionReference) error) error {
	for _, root := range options.Roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(root.Canonical)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if info.Mode().IsRegular() {
			if strings.EqualFold(filepath.Ext(root.Canonical), ".jsonl") {
				if err := emitReference(ctx, root, root.Canonical, info, emit); err != nil {
					return err
				}
			}
			continue
		}
		err = filepath.WalkDir(root.Canonical, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			return emitReference(ctx, root, path, info, emit)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func emitReference(ctx context.Context, root domain.DetectedRoot, path string, info fs.FileInfo, emit func(domain.SessionReference) error) error {
	header, _ := readHeader(ctx, path, probeRecordLimit)
	modified := info.ModTime()
	nativeID := header.ID
	if nativeID == "" {
		nativeID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	metadata := map[string]string{"updated_at_source": "filesystem"}
	if header.CWD != "" {
		metadata["cwd"] = header.CWD
		metadata["working_directory"] = header.CWD
	}
	if header.ParentSession != "" {
		metadata["parent_session"] = header.ParentSession
		metadata["parent_session_id"] = header.ParentSession
	}
	if header.LeafID != "" {
		metadata["leaf_id"] = header.LeafID
	}
	return emit(domain.SessionReference{
		HarnessID: "pi", CanonicalSourceRoot: root.Canonical,
		CanonicalSourceIdentity: path, DisplayPath: path,
		NativeSessionID: nativeID, SizeBytes: info.Size(),
		SourceModifiedAt: &modified, CandidateStartedAt: parseTimestamp(header.Timestamp), CandidateUpdatedAt: &modified,
		SourceKind: "session_jsonl", SourceVersion: versionText(header.Version), Metadata: metadata,
	})
}

func (*Adapter) Probe(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	header, err := readHeader(ctx, reference.CanonicalSourceIdentity, probeRecordLimit)
	if err != nil {
		return domain.ProbeResult{NativeSessionID: reference.NativeSessionID,
			Err: &domain.DiagnosticError{Code: "pi_probe_error", Category: "unsupported_format", Message: err.Error(), Harness: "pi"}}
	}
	result := domain.ProbeResult{
		Supported:       header.Type == "session",
		NativeSessionID: firstNonEmpty(header.ID, reference.NativeSessionID),
		SourceKind:      "session_jsonl", SourceVersion: versionText(header.Version),
		StartedAt: parseTimestamp(header.Timestamp), UpdatedAt: reference.CandidateUpdatedAt,
		Metadata: map[string]string{"working_directory": header.CWD,
			"parent_session_id": header.ParentSession},
	}
	if header.Version < 1 || header.Version > 3 {
		result.Warnings = append(result.Warnings, warning("pi_unsupported_version", "unsupported_format", 1, "Pi session version is not supported; records will be retained as unknown"))
	}
	return result
}

func (a *Adapter) Parse(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	before, err := os.Stat(reference.CanonicalSourceIdentity)
	if err != nil {
		return parseError("read_error", err)
	}
	file, err := os.Open(reference.CanonicalSourceIdentity)
	if err != nil {
		return parseError("read_error", err)
	}
	defer file.Close()

	recordLimit := reference.NativeRecordLimit
	if recordLimit <= 0 {
		recordLimit = defaultRecordLimit
	}
	index, result := buildIndex(ctx, file, recordLimit)
	if result.Err != nil {
		return result
	}
	if a.afterIndex != nil {
		a.afterIndex()
	}
	graph := analyzeGraph(index)
	result.Partial = result.Partial || graph.partial
	result.Warnings = append(result.Warnings, graph.warnings...)

	for _, recordIndex := range graph.order {
		if err := ctx.Err(); err != nil {
			return parseError("cancelled", err)
		}
		record := index.records[recordIndex]
		if record.tooLarge {
			continue
		}
		raw, err := readIndexedRecord(file, record, recordLimit)
		if err != nil {
			return parseError("read_error", err)
		}
		var events []domain.NativeEvent
		if record.malformed {
			events = []domain.NativeEvent{malformedEvent(raw, record.line)}
		} else if record.header {
			events = []domain.NativeEvent{headerEvent(raw, index.header, record.line)}
		} else if index.header.Version < 1 || index.header.Version > 3 {
			events = []domain.NativeEvent{unknownEvent(raw, record.node, record.line, "unsupported_version")}
		} else {
			events, err = mapEntry(raw, record.node, record.line, index.header.Version, graph.branch[recordIndex])
			if err != nil {
				result.Malformed++
				result.Partial = true
				result.Warnings = append(result.Warnings, warning("pi_malformed_record", "malformed_record", record.line, "native record could not be decoded"))
				events = []domain.NativeEvent{malformedEvent(raw, record.line)}
			}
		}
		for _, event := range events {
			if err := sink(ctx, event); err != nil {
				return parseError("normalization_error", err)
			}
		}
	}

	after, statErr := os.Stat(reference.CanonicalSourceIdentity)
	if statErr == nil && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())) {
		result.SourceChanged = true
		result.Partial = true
		result.Warnings = append(result.Warnings, warning("pi_concurrent_change", "integrity_warning", 0, "session changed during export"))
	}
	return result
}

type rootCandidate struct{ path, origin string }

func (a *Adapter) candidateRoots(env domain.Environment) ([]rootCandidate, []domain.Warning) {
	var candidates []rootCandidate
	for _, path := range a.ExplicitRoots {
		if path != "" {
			candidates = append(candidates, rootCandidate{resolveUserPath(path, env.HomeDir, ""), "explicit"})
		}
	}
	agentDir := env.Variables["PI_CODING_AGENT_DIR"]
	if agentDir == "" && env.HomeDir != "" {
		agentDir = filepath.Join(env.HomeDir, ".pi", "agent")
	} else {
		agentDir = resolveUserPath(agentDir, env.HomeDir, "")
	}
	if value := env.Variables["PI_CODING_AGENT_SESSION_DIR"]; value != "" {
		candidates = append(candidates, rootCandidate{resolveUserPath(value, env.HomeDir, agentDir), "PI_CODING_AGENT_SESSION_DIR"})
	}
	var warnings []domain.Warning
	if agentDir != "" {
		settingsPath := filepath.Join(agentDir, "settings.json")
		data, err := os.ReadFile(settingsPath)
		if err == nil {
			var settings struct {
				SessionDir string `json:"sessionDir"`
			}
			if json.Unmarshal(data, &settings) != nil {
				warnings = append(warnings, warning("pi_settings_invalid", "configuration", 0, "Pi settings.json could not be decoded"))
			} else if settings.SessionDir != "" {
				candidates = append(candidates, rootCandidate{resolveUserPath(settings.SessionDir, env.HomeDir, agentDir), "settings.json"})
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			warnings = append(warnings, warning("pi_settings_unreadable", "configuration", 0, "Pi settings.json could not be read"))
		}
		candidates = append(candidates, rootCandidate{filepath.Join(agentDir, "sessions"), "default"})
	}
	for i := range candidates {
		absolute, err := filepath.Abs(candidates[i].path)
		if err == nil {
			candidates[i].path = filepath.Clean(absolute)
		}
	}
	return candidates, warnings
}

func resolveUserPath(value, home, relativeBase string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) && relativeBase != "" {
		return filepath.Join(relativeBase, value)
	}
	return value
}

type sessionHeader struct {
	Type          string `json:"type"`
	Version       int    `json:"version"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession"`
	LeafID        string `json:"leafId"`
	CurrentLeafID string `json:"currentLeafId"`
}

func readHeader(ctx context.Context, path string, limit int64) (sessionHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return sessionHeader{}, err
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return sessionHeader{}, err
	}
	raw, tooLarge, _, readErr := readRecord(bufio.NewReaderSize(file, 64<<10), limit)
	if tooLarge {
		return sessionHeader{}, errors.New("pi session header exceeds probe limit")
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return sessionHeader{}, readErr
	}
	var header sessionHeader
	if err := json.Unmarshal(raw, &header); err != nil || header.Type != "session" {
		return sessionHeader{}, errors.New("first native record is not a Pi session header")
	}
	if header.Version == 0 {
		header.Version = 1
	}
	if header.LeafID == "" {
		header.LeafID = header.CurrentLeafID
	}
	return header, nil
}

func readRecord(reader *bufio.Reader, limit int64) ([]byte, bool, int64, error) {
	var data []byte
	var consumed int64
	tooLarge := false
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		if !tooLarge && consumed <= limit {
			data = append(data, fragment...)
		} else {
			tooLarge = true
			data = nil
		}
		if err == nil {
			return trimLine(data), tooLarge, consumed, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return trimLine(data), tooLarge, consumed, io.EOF
		}
		return nil, tooLarge, consumed, err
	}
}

func trimLine(value []byte) []byte {
	value = bytesTrimSuffix(value, '\n')
	value = bytesTrimSuffix(value, '\r')
	return value
}

func bytesTrimSuffix(value []byte, suffix byte) []byte {
	if len(value) > 0 && value[len(value)-1] == suffix {
		return value[:len(value)-1]
	}
	return value
}

func versionText(version int) string {
	if version == 0 {
		return ""
	}
	return strconv.Itoa(version)
}

func warning(code, category string, line int64, message string) domain.Warning {
	return domain.Warning{ID: fmt.Sprintf("%s_%d", code, line), Code: code,
		Severity: domain.SeverityWarning, Category: category, Message: message}
}

func parseError(code string, err error) domain.ParseResult {
	return domain.ParseResult{Partial: true,
		Err: &domain.DiagnosticError{Code: code, Category: code, Message: err.Error(), Harness: "pi"}}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func sortIntsByRecord(indices []int, records []indexedRecord) {
	sort.SliceStable(indices, func(i, j int) bool { return records[indices[i]].line < records[indices[j]].line })
}

var _ domain.Adapter = (*Adapter)(nil)
