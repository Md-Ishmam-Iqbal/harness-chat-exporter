package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/adapters/claude"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/adapters/codex"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/adapters/opencode"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/adapters/pi"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/archive"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/config"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/pipeline"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/render"
	archiveschemas "github.com/ishmam-iqbal-sazim/harness-chat-exporter/schemas"
)

type registryEntry struct {
	id             string
	parserVersions string
	adapter        domain.Adapter
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	command, options, err := config.Parse(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, config.Usage)
			return domain.ExitSuccess
		}
		fmt.Fprintln(stderr, err)
		return domain.ExitInvalidConfiguration
	}
	entries := registry(options)
	switch command {
	case config.CommandVersion:
		fmt.Fprintln(stdout, version)
		return domain.ExitSuccess
	case config.CommandAdapters:
		for _, entry := range entries {
			fmt.Fprintf(stdout, "%s\tsupported\t%s\n", entry.id, entry.parserVersions)
		}
		return domain.ExitSuccess
	case config.CommandInspect:
		return inspect(ctx, entries, environment(), stdout, stderr)
	case config.CommandPreview:
		return export(ctx, entries, options, environment(), stdout, stderr, version, true)
	case config.CommandExport:
		return export(ctx, entries, options, environment(), stdout, stderr, version, false)
	default:
		return domain.ExitInvalidConfiguration
	}
}

func registry(options config.Options) []registryEntry {
	return []registryEntry{
		{id: "claude", parserVersions: "tolerant JSONL", adapter: claude.New(options.AdapterRoots["claude"]...)},
		{id: "codex", parserVersions: "rollout JSONL", adapter: codex.New(options.AdapterRoots["codex"]...)},
		{id: "opencode", parserVersions: "SQLite, legacy root, legacy project", adapter: opencode.New(options.AdapterRoots["opencode"]...)},
		{id: "pi", parserVersions: "session v1-v3", adapter: pi.New(options.AdapterRoots["pi"]...)},
	}
}

