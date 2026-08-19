package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/ncruces/go-sqlite3/driver"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

const defaultRecordLimit = int64(64 << 20)

const (
	sourceSQLite      = "opencode_sqlite"
	sourceLegacyRoot  = "opencode_legacy_root"
	sourceLegacyLocal = "opencode_legacy_project"
)

type Adapter struct {
	ExplicitRoots []string
}

func New(explicitRoots ...string) *Adapter {
	return &Adapter{ExplicitRoots: append([]string(nil), explicitRoots...)}
}

func (*Adapter) ID() string          { return "opencode" }
func (*Adapter) DisplayName() string { return "OpenCode" }

func (a *Adapter) Detect(ctx context.Context, env domain.Environment) domain.DetectionResult {
	result := domain.DetectionResult{Status: domain.DetectionNotDetected}
	seen := map[string]bool{}
	versions := map[string]bool{}
	for index, candidate := range a.candidates(env) {
		if err := ctx.Err(); err != nil {
			break
		}
		if candidate.origin == "default" {
			if info, err := os.Lstat(candidate.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				continue
			}
		}
		sources := probeCandidate(candidate.path)
		for _, source := range sources {
			canonical := canonicalPath(source.path)
			if seen[canonical] {
				continue
			}
			seen[canonical] = true
			readable := isReadable(source.path)
			result.Roots = append(result.Roots, domain.DetectedRoot{
				Path: source.path, Canonical: canonical, Origin: candidate.origin,
				Precedence: index, Readable: readable,
			})
			versions[source.kind] = true
			if source.warning != nil {
				result.Warnings = append(result.Warnings, *source.warning)
			}
		}
	}
	if len(result.Roots) > 0 {
		result.Status = domain.DetectionDetected
		for _, root := range result.Roots {
			if !root.Readable {
				result.Status = domain.DetectionDegraded
			}
		}
		if len(result.Warnings) > 0 {
			result.Status = domain.DetectionDegraded
		}
	}
	keys := make([]string, 0, len(versions))
	for value := range versions {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	result.Version = strings.Join(keys, ",")
	return result
}

func (a *Adapter) Discover(ctx context.Context, options domain.DiscoveryOptions, emit func(domain.SessionReference) error) error {
	var discoveryErrors []error
	for _, root := range options.Roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := sourceKind(root.Canonical)
		switch kind {
		case sourceSQLite:
			if err := discoverSQLite(ctx, root, emit); err != nil {
				discoveryErrors = append(discoveryErrors, err)
			}
		case sourceLegacyRoot, sourceLegacyLocal:
			if err := discoverLegacy(ctx, root, kind, emit); err != nil {
				discoveryErrors = append(discoveryErrors, err)
			}
		}
	}
	return errors.Join(discoveryErrors...)
}

func (*Adapter) Probe(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	switch reference.SourceKind {
	case sourceSQLite:
		return probeSQLite(ctx, reference)
	case sourceLegacyRoot, sourceLegacyLocal:
		return probeLegacy(ctx, reference)
	default:
		return domain.ProbeResult{Supported: false, NativeSessionID: reference.NativeSessionID,
			Warnings: []domain.Warning{warn("opencode_unknown_source", "unsupported_format", "unrecognized OpenCode source generation")}}
	}
}

func (*Adapter) Parse(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	switch reference.SourceKind {
	case sourceSQLite:
		return parseSQLite(ctx, reference, sink)
	case sourceLegacyRoot, sourceLegacyLocal:
		return parseLegacy(ctx, reference, sink)
	default:
		return failedParse("unsupported_format", errors.New("unrecognized OpenCode source generation"))
	}
}

type rootCandidate struct{ path, origin string }

