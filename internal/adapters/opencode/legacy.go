package opencode

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func probeLegacy(ctx context.Context, reference domain.SessionReference) domain.ProbeResult {
	if err := ctx.Err(); err != nil {
		return diagnostic("cancelled", err)
	}
	raw, tooLarge, err := readBoundedFile(reference.CanonicalSourceIdentity, defaultRecordLimit)
	if err != nil {
		return diagnostic("read_error", err)
	}
	if tooLarge {
		return diagnostic("size_limit", errors.New("OpenCode legacy session record exceeds parser ceiling"))
	}
	var session map[string]any
	if err := json.Unmarshal(raw, &session); err != nil {
		return diagnostic("malformed_record", errors.New("OpenCode legacy session record is malformed"))
	}
	id := firstNonEmpty(valueString(session["id"]), reference.NativeSessionID)
	started := firstTime(session, "time_created", "created_at", "created")
	updated := firstTime(session, "time_updated", "updated_at", "updated")
	if times, ok := asObject(session["time"]); ok {
		if started == nil {
			started = parseTime(times["created"])
		}
		if updated == nil {
			updated = parseTime(times["updated"])
		}
	}
	return domain.ProbeResult{Supported: true, NativeSessionID: id, SourceKind: reference.SourceKind,
		SourceVersion: legacyVersion(reference.SourceKind), StartedAt: started, UpdatedAt: updated}
}

func parseLegacy(ctx context.Context, reference domain.SessionReference, sink domain.NativeEventSink) domain.ParseResult {
	before, statErr := os.Stat(reference.CanonicalSourceIdentity)
	if statErr != nil {
		return failedParse("read_error", statErr)
	}
	result := domain.ParseResult{}
	recordLimit := reference.NativeRecordLimit
	if recordLimit <= 0 {
		recordLimit = defaultRecordLimit
	}
	sessionRaw, tooLarge, err := readBoundedFile(reference.CanonicalSourceIdentity, recordLimit)
	if err != nil {
		return failedParse("read_error", err)
	}
	if tooLarge {
		result.Partial = true
		result.Malformed++
		result.Warnings = append(result.Warnings, warn("opencode_legacy_session_too_large", "size_limit", "legacy session record exceeds parser ceiling"))
		return result
	}
	var sessionRow map[string]any
	if err := json.Unmarshal(sessionRaw, &sessionRow); err != nil {
		result.Partial = true
		result.Malformed++
		result.Warnings = append(result.Warnings, warn("opencode_malformed_session", "malformed_record", "legacy session record could not be decoded"))
		event := malformedLegacyEvent("session", reference.CanonicalSourceIdentity, sessionRaw, 0)
		if err := sink(ctx, event); err != nil {
			return failedParse("normalization_error", err)
		}
		return result
	}
	sessionID := firstNonEmpty(valueString(sessionRow["id"]), reference.NativeSessionID)
	sessionEvent := metadataEvent("session:"+sessionID, "legacy.session",
		marshalNative(map[string]any{"session": sessionRow, "storage_generation": reference.SourceKind}),
		legacyObjectTime(sessionRow), 0)
	if err := sink(ctx, sessionEvent); err != nil {
		return failedParse("normalization_error", err)
	}

	messageRoot := reference.Metadata["message_root"]
	entries, err := os.ReadDir(messageRoot)
	if err != nil {
		result.Partial = true
		result.Warnings = append(result.Warnings, warn("opencode_missing_message_directory", "integrity_warning", "legacy session message directory is missing or unreadable"))
		return result
	}
	messagePaths := jsonPaths(entries, messageRoot)
	var index int64 = 1
	for _, messagePath := range messagePaths {
		if err := ctx.Err(); err != nil {
			return failedParse("cancelled", err)
		}
		messageRaw, tooLarge, readErr := readBoundedFile(messagePath, recordLimit)
		if readErr != nil {
			result.Partial = true
			result.Warnings = append(result.Warnings, warn("opencode_legacy_message_read_error", "read_error", "legacy message record is unreadable"))
			continue
		}
		if tooLarge {
			result.Partial = true
			result.Warnings = append(result.Warnings, warn("opencode_legacy_message_too_large", "size_limit", "legacy message record exceeds parser ceiling"))
			continue
		}
		var messageRow map[string]any
		if err := json.Unmarshal(messageRaw, &messageRow); err != nil {
			result.Partial = true
			result.Malformed++
			result.Warnings = append(result.Warnings, warn("opencode_malformed_message", "malformed_record", "legacy message record could not be decoded"))
			if err := sink(ctx, malformedLegacyEvent("message", messagePath, messageRaw, index)); err != nil {
				return failedParse("normalization_error", err)
			}
			index++
			continue
		}
		messageID := firstNonEmpty(valueString(messageRow["id"]), strings.TrimSuffix(filepath.Base(messagePath), filepath.Ext(messagePath)))
		messageNative := marshalNative(map[string]any{"message": messageRow, "storage_generation": reference.SourceKind})
		messageEvent := mapMessageMetadata(messageRow, messageID, messageNative, index)
		if err := sink(ctx, messageEvent); err != nil {
			return failedParse("normalization_error", err)
		}
		index++

		partDirectory := legacyPartDirectory(reference, messageID)
		partEntries, partErr := os.ReadDir(partDirectory)
		if partErr != nil {
			result.Partial = true
			result.Warnings = append(result.Warnings, warn("opencode_missing_part_directory", "integrity_warning", "legacy message part directory is missing or unreadable"))
			continue
		}
		partPaths := jsonPaths(partEntries, partDirectory)
		if len(partPaths) == 0 {
			result.Partial = true
			result.Warnings = append(result.Warnings, warn("opencode_message_without_parts", "integrity_warning", "legacy message has no part records"))
		}
		for _, partPath := range partPaths {
			if err := ctx.Err(); err != nil {
				return failedParse("cancelled", err)
			}
			partRaw, tooLarge, readErr := readBoundedFile(partPath, recordLimit)
			if readErr != nil {
				result.Partial = true
				result.Warnings = append(result.Warnings, warn("opencode_legacy_part_read_error", "read_error", "legacy part record is unreadable"))
				continue
			}
			if tooLarge {
				result.Partial = true
				result.Warnings = append(result.Warnings, warn("opencode_legacy_part_too_large", "size_limit", "legacy part record exceeds parser ceiling"))
				continue
			}
			var partRow map[string]any
			if err := json.Unmarshal(partRaw, &partRow); err != nil {
				result.Partial = true
				result.Malformed++
				result.Warnings = append(result.Warnings, warn("opencode_malformed_part", "malformed_record", "legacy part record could not be decoded"))
				if err := sink(ctx, malformedLegacyEvent("part", partPath, partRaw, index)); err != nil {
					return failedParse("normalization_error", err)
				}
				index++
				continue
			}
			events, warnings := mapPart(messageRow, partRow, sessionRow, index, reference.SourceKind)
			if len(warnings) > 0 {
				result.Partial = true
				result.Warnings = append(result.Warnings, warnings...)
			}
			for _, event := range events {
				if err := sink(ctx, event); err != nil {
					return failedParse("normalization_error", err)
				}
				index++
			}
		}
	}
	after, err := os.Stat(reference.CanonicalSourceIdentity)
	if err == nil && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime())) {
		result.SourceChanged = true
		result.Partial = true
		result.Warnings = append(result.Warnings, warn("opencode_concurrent_change", "integrity_warning", "legacy session changed during export"))
	}
	return result
}