func environment() domain.Environment {
	home, _ := os.UserHomeDir()
	values := map[string]string{}
	for _, key := range []string{
		"CLAUDE_CONFIG_DIR", "CODEX_HOME", "OPENCODE_DB", "OPENCODE_DATA_DIR",
		"PI_CODING_AGENT_DIR", "PI_CODING_AGENT_SESSION_DIR", "XDG_DATA_HOME",
	} {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	started := time.Now()
	return domain.Environment{HomeDir: home, Variables: values, CommandStarted: started, LocalTimezone: started.Location().String()}
}

func selectedEntries(entries []registryEntry, selected []string) []registryEntry {
	wanted := map[string]bool{}
	for _, value := range selected {
		wanted[value] = true
	}
	result := []registryEntry{}
	for _, entry := range entries {
		if wanted[entry.id] && entry.adapter != nil {
			result = append(result, entry)
		}
	}
	return result
}

func inspect(ctx context.Context, entries []registryEntry, env domain.Environment, stdout, stderr io.Writer) int {
	detected := 0
	warnings := false
	for _, entry := range entries {
		result := verifyDetection(entry.id, entry.adapter.Detect(ctx, env))
		if result.Status == domain.DetectionDegraded || len(result.Warnings) > 0 {
			warnings = true
		}
		count := 0
		var firstReference *domain.SessionReference
		if result.Status == domain.DetectionDetected || result.Status == domain.DetectionDegraded {
			detected++
			discoverErr := entry.adapter.Discover(ctx, domain.DiscoveryOptions{Roots: result.Roots}, func(reference domain.SessionReference) error {
				count++
				if firstReference == nil {
					copy := reference
					firstReference = &copy
				}
				return nil
			})
			if discoverErr != nil {
				warnings = true
				fmt.Fprintf(stderr, "warning: %s discovery was incomplete\n", entry.id)
			}
		}
		version := result.Version
		if version == "" && firstReference != nil {
			probe := entry.adapter.Probe(ctx, *firstReference)
			version = firstNonEmpty(probe.SourceVersion, probe.SourceKind, firstReference.SourceVersion, firstReference.SourceKind)
		}
		fmt.Fprintf(stdout, "%s\n  Status: %s\n  Version/layout: %s\n  Sessions: %d\n", entry.adapter.DisplayName(), result.Status, firstNonEmpty(version, "unknown"), count)
		for _, root := range result.Roots {
			fmt.Fprintf(stdout, "  Root: %s (readable=%t, origin=%s)\n", root.Path, root.Readable, root.Origin)
		}
	}
	if err := ctx.Err(); err != nil {
		fmt.Fprintln(stderr, "inspection cancelled")
		return domain.ExitCancelled
	}
	if detected == 0 {
		return domain.ExitNoHarnesses
	}
	if warnings {
		return domain.ExitWarnings
	}
	return domain.ExitSuccess
}

type discovered struct {
	adapter domain.Adapter
	ref     domain.SessionReference
	probe   domain.ProbeResult
}

func export(ctx context.Context, entries []registryEntry, options config.Options, env domain.Environment, stdout, stderr io.Writer, version string, preview bool) int {
	if !preview && options.OutputFormat != config.FormatCSV {
		fmt.Fprintln(stderr, "Warning: exported user messages and assistant previews are not redacted.")
	}
	selected := selectedEntries(entries, options.Harnesses)
	if len(selected) == 0 {
		fmt.Fprintln(stderr, "no requested harness adapter is implemented")
		return domain.ExitNoHarnesses
	}

	detections := make([]domain.AdapterReport, 0, len(selected))
	all := []discovered{}
	detectionWarnings := false
	var detectedCount int
	var discoveryFailures []domain.DiagnosticError
	for _, entry := range selected {
		detection := verifyDetection(entry.id, entry.adapter.Detect(ctx, env))
		report := domain.AdapterReport{ID: entry.id, Status: detection.Status, Roots: []string{}}
		if detection.Status == domain.DetectionDegraded {
			detectionWarnings = true
		}
		if detection.Version != "" {
			version := detection.Version
			report.Version = &version
		}
		for _, warning := range detection.Warnings {
			report.Warnings = append(report.Warnings, warning.ID)
			detectionWarnings = true
		}
		for _, root := range detection.Roots {
			report.Roots = append(report.Roots, root.Path)
		}
		detections = append(detections, report)
		if detection.Status != domain.DetectionDetected && detection.Status != domain.DetectionDegraded {
			continue
		}
		detectedCount++
		err := entry.adapter.Discover(ctx, domain.DiscoveryOptions{Roots: detection.Roots}, func(reference domain.SessionReference) error {
			all = append(all, discovered{adapter: entry.adapter, ref: reference})
			return nil
		})
		if err != nil {
			fmt.Fprintf(stderr, "warning: %s discovery was incomplete\n", entry.id)
			discoveryFailures = append(discoveryFailures, domain.DiagnosticError{
				Code: "discovery_error", Category: "discovery",
				Message: "one or more configured source roots could not be fully discovered", Harness: entry.id,
			})
		}
	}
	if detectedCount == 0 {
		fmt.Fprintln(stderr, "no supported harnesses detected")
		return domain.ExitNoHarnesses
	}
	if len(all) == 0 {
		fmt.Fprintln(stderr, "no sessions discovered")
		if len(discoveryFailures) > 0 {
			return domain.ExitSourceReadFailure
		}
		return domain.ExitNoSessions
	}
	fmt.Fprintf(stdout, "Scanning %d candidate sessions across %d detected harnesses\n", len(all), detectedCount)

	all, deduplicatedCount := deduplicateDiscovered(all, func(reference domain.SessionReference) string {
		return reference.HarnessID + "\x00" + reference.CanonicalSourceIdentity
	})
	warnings := detectionWarnings || len(discoveryFailures) > 0
	errorsList := append([]domain.DiagnosticError(nil), discoveryFailures...)
	probed := make([]discovered, 0, len(all))
	for _, item := range all {
		probe := item.adapter.Probe(ctx, item.ref)
		if probe.Err != nil {
			warnings = true
			errorsList = append(errorsList, *probe.Err)
			continue
		}
		if len(probe.Warnings) > 0 {
			warnings = true
		}
		if !probe.Supported {
			warnings = true
			errorsList = append(errorsList, domain.DiagnosticError{
				Code: "unsupported_format", Category: "parse",
				Message: "session source was skipped because its native format could not be identified",
				Harness: item.ref.HarnessID,
			})
			continue
		}
		if probe.NativeSessionID != "" {
			item.ref.NativeSessionID = probe.NativeSessionID
		}
		item.ref.SourceVersion = probe.SourceVersion
		item.ref.NativeRecordLimit = options.MaxNativeRecordBytes
		if item.ref.Metadata == nil {
			item.ref.Metadata = map[string]string{}
		}
		for key, value := range probe.Metadata {
			if value != "" {
				item.ref.Metadata[key] = value
			}
		}
		item.ref.CandidateStartedAt = probe.StartedAt
		if probe.UpdatedAt != nil {
			item.ref.CandidateUpdatedAt = probe.UpdatedAt
		}
		item.probe = probe
		probed = append(probed, item)
	}
	all, duplicateSessions := deduplicateDiscovered(probed, func(reference domain.SessionReference) string {
		if reference.NativeSessionID == "" {
			return ""
		}
		return reference.HarnessID + "\x00" + reference.NativeSessionID
	})
	deduplicatedCount += duplicateSessions
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].ref.HarnessID != all[j].ref.HarnessID {
			return all[i].ref.HarnessID < all[j].ref.HarnessID
		}
		return all[i].ref.DisplayPath < all[j].ref.DisplayPath
	})
	parents := indexParentSessions(all)
	if len(all) == 0 {
		return domain.ExitNoSessions
	}
	workspaceRoot := options.OutputDirectory
	if preview {
		workspaceRoot = os.TempDir()
	} else if err := os.MkdirAll(options.OutputDirectory, 0o700); err != nil {
		fmt.Fprintln(stderr, "cannot create output directory")
		return domain.ExitOutputFailure
	}
	workspace, err := pipeline.NewWorkspace(workspaceRoot)
	if err != nil {
		fmt.Fprintln(stderr, "cannot create export workspace")
		return domain.ExitOutputFailure
	}
	publicationSucceeded := false
	defer func() {
		if options.KeepTemp && !publicationSucceeded {
			fmt.Fprintf(stderr, "Retained temporary workspace: %s\n", workspace.Path())
			return
		}
		_ = workspace.Close()
	}()

	finalized := []*pipeline.FinalizedSession{}
	var selectedRecordBytes int64
	var totalLimitExclusions int64
	var rangeExclusions int64
	for _, item := range all {
		if err := ctx.Err(); err != nil {
			cleanupFinalized(finalized, options.KeepTemp)
			return domain.ExitCancelled
		}
		probe := item.probe
		linkParentSession(&item.ref, parents)
		if definitelyOutsideRange(item.ref, options.Range) {
			rangeExclusions++
			continue
		}
		session := baseSession(item.ref)
		session.Integrity.Warnings = append(session.Integrity.Warnings, probe.Warnings...)
		if item.ref.Metadata["parent_session_id"] != "" && item.ref.Metadata["parent_normalized_id"] == "" {
			warnings = true
			session.Integrity.Warnings = append(session.Integrity.Warnings, domain.Warning{
				ID: "parent_session_missing_" + session.ID, Code: "parent_session_missing",
				Severity: domain.SeverityWarning, Category: "integrity",
				Message: "the native parent session was not available in discovered local history",
			})
		}
		var finished *pipeline.FinalizedSession
		var parsed domain.ParseResult
		var finishErr error
		for attempt := 0; attempt < 2; attempt++ {
			sink, sinkErr := pipeline.NewSessionSink(workspace, pipeline.SessionOptions{
				Reference: item.ref, Session: session, Range: options.Range,
				Scope: domain.SessionScope(options.SessionScope), MaxResponseBytes: options.MaxResponseBytes,
				MaxNativeRecordBytes: options.MaxNativeRecordBytes,
			})
			if sinkErr != nil {
				cleanupFinalized(finalized, options.KeepTemp)
				return domain.ExitOutputFailure
			}
			parsed = item.adapter.Parse(ctx, item.ref, sink.NativeSink())
			if parsed.SourceChanged && attempt == 0 {
				_ = sink.Abort()
				continue
			}
			finished, finishErr = sink.Finalize(ctx, parsed)
			if finishErr != nil {
				_ = sink.Abort()
			}
			break
		}
		if finishErr != nil {
			warnings = true
			errorsList = append(errorsList, domain.DiagnosticError{Code: "session_finalize", Category: "io", Message: "session could not be finalized", Harness: item.ref.HarnessID})
			continue
		}
		if parsed.Partial || parsed.Err != nil || len(parsed.Warnings) > 0 {
			warnings = true
		}
		errorsList = append(errorsList, finished.Diagnostics()...)
		if finished.Session.Integrity.Status != domain.IntegrityComplete || finished.Session.Integrity.ConcurrentlyModified {
			warnings = true
		}
		if finished.Included {
			var sessionRecordBytes int64
			if info, statErr := os.Stat(finished.SpoolPath()); statErr == nil {
				sessionRecordBytes = info.Size()
			}
			if options.MaxSessionBytes > 0 && sessionRecordBytes > options.MaxSessionBytes {
				warnings = true
				finished.Session.Integrity.Warnings = append(finished.Session.Integrity.Warnings, domain.Warning{
					ID: "max_session_size_" + finished.Session.ID, Code: "max_session_size",
					Severity: domain.SeverityWarning, Category: "limit",
					Message: "normalized session records exceeded the configured soft session limit; the complete session was retained",
				})
			}
			if options.MaxTotalBytes > 0 && selectedRecordBytes > options.MaxTotalBytes-sessionRecordBytes {
				warnings = true
				totalLimitExclusions++
				errorsList = append(errorsList, domain.DiagnosticError{
					Code: "max_total_size", Category: "limit",
					Message: "session omitted because normalized session records exceeded the configured total export limit",
					Harness: item.ref.HarnessID, SessionID: finished.Session.ID,
				})
				_ = finished.Close()
				continue
			}
			selectedRecordBytes += sessionRecordBytes
			finalized = append(finalized, finished)
		} else {
			_ = finished.Close()
		}
	}
	if len(finalized) == 0 {
		return domain.ExitNoSessions
	}
	if options.Strict && warnings {
		cleanupFinalized(finalized, options.KeepTemp)
		return domain.ExitStrictFailure
	}

	fileName := options.FileName
	if fileName == "" {
		fileName = config.DefaultFileNameFor(options.Range, options.OutputFormat)
	}
	destination, err := uniqueDestination(filepath.Join(options.OutputDirectory, fileName))
	if err != nil {
		cleanupFinalized(finalized, options.KeepTemp)
		fmt.Fprintf(stderr, "cannot select output destination: %v\n", err)
		return domain.ExitOutputFailure
	}
	conversations := make([]render.Conversation, 0, len(finalized))
	for _, item := range finalized {
		value := item
		conversations = append(conversations, render.Conversation{Session: value.Session, Events: value.Replay})
	}
	if preview {
		printPreview(stdout, options, destination, finalized, warnings, len(errorsList))
		cleanupFinalized(finalized, options.KeepTemp)
		publicationSucceeded = true
		return domain.ExitSuccess
	}

	if options.OutputFormat != config.FormatZIP {
		resultPath, publishErr := publishDirect(ctx, destination, func(writer io.Writer) error {
			switch options.OutputFormat {
			case config.FormatMarkdown:
				return render.WriteMarkdown(ctx, writer, conversations)
			case config.FormatJSONL:
				return render.WriteJSONL(ctx, writer, conversations)
			case config.FormatCSV:
				return render.WriteCSV(ctx, writer, conversations)
			default:
				return fmt.Errorf("unsupported direct output format %q", options.OutputFormat)
			}
		})
		cleanupFinalized(finalized, options.KeepTemp)
		if publishErr != nil {
			if errors.Is(publishErr, context.Canceled) {
				return domain.ExitCancelled
			}
			fmt.Fprintf(stderr, "output publication failed: %v\n", publishErr)
			return domain.ExitOutputFailure
		}
		publicationSucceeded = true
		printCreated(stdout, resultPath, finalizedCounts(conversations))
		if warnings {
			fmt.Fprintln(stderr, "Completed with coverage warnings; selected messages were still exported successfully.")
		}
		return domain.ExitSuccess
	}

	archiveSessions := make([]archive.FinalizedSession, 0, len(finalized))
	for _, item := range finalized {
		value := item
		archiveSessions = append(archiveSessions, archive.FinalizedSession{Record: value.Session, Events: value.Replay})
	}
	schemaFiles, err := archiveschemas.ArchiveFiles()
	if err != nil {
		cleanupFinalized(finalized, options.KeepTemp)
		return domain.ExitOutputFailure
	}
	exportedAt := env.CommandStarted
	if options.Reproducible {
		exportedAt = options.Range.To.UTC()
	}
	manifest := domain.Manifest{
		CommandVersion: version, ExportedAt: exportedAt,
		Range:    domain.ManifestRange{From: options.Range.From.UTC(), To: options.Range.To.UTC(), Scope: domain.SessionScope(options.SessionScope)},
		Timezone: env.LocalTimezone, Adapters: detections,
		Options: domain.ManifestOptions{Harnesses: options.Harnesses, ResponseLimit: options.MaxResponseBytes,
			SessionSoftLimit: options.MaxSessionBytes,
			TotalExportLimit: options.MaxTotalBytes, NativeRecordLimit: options.MaxNativeRecordBytes,
			Strict: options.Strict, Reproducible: options.Reproducible},
	}
	if deduplicatedCount > 0 {
		manifest.Deduplications = append(manifest.Deduplications, domain.ManifestNotice{
			Code: "duplicate_source", Count: deduplicatedCount,
			Message: "duplicate session sources were omitted using native or canonical source identity",
		})
	}
	if totalLimitExclusions > 0 {
		manifest.Exclusions = append(manifest.Exclusions, domain.ManifestNotice{
			Code: "max_total_size", Count: totalLimitExclusions,
			Message: "sessions were omitted after normalized session records reached the configured total export limit",
		})
	}
	if rangeExclusions > 0 {
		manifest.Exclusions = append(manifest.Exclusions, domain.ManifestNotice{
			Code: "outside_range", Count: rangeExclusions,
			Message: "sessions with reliable source bounds wholly outside the selected period were not parsed",
		})
	}
	builder := archive.Builder{}
	includeCombined := options.CombinedMarkdown != "never"
	if options.CombinedMarkdown == "always" {
		builder.CombinedMarkdownLimit = -1
	}
	result, err := builder.Build(ctx, destination, archive.Input{
		Manifest: manifest, Sessions: archiveSessions, Errors: errorsList,
		Schemas: schemaFiles, IncludeCombinedMarkdown: includeCombined,
	})
	cleanupFinalized(finalized, options.KeepTemp)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return domain.ExitCancelled
		}
		fmt.Fprintf(stderr, "archive publication failed: %v\n", err)
		return domain.ExitOutputFailure
	}
	publicationSucceeded = true
	printCreated(stdout, result.Path, result.Manifest.Totals)
	if warnings || result.Manifest.IncompleteCoverage {
		fmt.Fprintln(stderr, "Completed with coverage warnings; see manifest.json and errors.jsonl inside the archive.")
	}
	return domain.ExitSuccess
}