func (a *Adapter) candidates(env domain.Environment) []rootCandidate {
	var values []rootCandidate
	for _, root := range a.ExplicitRoots {
		if root != "" {
			values = append(values, rootCandidate{root, "explicit"})
		}
	}
	if value := env.Variables["OPENCODE_DB"]; value != "" {
		values = append(values, rootCandidate{value, "OPENCODE_DB"})
	}
	if value := env.Variables["OPENCODE_DATA_DIR"]; value != "" {
		values = append(values, rootCandidate{value, "OPENCODE_DATA_DIR"})
	}
	if value := env.Variables["XDG_DATA_HOME"]; value != "" {
		values = append(values, rootCandidate{filepath.Join(value, "opencode"), "XDG_DATA_HOME"})
	}
	if env.HomeDir != "" {
		values = append(values, rootCandidate{filepath.Join(env.HomeDir, ".local", "share", "opencode"), "default"})
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

type detectedSource struct {
	path    string
	kind    string
	warning *domain.Warning
}

func probeCandidate(path string) []detectedSource {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if !info.IsDir() {
		if strings.HasSuffix(strings.ToLower(path), ".db") {
			return []detectedSource{sqliteSource(path)}
		}
		return nil
	}
	path = canonicalPath(path)

	var result []detectedSource
	for _, name := range []string{"opencode-next.db", "opencode.db"} {
		database := filepath.Join(path, name)
		if item, err := os.Lstat(database); err == nil && !item.IsDir() && item.Mode()&os.ModeSymlink == 0 {
			result = append(result, sqliteSource(database))
		}
	}
	if kind := sourceKind(path); kind == sourceLegacyRoot || kind == sourceLegacyLocal {
		result = append(result, detectedSource{path: path, kind: kind})
	} else {
		storage := filepath.Join(path, "storage")
		if item, err := os.Lstat(storage); err == nil && item.Mode()&os.ModeSymlink == 0 && sourceKind(storage) == sourceLegacyRoot {
			result = append(result, detectedSource{path: storage, kind: sourceLegacyRoot})
		}
		matches, _ := filepath.Glob(filepath.Join(path, "project", "*", "storage"))
		sort.Strings(matches)
		for _, match := range matches {
			project := filepath.Dir(match)
			projectInfo, projectErr := os.Lstat(project)
			storageInfo, storageErr := os.Lstat(match)
			if projectErr == nil && storageErr == nil && projectInfo.Mode()&os.ModeSymlink == 0 && storageInfo.Mode()&os.ModeSymlink == 0 && sourceKind(match) == sourceLegacyLocal {
				result = append(result, detectedSource{path: match, kind: sourceLegacyLocal})
			}
		}
	}
	return result
}

func sqliteSource(path string) detectedSource {
	if !hasSQLiteHeader(path) {
		value := warn("opencode_invalid_database_header", "unsupported_format", "OpenCode database has an invalid or unsupported SQLite header")
		return detectedSource{path: path, kind: sourceSQLite, warning: &value}
	}
	return detectedSource{path: path, kind: sourceSQLite}
}

func sourceKind(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return ""
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	if !info.IsDir() {
		if strings.HasSuffix(strings.ToLower(path), ".db") {
			return sourceSQLite
		}
		return ""
	}
	if isDirectory(filepath.Join(path, "session", "info")) &&
		isDirectory(filepath.Join(path, "session", "message")) {
		return sourceLegacyLocal
	}
	if isDirectory(filepath.Join(path, "session")) && isDirectory(filepath.Join(path, "message")) {
		return sourceLegacyRoot
	}
	return ""
}

func hasSQLiteHeader(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	header := make([]byte, 16)
	if _, err := file.Read(header); err != nil {
		return false
	}
	return string(header) == "SQLite format 3\x00"
}

func isDirectory(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.IsDir()
}

func isReadable(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	return file.Close() == nil
}

func canonicalPath(path string) string {
	value, err := filepath.EvalSymlinks(path)
	if err == nil {
		return value
	}
	return filepath.Clean(path)
}

func openReadOnly(path string) (*sql.DB, error) {
	u := sqliteFileURL(path)
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	database, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	return database, nil
}

func sqliteFileURL(path string) *url.URL {
	normalized := filepath.ToSlash(path)
	// filepath.ToSlash only recognizes the current platform's separator. Keep
	// Windows paths valid when this helper is exercised by platform-neutral
	// tests, and ensure a drive letter is parsed as a path rather than a URI
	// authority (file:///C:/...), which would otherwise look like a port.
	if len(path) >= 3 && path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		normalized = "/" + strings.ReplaceAll(path, "\\", "/")
	}
	return &url.URL{Scheme: "file", Path: normalized}
}

func discoverLegacy(ctx context.Context, root domain.DetectedRoot, kind string, emit func(domain.SessionReference) error) error {
	var pattern string
	if kind == sourceLegacyLocal {
		pattern = filepath.Join(root.Canonical, "session", "info", "*.json")
	} else {
		pattern = filepath.Join(root.Canonical, "session", "*", "*.json")
	}
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Stat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		id := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		modified := info.ModTime().UTC()
		metadata := map[string]string{"session_path": path, "layout": kind, "updated_at_source": "filesystem"}
		var started *time.Time
		updated := &modified
		if raw, tooLarge, readErr := readBoundedFile(path, defaultRecordLimit); readErr == nil && !tooLarge {
			var session map[string]any
			if json.Unmarshal(raw, &session) == nil {
				id = firstNonEmpty(valueString(session["id"]), id)
				started = firstTime(session, "time_created", "created_at", "created")
				if parsed := firstTime(session, "time_updated", "updated_at", "updated"); parsed != nil {
					updated = parsed
					metadata["updated_at_source"] = "session"
				}
				if times, ok := asObject(session["time"]); ok {
					if started == nil {
						started = parseTime(times["created"])
					}
					if parsed := parseTime(times["updated"]); parsed != nil {
						updated = parsed
						metadata["updated_at_source"] = "session"
					}
				}
				for _, key := range []string{"parent_id", "parentID", "directory", "title", "version", "project_id", "projectID"} {
					if value := valueString(session[key]); value != "" {
						metadata[key] = value
					}
				}
				if metadata["parent_id"] == "" {
					metadata["parent_id"] = metadata["parentID"]
				}
				normalizeMetadata(metadata, session)
			}
		}
		if kind == sourceLegacyLocal {
			metadata["message_root"] = filepath.Join(root.Canonical, "session", "message", id)
			metadata["part_root"] = filepath.Join(root.Canonical, "session", "part", id)
		} else {
			metadata["message_root"] = filepath.Join(root.Canonical, "message", id)
			metadata["part_root"] = filepath.Join(root.Canonical, "part")
			metadata["project_key"] = filepath.Base(filepath.Dir(path))
		}
		if err := emit(domain.SessionReference{
			HarnessID: "opencode", CanonicalSourceRoot: root.Canonical,
			CanonicalSourceIdentity: path, DisplayPath: path, NativeSessionID: id,
			SizeBytes: info.Size(), SourceModifiedAt: &modified, CandidateStartedAt: started, CandidateUpdatedAt: updated,
			SourceKind: kind, SourceVersion: legacyVersion(kind),
			Metadata: metadata,
		}); err != nil {
			return err
		}
	}
	return nil
}

