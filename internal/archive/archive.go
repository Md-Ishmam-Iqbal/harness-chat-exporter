package archive

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/render"
	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/validation"
)

const DefaultCombinedMarkdownLimit int64 = 25 << 20

var (
	ErrDestinationExists = errors.New("archive destination already exists")
	ErrStrictIncomplete  = errors.New("strict export has incomplete coverage")
	stableZipTime        = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
)

type FinalizedSession struct {
	Record domain.SessionRecord
	Events render.EventStream
}

type Input struct {
	Manifest                domain.Manifest
	Sessions                []FinalizedSession
	Errors                  []domain.DiagnosticError
	Schemas                 map[string][]byte
	IncludeCombinedMarkdown bool
}

type Builder struct {
	CombinedMarkdownLimit int64
}

type Result struct {
	Path     string
	Manifest domain.Manifest
}

func (b Builder) Build(ctx context.Context, destination string, input Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	destination, err := filepath.Abs(destination)
	if err != nil {
		return Result{}, err
	}
	if filepath.Base(destination) == "." || filepath.Base(destination) == string(filepath.Separator) {
		return Result{}, fmt.Errorf("invalid archive destination")
	}
	directory := filepath.Dir(destination)
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		if err != nil {
			return Result{}, fmt.Errorf("destination directory: %w", err)
		}
		return Result{}, fmt.Errorf("destination parent is not a directory")
	}
	if _, err := os.Lstat(destination); err == nil {
		return Result{}, ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	workspace, err := os.MkdirTemp(directory, ".hce-work-*")
	if err != nil {
		return Result{}, err
	}
	if err := os.Chmod(workspace, 0o700); err != nil {
		os.RemoveAll(workspace)
		return Result{}, err
	}
	defer os.RemoveAll(workspace)

	staging := &stager{root: workspace, entries: map[string]fileMetadata{}, folded: map[string]string{}}
	manifest, err := b.stage(ctx, staging, input)
	if err != nil {
		return Result{}, err
	}
	if manifest.Options.Strict && manifest.IncompleteCoverage {
		return Result{}, ErrStrictIncomplete
	}
	temporaryZIP := filepath.Join(workspace, "archive.tmp")
	if err := staging.writeZIP(ctx, temporaryZIP); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(temporaryZIP, 0o600); err != nil {
		return Result{}, err
	}
	if err := validation.Archive(temporaryZIP); err != nil {
		return Result{}, fmt.Errorf("reopen verification: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	// A same-filesystem hard link gives standard-library Go an atomic
	// create-if-absent operation. Unlike os.Rename, it cannot replace a target
	// created between the initial check and publication.
	if err := os.Link(temporaryZIP, destination); err != nil {
		if _, statErr := os.Lstat(destination); statErr == nil {
			return Result{}, ErrDestinationExists
		}
		return Result{}, fmt.Errorf("publish archive: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		// The published file is already valid. Remove it only if it is still the
		// hard link we created, which is necessarily true before this returns.
		_ = os.Remove(destination)
		return Result{}, err
	}
	return Result{Path: destination, Manifest: manifest}, nil
}

func (b Builder) stage(ctx context.Context, staging *stager, input Input) (domain.Manifest, error) {
	for _, name := range []string{"schemas/event.schema.json", "schemas/manifest.schema.json", "schemas/session.schema.json"} {
		data, ok := input.Schemas[name]
		if !ok || !json.Valid(data) {
			return domain.Manifest{}, fmt.Errorf("required valid schema %q not supplied", name)
		}
		if err := staging.addBytes(name, data); err != nil {
			return domain.Manifest{}, err
		}
	}
	sessions := append([]FinalizedSession(nil), input.Sessions...)
	sort.SliceStable(sessions, func(i, j int) bool { return sessionLess(sessions[i].Record, sessions[j].Record) })
	links := make([]render.SessionLink, 0, len(sessions))
	for _, session := range sessions {
		name, err := render.SessionPath(session.Record)
		if err != nil {
			return domain.Manifest{}, err
		}
		links = append(links, render.SessionLink{Session: session.Record, Path: name})
	}

	manifest := input.Manifest
	manifest.ArchiveFormat = "harness-chat-exporter"
	manifest.ArchiveVersion = "1.0"
	manifest.DisclosurePolicy = "usage-focused"
	manifest.ReasoningPolicy = "excluded"
	manifest.RedactionEnabled = false
	manifest.Errors = append([]domain.DiagnosticError(nil), input.Errors...)
	manifest.Truncations = nil
	manifest.Options.CombinedMarkdown = input.IncludeCombinedMarkdown
	sources := map[string]struct{}{}
	var warningCount int64
	incomplete := len(input.Errors) > 0
	for _, adapter := range manifest.Adapters {
		warningCount += int64(len(adapter.Warnings))
		if adapter.Status == domain.DetectionDegraded || len(adapter.Warnings) > 0 {
			incomplete = true
		}
	}

	err := staging.add("sessions.jsonl", func(jsonl io.Writer) error {
		encoder := json.NewEncoder(jsonl)
		for index := range sessions {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := sessions[index]
			if item.Events == nil {
				return fmt.Errorf("session %s has nil event stream", item.Record.ID)
			}
			if err := encoder.Encode(usageSession(item.Record)); err != nil {
				return err
			}
			if item.Record.Source.Path != "" {
				sources[item.Record.Source.Path] = struct{}{}
			}
			warningCount += int64(len(item.Record.Integrity.Warnings))
			if len(item.Record.Integrity.Warnings) > 0 {
				incomplete = true
			}
			if item.Record.Integrity.Status != domain.IntegrityComplete || item.Record.Integrity.ConcurrentlyModified || item.Record.Integrity.MalformedNativeRecords > 0 || item.Record.Integrity.TruncatedEvents > 0 {
				incomplete = true
			}
			name := links[index].Path
			err := staging.add(name, func(markdown io.Writer) error {
				return render.WriteSession(ctx, markdown, item.Record, func(streamContext context.Context, yield func(domain.EventRecord) error) error {
					return item.Events(streamContext, func(event domain.EventRecord) error {
						if event.SessionID != item.Record.ID {
							return fmt.Errorf("event %s belongs to %s, expected %s", event.ID, event.SessionID, item.Record.ID)
						}
						if err := encoder.Encode(usageEvent(event)); err != nil {
							return err
						}
						manifest.Truncations = append(manifest.Truncations, event.Truncations...)
						return yield(event)
					})
				})
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return domain.Manifest{}, err
	}
	manifest.Totals.Sources = int64(len(sources))
	manifest.Totals.Sessions = int64(len(sessions))
	manifest.Totals.Events = 0
	for _, session := range sessions {
		manifest.Totals.Events += session.Record.Counts.Events
	}
	manifest.Totals.Warnings = warningCount
	manifest.Totals.Errors = int64(len(input.Errors))
	manifest.Totals.Truncations = int64(len(manifest.Truncations))
	manifest.IncompleteCoverage = manifest.IncompleteCoverage || incomplete

	if err := staging.add("errors.jsonl", func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		for _, item := range input.Errors {
			if err := encoder.Encode(item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return domain.Manifest{}, err
	}
	if err := staging.addBytes("redactions.jsonl", nil); err != nil {
		return domain.Manifest{}, err
	}
	if err := staging.add("REVIEW_GUIDE.md", render.WriteReviewGuide); err != nil {
		return domain.Manifest{}, err
	}
	if err := staging.add("README.md", func(w io.Writer) error { return render.WriteREADME(w, manifest, links) }); err != nil {
		return domain.Manifest{}, err
	}
	if err := staging.add("SUMMARY.md", func(w io.Writer) error { return render.WriteSummary(w, manifest, links) }); err != nil {
		return domain.Manifest{}, err
	}
	if input.IncludeCombinedMarkdown {
		included, err := b.addCombined(ctx, staging, links)
		if err != nil {
			return domain.Manifest{}, err
		}
		if !included {
			manifest.Options.CombinedMarkdown = false
			manifest.Exclusions = append(manifest.Exclusions, domain.ManifestNotice{Code: "combined_markdown_size_limit", Count: 1, Message: "ALL_CONVERSATIONS.md omitted because its estimated size exceeded the configured limit"})
			if err := staging.replace("README.md", func(w io.Writer) error { return render.WriteREADME(w, manifest, links) }); err != nil {
				return domain.Manifest{}, err
			}
			if err := staging.replace("SUMMARY.md", func(w io.Writer) error { return render.WriteSummary(w, manifest, links) }); err != nil {
				return domain.Manifest{}, err
			}
		}
	}
	manifest.Files = staging.manifestFiles()
	if err := staging.add("manifest.json", func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(manifest)
	}); err != nil {
		return domain.Manifest{}, err
	}
	if err := staging.add("CHECKSUMS.sha256", func(w io.Writer) error {
		names := staging.namesExcept("CHECKSUMS.sha256")
		for _, name := range names {
			if _, err := fmt.Fprintf(w, "%s  %s\n", staging.entries[name].sha256, name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return domain.Manifest{}, err
	}
	return manifest, nil
}

type usageSessionRecord struct {
	RecordType    domain.RecordType   `json:"record_type"`
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	Harness       string              `json:"harness"`
	Counts        domain.RecordCounts `json:"counts"`
	Integrity     domain.Integrity    `json:"integrity"`
}

func usageSession(session domain.SessionRecord) usageSessionRecord {
	return usageSessionRecord{
		RecordType: session.RecordType, SchemaVersion: session.SchemaVersion,
		ID: session.ID, Harness: session.Harness, Counts: session.Counts, Integrity: session.Integrity,
	}
}

type usageEventRecord struct {
	RecordType    domain.RecordType   `json:"record_type"`
	SchemaVersion string              `json:"schema_version"`
	ID            string              `json:"id"`
	SessionID     string              `json:"session_id"`
	Sequence      int64               `json:"sequence"`
	Type          domain.EventType    `json:"type"`
	Role          domain.Role         `json:"role"`
	Content       []usageContentBlock `json:"content"`
	Truncations   []domain.Truncation `json:"truncations,omitempty"`
}

type usageContentBlock struct {
	Text string `json:"text"`
}

func usageEvent(event domain.EventRecord) usageEventRecord {
	content := make([]usageContentBlock, 0, len(event.Content))
	for _, block := range event.Content {
		if block.Text != nil {
			content = append(content, usageContentBlock{Text: *block.Text})
		}
	}
	return usageEventRecord{
		RecordType: event.RecordType, SchemaVersion: event.SchemaVersion,
		ID: event.ID, SessionID: event.SessionID, Sequence: event.Sequence,
		Type: event.Type, Role: event.Role, Content: content, Truncations: event.Truncations,
	}
}

func (b Builder) addCombined(ctx context.Context, staging *stager, links []render.SessionLink) (bool, error) {
	limit := b.CombinedMarkdownLimit
	if limit == 0 {
		limit = DefaultCombinedMarkdownLimit
	}
	estimate := int64(len("# All conversations\n\n"))
	for _, link := range links {
		estimate += staging.entries[link.Path].size + int64(len(link.Path)+32)
		if limit > 0 && estimate > limit {
			return false, nil
		}
	}
	return true, staging.add("ALL_CONVERSATIONS.md", func(w io.Writer) error {
		if _, err := io.WriteString(w, "# All conversations\n\n"); err != nil {
			return err
		}
		for _, link := range links {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "---\n\nSource: [%s](%s)\n\n", link.Session.ID, link.Path); err != nil {
				return err
			}
			file, err := os.Open(staging.entries[link.Path].diskPath)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(w, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
}

func sessionLess(left, right domain.SessionRecord) bool {
	leftKey := []string{left.Harness, projectKey(left), timeKey(left.StartedAt), left.ID}
	rightKey := []string{right.Harness, projectKey(right), timeKey(right.StartedAt), right.ID}
	for index := range leftKey {
		if leftKey[index] != rightKey[index] {
			return leftKey[index] < rightKey[index]
		}
	}
	return false
}

func projectKey(session domain.SessionRecord) string {
	for _, value := range []string{session.Project.RepositoryRoot, session.Project.WorkingDirectory, session.Project.DisplayName} {
		if value != "" {
			return value
		}
	}
	return ""
}

func timeKey(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

type fileMetadata struct {
	diskPath string
	size     int64
	sha256   string
}

type stager struct {
	root    string
	entries map[string]fileMetadata
	folded  map[string]string
}

func (s *stager) addBytes(name string, content []byte) error {
	return s.add(name, func(w io.Writer) error {
		_, err := w.Write(content)
		return err
	})
}

func (s *stager) add(name string, write func(io.Writer) error) error {
	if err := validation.ArchivePath(name); err != nil {
		return err
	}
	fold := strings.ToLower(name)
	if other, exists := s.folded[fold]; exists {
		return fmt.Errorf("archive entry %q collides with %q", name, other)
	}
	diskPath := filepath.Join(s.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(diskPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriterSize(file, 64*1024)
	hash := sha256.New()
	err = write(io.MultiWriter(buffered, hash))
	if flushErr := buffered.Flush(); err == nil {
		err = flushErr
	}
	if syncErr := file.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(diskPath)
		return err
	}
	stat, err := os.Stat(diskPath)
	if err != nil {
		return err
	}
	s.entries[name] = fileMetadata{diskPath: diskPath, size: stat.Size(), sha256: hex.EncodeToString(hash.Sum(nil))}
	s.folded[fold] = name
	return nil
}

func (s *stager) replace(name string, write func(io.Writer) error) error {
	metadata, ok := s.entries[name]
	if !ok {
		return fmt.Errorf("cannot replace absent entry %q", name)
	}
	if err := os.Remove(metadata.diskPath); err != nil {
		return err
	}
	delete(s.entries, name)
	delete(s.folded, strings.ToLower(name))
	return s.add(name, write)
}

func (s *stager) manifestFiles() []domain.ManifestFile {
	names := s.namesExcept("manifest.json", "CHECKSUMS.sha256")
	result := make([]domain.ManifestFile, 0, len(names))
	for _, name := range names {
		metadata := s.entries[name]
		result = append(result, domain.ManifestFile{Path: name, SizeBytes: metadata.size, SHA256: metadata.sha256})
	}
	return result
}

func (s *stager) namesExcept(excluded ...string) []string {
	skip := map[string]bool{}
	for _, name := range excluded {
		skip[name] = true
	}
	names := make([]string, 0, len(s.entries))
	for name := range s.entries {
		if !skip[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (s *stager) writeZIP(ctx context.Context, filename string) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(file)
	for _, name := range s.namesExcept() {
		if err := ctx.Err(); err != nil {
			zw.Close()
			file.Close()
			return err
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.Modified = stableZipTime
		header.SetMode(0o600)
		writer, err := zw.CreateHeader(header)
		if err != nil {
			zw.Close()
			file.Close()
			return err
		}
		source, err := os.Open(s.entries[name].diskPath)
		if err != nil {
			zw.Close()
			file.Close()
			return err
		}
		_, copyErr := io.Copy(writer, source)
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil {
			zw.Close()
			file.Close()
			return errors.Join(copyErr, closeErr)
		}
	}
	if err := zw.Close(); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