func finalizedCounts(conversations []render.Conversation) domain.ManifestTotals {
	counts := domain.ManifestTotals{Sessions: int64(len(conversations))}
	for _, conversation := range conversations {
		counts.Events += conversation.Session.Counts.Events
		counts.Truncations += conversation.Session.Integrity.TruncatedEvents
		counts.Warnings += int64(len(conversation.Session.Integrity.Warnings))
	}
	return counts
}

func printCreated(w io.Writer, path string, totals domain.ManifestTotals) {
	fmt.Fprintf(w, "Created: %s\nSessions: %d\nMessages: %d\n", path, totals.Sessions, totals.Events)
}

func printPreview(w io.Writer, options config.Options, destination string, sessions []*pipeline.FinalizedSession, warnings bool, errorsCount int) {
	var userMessages, assistantMessages, messages int64
	for _, session := range sessions {
		messages += session.Session.Counts.Events
		userMessages += session.Session.Counts.UserMessages
		assistantMessages += session.Session.Counts.AssistantMessages
	}
	fmt.Fprintf(w, "Preview\nRange: %s to %s\nScope: %s\nFormat: %s\nOutput: %s\nSessions: %d\nMessages: %d (%d user, %d assistant)\nWarnings: %t\nErrors: %d\n",
		options.Range.From.Format(time.RFC3339), options.Range.To.Format(time.RFC3339), options.SessionScope,
		options.OutputFormat, destination, len(sessions), messages, userMessages, assistantMessages, warnings, errorsCount)
}