func legacyVersion(kind string) string {
	if kind == sourceLegacyLocal {
		return "project-session-tree"
	}
	return "root-session-message-part"
}

func warn(code, category, message string) domain.Warning {
	return domain.Warning{ID: code, Code: code, Severity: domain.SeverityWarning, Category: category, Message: message}
}

func failedParse(code string, err error) domain.ParseResult {
	return domain.ParseResult{Partial: true, Err: &domain.DiagnosticError{
		Code: code, Category: code, Harness: "opencode", Message: err.Error(),
	}}
}

func diagnostic(code string, err error) domain.ProbeResult {
	return domain.ProbeResult{Err: &domain.DiagnosticError{
		Code: code, Category: code, Harness: "opencode", Message: err.Error(),
	}}
}

func parseTime(value any) *time.Time {
	var parsed time.Time
	switch item := value.(type) {
	case time.Time:
		parsed = item
	case int64:
		parsed = unixTime(float64(item))
	case int:
		parsed = unixTime(float64(item))
	case float64:
		parsed = unixTime(item)
	case json.Number:
		number, err := item.Float64()
		if err != nil {
			return nil
		}
		parsed = unixTime(number)
	case string:
		if number, err := json.Number(item).Float64(); err == nil {
			parsed = unixTime(number)
		} else {
			value, err := time.Parse(time.RFC3339Nano, item)
			if err != nil {
				return nil
			}
			parsed = value
		}
	default:
		return nil
	}
	if parsed.IsZero() {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func unixTime(value float64) time.Time {
	// OpenCode currently uses Unix milliseconds. Accept seconds and
	// micro/nanoseconds as tolerant-reader fallbacks for older/future stores.
	switch {
	case value > 1e17:
		return time.Unix(0, int64(value))
	case value > 1e14:
		return time.Unix(0, int64(value)*int64(time.Microsecond))
	case value > 1e11:
		seconds := int64(value / 1000)
		nanos := int64(value-float64(seconds*1000)) * int64(time.Millisecond)
		return time.Unix(seconds, nanos)
	default:
		seconds := int64(value)
		nanos := int64((value - float64(seconds)) * 1e9)
		return time.Unix(seconds, nanos)
	}
}

func int64Pointer(value int64) *int64    { return &value }
func stringPointer(value string) *string { return &value }

func normalizeMetadata(metadata map[string]string, row map[string]any) {
	aliases := map[string][]string{
		"title":             {"title"},
		"summary":           {"summary"},
		"working_directory": {"directory", "working_directory", "cwd"},
		"project_name":      {"project_name", "project_id", "projectID"},
		"parent_session_id": {"parent_session_id", "parent_id", "parentID"},
		"repository_root":   {"repository_root", "repo_root"},
		"repository_name":   {"repository_name", "repo_name"},
		"git_branch":        {"git_branch", "branch"},
		"git_commit":        {"git_commit", "commit"},
	}
	for canonical, keys := range aliases {
		if metadata[canonical] != "" {
			continue
		}
		for _, key := range keys {
			if value := firstNonEmpty(metadata[key], valueString(row[key])); value != "" {
				metadata[canonical] = value
				break
			}
		}
	}
	if metadata["project_name"] == "" && metadata["working_directory"] != "" {
		metadata["project_name"] = filepath.Base(filepath.Clean(metadata["working_directory"]))
	}
	if metadata["parent_session_id"] != "" {
		metadata["subagent"] = "true"
	}
}

var _ domain.Adapter = (*Adapter)(nil)
