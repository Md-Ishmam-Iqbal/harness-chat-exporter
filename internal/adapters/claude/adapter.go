package claude

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
	"strings"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

const defaultRecordLimit = int64(64 << 20)

type Adapter struct{ ExplicitRoots []string }

func New(explicitRoots ...string) *Adapter {
	return &Adapter{ExplicitRoots: append([]string(nil), explicitRoots...)}
}

func (*Adapter) ID() string          { return "claude" }
func (*Adapter) DisplayName() string { return "Claude Code" }

func (a *Adapter) Detect(_ context.Context, env domain.Environment) domain.DetectionResult {
	result := domain.DetectionResult{Status: domain.DetectionNotDetected}
	for index, candidate := range a.candidateRoots(env) {
		if candidate.origin == "default" {
			if info, err := os.Lstat(candidate.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		info, err := os.Stat(filepath.Join(candidate.path, "projects"))
		if err != nil || !info.IsDir() {
			continue
		}
		canonical, err := filepath.EvalSymlinks(candidate.path)
		if err != nil {
			canonical = filepath.Clean(candidate.path)
		}
		result.Roots = append(result.Roots, domain.DetectedRoot{Path: candidate.path,
			Canonical: canonical, Origin: candidate.origin, Precedence: index, Readable: true})
	}
	if len(result.Roots) > 0 {
		result.Status = domain.DetectionDetected
	}
	return result
}

func (*Adapter) Discover(ctx context.Context, options domain.DiscoveryOptions, emit func(domain.SessionReference) error) error {
	for _, root := range options.Roots {
		projects := filepath.Join(root.Canonical, "projects")
		err := filepath.WalkDir(projects, func(path string, entry fs.DirEntry, walkErr error) error {
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
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			modified := info.ModTime()
			relative, _ := filepath.Rel(projects, path)
			parts := strings.Split(filepath.ToSlash(relative), "/")
			metadata := map[string]string{"updated_at_source": "filesystem"}
			if len(parts) > 1 {
				metadata["encoded_project"] = parts[0]
			}
			if strings.Contains(filepath.ToSlash(relative), "/subagents/") {
				metadata["subagent"] = "true"
				if len(parts) >= 4 && parts[2] == "subagents" {
					metadata["parent_session_id"] = parts[1]
				}
			}
			return emit(domain.SessionReference{
				HarnessID: "claude", CanonicalSourceRoot: root.Canonical,
				CanonicalSourceIdentity: path, DisplayPath: path,
				NativeSessionID: strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())),
				SizeBytes:       info.Size(), SourceModifiedAt: &modified, CandidateUpdatedAt: &modified,
				SourceKind: "transcript_jsonl",
				Metadata:   metadata,
			})
		})
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (*Adapter) Probe(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	file, err := os.Open(reference.CanonicalSourceIdentity)
	if err != nil {
		return claudeProbeError(reference, "read_error", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	for line := int64(1); line <= 64; line++ {
		if err := ctx.Err(); err != nil {
			return claudeProbeError(reference, "cancelled", err)
		}
		raw, tooLarge, readErr := readLine(reader, 4<<20)
		if tooLarge {
			return claudeProbeError(reference, "size_limit", errors.New("header record exceeds probe limit"))
		}
		if len(raw) > 0 {
			var record nativeRecord
			if json.Unmarshal(raw, &record) == nil && record.Type != "" {
				id := firstNonEmpty(record.SessionID, reference.NativeSessionID)
				return domain.ProbeResult{Supported: true, NativeSessionID: id,
					SourceKind: "transcript_jsonl", SourceVersion: record.Version,
					StartedAt: parseTimestamp(record.Timestamp), UpdatedAt: reference.CandidateUpdatedAt,
					Metadata: map[string]string{"working_directory": record.CWD, "git_branch": record.GitBranch}}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return claudeProbeError(reference, "read_error", readErr)
		}
	}
	return domain.ProbeResult{Supported: false, NativeSessionID: reference.NativeSessionID,
		Warnings: []domain.Warning{claudeWarning("claude_probe_no_record", "unsupported_format", 0, "no recognizable transcript record found")}}
}

func (*Adapter) Parse(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	before, err := os.Stat(reference.CanonicalSourceIdentity)
	if err != nil {
		return claudeParseError("read_error", err)
	}
	file, err := os.Open(reference.CanonicalSourceIdentity)
	if err != nil {
		return claudeParseError("read_error", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	result := domain.ParseResult{}
	recordLimit := reference.NativeRecordLimit
	if recordLimit <= 0 {
		recordLimit = defaultRecordLimit
	}
	for line := int64(1); ; line++ {
		if err := ctx.Err(); err != nil {
			return claudeParseError("cancelled", err)
		}
		raw, tooLarge, readErr := readLine(reader, recordLimit)
		if tooLarge {
			result.Malformed++
			result.Partial = true
			result.Warnings = append(result.Warnings, claudeWarning("claude_record_too_large", "size_limit", line, "native record exceeds configured parser ceiling"))
		} else if len(raw) > 0 {
			events, mapErr := mapNativeRecord(raw, line)
			if mapErr != nil {
				result.Malformed++
				result.Partial = true
				result.Warnings = append(result.Warnings, claudeWarning("claude_malformed_record", "malformed_record", line, "native record could not be decoded"))
				events = []domain.NativeEvent{malformedNativeEvent(raw, line)}
			}
			for _, event := range events {
				if err := sink(ctx, event); err != nil {
					return claudeParseError("normalization_error", err)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return claudeParseError("read_error", readErr)
		}
	}
	after, statErr := os.Stat(reference.CanonicalSourceIdentity)
	if statErr == nil && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())) {
		result.SourceChanged = true
		result.Partial = true
		result.Warnings = append(result.Warnings, claudeWarning("claude_concurrent_change", "integrity_warning", 0, "session changed during export"))
	}
	return result
}

type rootCandidate struct{ path, origin string }

func (a *Adapter) candidateRoots(env domain.Environment) []rootCandidate {
	values := []rootCandidate{}
	for _, root := range a.ExplicitRoots {
		if root != "" {
			values = append(values, rootCandidate{root, "explicit"})
		}
	}
	if value := env.Variables["CLAUDE_CONFIG_DIR"]; value != "" {
		values = append(values, rootCandidate{value, "CLAUDE_CONFIG_DIR"})
	}
	if env.HomeDir != "" {
		values = append(values, rootCandidate{filepath.Join(env.HomeDir, ".claude"), "default"})
	}
	seen := map[string]bool{}
	result := values[:0]
	for _, value := range values {
		absolute, err := filepath.Abs(value.path)
		if err != nil || seen[absolute] {
			continue
		}
		seen[absolute] = true
		value.path = absolute
		result = append(result, value)
	}
	return result
}

func readLine(reader *bufio.Reader, limit int64) ([]byte, bool, error) {
	var data []byte
	var total int64
	tooLarge := false
	for {
		fragment, err := reader.ReadSlice('\n')
		total += int64(len(fragment))
		if !tooLarge && total <= limit {
			data = append(data, fragment...)
		} else {
			tooLarge = true
			data = nil
		}
		if err == nil {
			return trimLine(data), tooLarge, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return trimLine(data), tooLarge, io.EOF
		}
		return nil, tooLarge, err
	}
}

func trimLine(value []byte) []byte {
	if len(value) > 0 && value[len(value)-1] == '\n' {
		value = value[:len(value)-1]
	}
	if len(value) > 0 && value[len(value)-1] == '\r' {
		value = value[:len(value)-1]
	}
	return value
}

func claudeProbeError(reference domain.SessionReference, code string, err error) domain.ProbeResult {
	return domain.ProbeResult{NativeSessionID: reference.NativeSessionID,
		Err: &domain.DiagnosticError{Code: code, Category: code, Message: err.Error(), Harness: "claude"}}
}

func claudeParseError(code string, err error) domain.ParseResult {
	return domain.ParseResult{Partial: true,
		Err: &domain.DiagnosticError{Code: code, Category: code, Message: err.Error(), Harness: "claude"}}
}

func claudeWarning(id, category string, line int64, message string) domain.Warning {
	return domain.Warning{ID: fmt.Sprintf("%s_%d", id, line), Code: id,
		Severity: domain.SeverityWarning, Category: category, Message: message}
}

var _ domain.Adapter = (*Adapter)(nil)