func baseSession(reference domain.SessionReference) domain.SessionRecord {
	title := firstNonEmpty(reference.Metadata["title"], reference.NativeSessionID)
	project := firstNonEmpty(reference.Metadata["project_name"], reference.Metadata["encoded_project"])
	if project == "" {
		project = filepath.Base(filepath.Dir(reference.DisplayPath))
	}
	structure := domain.SessionStructure{Subagent: reference.Metadata["subagent"] == "true"}
	if parentID := reference.Metadata["parent_normalized_id"]; parentID != "" {
		structure.ParentSessionID = &parentID
	}
	var summary *string
	if value := reference.Metadata["summary"]; value != "" {
		summary = &value
	}
	return domain.SessionRecord{
		RecordType: domain.RecordSession, SchemaVersion: domain.SchemaVersion,
		ID:      normalizedSessionID(reference),
		Harness: reference.HarnessID, HarnessVersion: reference.SourceVersion,
		NativeSessionID: reference.NativeSessionID, Title: title, Summary: summary,
		Project: domain.Project{DisplayName: project, WorkingDirectory: reference.Metadata["working_directory"],
			WorkingDirectoryRedacted: false, RepositoryRoot: reference.Metadata["repository_root"],
			RepositoryName: reference.Metadata["repository_name"], GitBranch: reference.Metadata["git_branch"],
			GitCommit: reference.Metadata["git_commit"], GitMetadataSource: reference.Metadata["git_metadata_source"]},
		Source: domain.Source{Path: reference.DisplayPath, PathRedacted: false,
			Format: reference.SourceKind, FormatVersion: reference.SourceVersion, SizeBytes: reference.SizeBytes,
			ModifiedAt: reference.SourceModifiedAt},
		ModelUsage: []domain.ModelUsage{}, Structure: structure,
	}
}