func legacyPartDirectory(reference domain.SessionReference, messageID string) string {
	root := reference.Metadata["part_root"]
	if reference.SourceKind == sourceLegacyLocal {
		return filepath.Join(root, messageID)
	}
	return filepath.Join(root, messageID)
}

func readBoundedFile(path string, limit int64) ([]byte, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, true, nil
	}
	return data, false, nil
}

func jsonPaths(entries []os.DirEntry, root string) []string {
	var result []string
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		result = append(result, filepath.Join(root, entry.Name()))
	}
	sort.Strings(result)
	return result
}

func legacyObjectTime(row map[string]any) *time.Time {
	if result := firstTime(row, "time_created", "created_at", "created"); result != nil {
		return result
	}
	if times, ok := asObject(row["time"]); ok {
		if result := parseTime(times["created"]); result != nil {
			return result
		}
		return parseTime(times["updated"])
	}
	return nil
}

func malformedLegacyEvent(kind, path string, raw []byte, index int64) domain.NativeEvent {
	encoding := "utf8"
	data := string(raw)
	if !utf8.Valid(raw) {
		encoding = "base64"
		data = base64.StdEncoding.EncodeToString(raw)
	}
	native := marshalNative(map[string]any{"kind": kind, "path": path, "encoding": encoding, "data": data})
	return domain.NativeEvent{
		NativeEventID:  fmt.Sprintf("malformed:%s:%d", kind, index),
		SourcePosition: domain.SourcePosition{Index: int64Pointer(index)}, Type: domain.EventUnknown,
		Role: domain.RoleUnknown, TimestampSource: domain.TimestampUnknown, TimestampConfidence: domain.ConfidenceUnknown,
		NativeType: "legacy.malformed_" + kind, Native: native,
		Content: []domain.ContentBlock{{Type: "malformed", Data: cloneRaw(native), Visibility: stringPointer("native")}},
	}
}
