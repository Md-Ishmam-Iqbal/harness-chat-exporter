package codex

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

type Adapter struct {
	ExplicitRoots []string
}

func New(explicitRoots ...string) *Adapter {
	return &Adapter{ExplicitRoots: append([]string(nil), explicitRoots...)}
}

func (*Adapter) ID() string          { return "codex" }
func (*Adapter) DisplayName() string { return "OpenAI Codex" }

func (a *Adapter) Detect(_ context.Context, env domain.Environment) domain.DetectionResult {
	roots := a.candidateRoots(env)
	result := domain.DetectionResult{Status: domain.DetectionNotDetected}
	for index, root := range roots {
		if root.origin == "default" {
			if info, err := os.Lstat(root.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		info, err := os.Stat(root.path)
		if err != nil || !info.IsDir() {
			continue
		}
		canonical, err := filepath.EvalSymlinks(root.path)
		if err != nil {
			canonical = filepath.Clean(root.path)
		}
		result.Roots = append(result.Roots, domain.DetectedRoot{
			Path: root.path, Canonical: canonical, Origin: root.origin,
			Precedence: index, Readable: true,
		})
	}
	if len(result.Roots) > 0 {
		result.Status = domain.DetectionDetected
	}
	return result
}

func (a *Adapter) Discover(ctx context.Context, options domain.DiscoveryOptions, emit func(domain.SessionReference) error) error {
	roots := options.Roots
	if len(roots) == 0 {
		return nil
	}
	for _, root := range roots {
		for _, directory := range []string{"sessions", "archived_sessions"} {
			base := filepath.Join(root.Canonical, directory)
			if _, err := os.Stat(base); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
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
				reference := domain.SessionReference{
					HarnessID: "codex", CanonicalSourceRoot: root.Canonical,
					CanonicalSourceIdentity: path, DisplayPath: path,
					NativeSessionID: nativeIDFromFilename(entry.Name()),
					SizeBytes:       info.Size(), SourceModifiedAt: &modified, CandidateUpdatedAt: &modified,
					SourceKind: "rollout_jsonl",
					Metadata:   map[string]string{"location": directory, "updated_at_source": "filesystem"},
				}
				return emit(reference)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (*Adapter) Probe(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	file, err := os.Open(reference.CanonicalSourceIdentity)
	if err != nil {
		return probeError(reference, "read_error", err)
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64<<10)
	for line := int64(1); line <= 64; line++ {
		if err := ctx.Err(); err != nil {
			return probeError(reference, "cancelled", err)
		}
		raw, tooLarge, readErr := readRecord(reader, 4<<20)
		if tooLarge {
			return probeError(reference, "size_limit", errors.New("session metadata record exceeds probe limit"))
		}
		if len(raw) > 0 {
			var record envelope
			if json.Unmarshal(raw, &record) == nil && record.Type == "session_meta" {
				var payload sessionMeta
				if json.Unmarshal(record.Payload, &payload) == nil {
					id := firstNonEmpty(payload.SessionID, payload.ID, reference.NativeSessionID)
					started := parseTimestamp(firstNonEmpty(payload.Timestamp, record.Timestamp))
					metadata := map[string]string{
						"working_directory": payload.CWD,
						"forked_from_id":    payload.ForkedFromID,
						"parent_session_id": payload.ParentThreadID,
						"agent_nickname":    payload.AgentNickname,
						"agent_role":        payload.AgentRole,
						"agent_path":        payload.AgentPath,
					}
					if payload.ParentThreadID != "" {
						metadata["subagent"] = "true"
					}
					return domain.ProbeResult{Supported: true, NativeSessionID: id,
						SourceKind: "rollout_jsonl", SourceVersion: payload.CLIVersion,
						StartedAt: started, UpdatedAt: reference.CandidateUpdatedAt,
						Metadata: metadata}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return probeError(reference, "read_error", readErr)
		}
	}
	return domain.ProbeResult{Supported: false, NativeSessionID: reference.NativeSessionID,
		SourceKind: "rollout_jsonl", UpdatedAt: reference.CandidateUpdatedAt,
		Warnings: []domain.Warning{warning("codex_probe_no_meta", "unsupported_format", 0, "no session_meta record found during probe")}}
}

func (*Adapter) Parse(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	before, err := os.Stat(reference.CanonicalSourceIdentity)
	if err != nil {
		return parseError("read_error", err)
	}
	file, err := os.Open(reference.CanonicalSourceIdentity)
	if err != nil {
		return parseError("read_error", err)
	}
	defer file.Close()

	result := domain.ParseResult{}
	commandCalls := map[string]bool{}
	recordLimit := reference.NativeRecordLimit
	if recordLimit <= 0 {
		recordLimit = defaultRecordLimit
	}
	reader := bufio.NewReaderSize(file, 64<<10)
	for line := int64(1); ; line++ {
		if err := ctx.Err(); err != nil {
			return parseError("cancelled", err)
		}
		raw, tooLarge, readErr := readRecord(reader, recordLimit)
		if tooLarge {
			result.Malformed++
			result.Partial = true
			result.Warnings = append(result.Warnings, warning("codex_record_too_large", "size_limit", line, "native record exceeds configured parser ceiling"))
		} else if len(raw) > 0 {
			event, mapErr := mapRecord(raw, line)
			if mapErr != nil {
				result.Malformed++
				result.Partial = true
				result.Warnings = append(result.Warnings, warning("codex_malformed_record", "malformed_record", line, "native record could not be decoded"))
				fallback := malformedEvent(raw, line)
				if err := sink(ctx, fallback); err != nil {
					return parseError("normalization_error", err)
				}
			} else {
				if event.Type == domain.EventCommand && event.Tool != nil && event.Tool.CallID != "" {
					commandCalls[event.Tool.CallID] = true
				}
				if event.Type == domain.EventToolResult && event.Tool != nil && commandCalls[event.Tool.CallID] {
					event.Type = domain.EventCommandResult
					event.Command = &domain.CommandPayload{}
				}
				if err := sink(ctx, event); err != nil {
					return parseError("normalization_error", err)
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return parseError("read_error", readErr)
		}
	}
	after, statErr := os.Stat(reference.CanonicalSourceIdentity)
	if statErr == nil && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())) {
		result.SourceChanged = true
		result.Partial = true
		result.Warnings = append(result.Warnings, warning("codex_concurrent_change", "integrity_warning", 0, "session changed during export"))
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
	if value := env.Variables["CODEX_HOME"]; value != "" {
		values = append(values, rootCandidate{value, "CODEX_HOME"})
	}
	if env.HomeDir != "" {
		values = append(values, rootCandidate{filepath.Join(env.HomeDir, ".codex"), "default"})
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

func nativeIDFromFilename(name string) string {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	parts := strings.Split(base, "-")
	if len(parts) >= 6 {
		return strings.Join(parts[len(parts)-5:], "-")
	}
	return base
}

func probeError(reference domain.SessionReference, code string, err error) domain.ProbeResult {
	return domain.ProbeResult{NativeSessionID: reference.NativeSessionID,
		Err: &domain.DiagnosticError{Code: code, Category: code, Message: err.Error(), Harness: "codex"}}
}

func parseError(code string, err error) domain.ParseResult {
	return domain.ParseResult{Partial: true,
		Err: &domain.DiagnosticError{Code: code, Category: code, Message: err.Error(), Harness: "codex"}}
}

func warning(id, category string, line int64, message string) domain.Warning {
	return domain.Warning{ID: fmt.Sprintf("%s_%d", id, line), Code: id,
		Severity: domain.SeverityWarning, Category: category, Message: message}
}

// readRecord bounds memory by retaining at most limit bytes while still
// consuming through the next JSONL delimiter.
func readRecord(reader *bufio.Reader, limit int64) ([]byte, bool, error) {
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

var _ domain.Adapter = (*Adapter)(nil)