func deduplicateDiscovered(values []discovered, key func(domain.SessionReference) string) ([]discovered, int64) {
	seen := map[string]bool{}
	result := []discovered{}
	var duplicates int64
	for _, value := range values {
		identity := key(value.ref)
		if identity != "" && seen[identity] {
			duplicates++
			continue
		}
		if identity != "" {
			seen[identity] = true
		}
		result = append(result, value)
	}
	return result, duplicates
}

func closeFinalized(values []*pipeline.FinalizedSession) {
	for _, value := range values {
		_ = value.Close()
	}
}

func cleanupFinalized(values []*pipeline.FinalizedSession, keep bool) {
	if !keep {
		closeFinalized(values)
	}
}

type parentIndex struct {
	bySource map[string]string
	byNative map[string]string
}

func indexParentSessions(values []discovered) parentIndex {
	index := parentIndex{bySource: map[string]string{}, byNative: map[string]string{}}
	for _, value := range values {
		id := normalizedSessionID(value.ref)
		index.bySource[filepath.Clean(value.ref.CanonicalSourceIdentity)] = id
		if value.ref.NativeSessionID != "" {
			index.byNative[value.ref.HarnessID+"\x00"+value.ref.NativeSessionID] = id
		}
	}
	return index
}

func linkParentSession(reference *domain.SessionReference, index parentIndex) {
	if reference.Metadata == nil {
		return
	}
	parent := reference.Metadata["parent_session_id"]
	if parent == "" {
		return
	}
	if id := index.byNative[reference.HarnessID+"\x00"+parent]; id != "" {
		reference.Metadata["parent_normalized_id"] = id
		return
	}
	path := parent
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(reference.CanonicalSourceIdentity), path)
	}
	if canonical, err := filepath.EvalSymlinks(path); err == nil {
		path = canonical
	}
	if id := index.bySource[filepath.Clean(path)]; id != "" {
		reference.Metadata["parent_normalized_id"] = id
	}
}

func normalizedSessionID(reference domain.SessionReference) string {
	return "hh_ses_" + domain.SessionReferenceDigest(reference)[:20]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func definitelyOutsideRange(reference domain.SessionReference, selected domain.TimeRange) bool {
	if reference.CandidateUpdatedAt != nil && reference.CandidateUpdatedAt.Before(selected.From) {
		return true
	}
	if reference.CandidateStartedAt != nil && reference.CandidateStartedAt.After(selected.To) {
		return true
	}
	return false
}

func verifyDetection(adapterID string, result domain.DetectionResult) domain.DetectionResult {
	for index := range result.Roots {
		root := &result.Roots[index]
		file, err := os.Open(root.Canonical)
		if err == nil {
			if info, statErr := file.Stat(); statErr != nil {
				err = statErr
			} else if info.IsDir() {
				_, readErr := file.Readdirnames(1)
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					err = readErr
				}
			}
			_ = file.Close()
		}
		root.Readable = err == nil
		if !root.Readable {
			result.Status = domain.DetectionDegraded
			result.Warnings = append(result.Warnings, domain.Warning{
				ID: fmt.Sprintf("%s_unreadable_root_%d", adapterID, index+1), Code: "permission_error",
				Severity: domain.SeverityWarning, Category: "io",
				Message: "a detected harness storage root is not readable",
			})
		}
	}
	return result
}
