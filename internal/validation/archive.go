package validation

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/render"
)

var requiredEntries = []string{
	"CHECKSUMS.sha256", "README.md", "REVIEW_GUIDE.md", "SUMMARY.md",
	"errors.jsonl", "manifest.json", "redactions.jsonl", "sessions.jsonl",
	"schemas/event.schema.json", "schemas/manifest.schema.json", "schemas/session.schema.json",
}

func Archive(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(file, stat.Size())
	if err != nil {
		return fmt.Errorf("open zip: %w", err)
	}
	entries := make(map[string]*zip.File, len(zr.File))
	folded := map[string]string{}
	previous := ""
	for index, entry := range zr.File {
		if err := ArchivePath(entry.Name); err != nil {
			return err
		}
		fold := strings.ToLower(entry.Name)
		if other, exists := folded[fold]; exists {
			return fmt.Errorf("case-folded duplicate entries %q and %q", other, entry.Name)
		}
		folded[fold] = entry.Name
		if _, exists := entries[entry.Name]; exists {
			return fmt.Errorf("duplicate entry %q", entry.Name)
		}
		if index > 0 && entry.Name <= previous {
			return fmt.Errorf("ZIP entries are not in lexical order")
		}
		previous = entry.Name
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", entry.Name)
		}
		entries[entry.Name] = entry
	}
	for _, required := range requiredEntries {
		if entries[required] == nil {
			return fmt.Errorf("required entry %q is missing", required)
		}
	}
	checksums, err := readChecksums(entries["CHECKSUMS.sha256"])
	if err != nil {
		return err
	}
	for name, entry := range entries {
		if name == "CHECKSUMS.sha256" {
			continue
		}
		digest, err := hashEntry(entry)
		if err != nil {
			return err
		}
		if checksums[name] != digest {
			return fmt.Errorf("checksum mismatch for %q", name)
		}
		delete(checksums, name)
	}
	if len(checksums) != 0 {
		return fmt.Errorf("checksum report references absent entries")
	}
	return reconcile(entries)
}

func reconcile(entries map[string]*zip.File) error {
	var manifest domain.Manifest
	if err := decodeEntry(entries["manifest.json"], &manifest); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if manifest.ArchiveFormat != "harness-chat-exporter" || manifest.ArchiveVersion != "1.0" || manifest.RedactionEnabled || manifest.DisclosurePolicy != "usage-focused" || manifest.ReasoningPolicy != "excluded" {
		return fmt.Errorf("manifest policy or format mismatch")
	}
	sessionsReader, err := entries["sessions.jsonl"].Open()
	if err != nil {
		return err
	}
	stats, statsErr := SessionsJSONL(sessionsReader)
	sessionsReader.Close()
	if statsErr != nil {
		return statsErr
	}
	errorsReader, err := entries["errors.jsonl"].Open()
	if err != nil {
		return err
	}
	errorCount, errorsErr := JSONL(errorsReader)
	errorsReader.Close()
	if errorsErr != nil {
		return errorsErr
	}
	redactionsReader, err := entries["redactions.jsonl"].Open()
	if err != nil {
		return err
	}
	redactionCount, redactionsErr := JSONL(redactionsReader)
	redactionsReader.Close()
	if redactionsErr != nil {
		return redactionsErr
	}
	if redactionCount != 0 {
		return fmt.Errorf("redactions.jsonl must be empty")
	}
	var warningCount int64
	for _, adapter := range manifest.Adapters {
		warningCount += int64(len(adapter.Warnings))
	}
	for _, session := range stats.Records {
		warningCount += int64(len(session.Integrity.Warnings))
	}
	if manifest.Totals.Sessions != stats.Sessions || manifest.Totals.Events != stats.Events || manifest.Totals.Errors != errorCount || manifest.Totals.Truncations != stats.Truncation || manifest.Totals.Warnings != warningCount {
		return fmt.Errorf("manifest totals do not reconcile with records")
	}
	if int64(len(manifest.Errors)) != errorCount {
		return fmt.Errorf("manifest errors do not reconcile with errors.jsonl")
	}
	links := make([]render.SessionLink, 0, len(stats.Records))
	for _, session := range stats.Records {
		name, err := render.SessionPath(session)
		if err != nil || entries[name] == nil {
			return fmt.Errorf("session %s Markdown reference is missing or unsafe", session.ID)
		}
		links = append(links, render.SessionLink{Session: session, Path: name})
	}
	if err := matchRenderedEntry(entries["README.md"], func(w io.Writer) error { return render.WriteREADME(w, manifest, links) }); err != nil {
		return fmt.Errorf("README.md: %w", err)
	}
	if err := matchRenderedEntry(entries["REVIEW_GUIDE.md"], render.WriteReviewGuide); err != nil {
		return fmt.Errorf("REVIEW_GUIDE.md: %w", err)
	}
	if err := matchRenderedEntry(entries["SUMMARY.md"], func(w io.Writer) error { return render.WriteSummary(w, manifest, links) }); err != nil {
		return fmt.Errorf("SUMMARY.md: %w", err)
	}
	_, hasCombined := entries["ALL_CONVERSATIONS.md"]
	if manifest.Options.CombinedMarkdown != hasCombined {
		return fmt.Errorf("combined Markdown option and entry presence disagree")
	}
	listed := map[string]domain.ManifestFile{}
	for _, item := range manifest.Files {
		if err := ArchivePath(item.Path); err != nil {
			return err
		}
		if item.Path == "manifest.json" || item.Path == "CHECKSUMS.sha256" {
			return fmt.Errorf("manifest cannot checksum %q", item.Path)
		}
		if _, exists := listed[item.Path]; exists {
			return fmt.Errorf("duplicate manifest file %q", item.Path)
		}
		listed[item.Path] = item
	}
	for name, entry := range entries {
		if name == "manifest.json" || name == "CHECKSUMS.sha256" {
			continue
		}
		item, ok := listed[name]
		if !ok || uint64(item.SizeBytes) != entry.UncompressedSize64 {
			return fmt.Errorf("manifest file metadata mismatch for %q", name)
		}
		digest, err := hashEntry(entry)
		if err != nil || item.SHA256 != digest {
			return fmt.Errorf("manifest file checksum mismatch for %q", name)
		}
		delete(listed, name)
	}
	if len(listed) != 0 {
		return fmt.Errorf("manifest references absent files")
	}
	for _, schema := range []string{"schemas/event.schema.json", "schemas/manifest.schema.json", "schemas/session.schema.json"} {
		var value any
		if err := decodeEntry(entries[schema], &value); err != nil {
			return fmt.Errorf("%s: %w", schema, err)
		}
	}
	return nil
}

func readChecksums(entry *zip.File) (map[string]string, error) {
	r, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	result := map[string]string{}
	scanner := bufio.NewScanner(r)
	previous := ""
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != 64 {
			return nil, fmt.Errorf("invalid checksum line")
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return nil, fmt.Errorf("invalid checksum digest")
		}
		if err := ArchivePath(parts[1]); err != nil {
			return nil, err
		}
		if parts[1] <= previous || result[parts[1]] != "" {
			return nil, fmt.Errorf("checksums are not unique and sorted")
		}
		previous = parts[1]
		result[parts[1]] = parts[0]
	}
	return result, scanner.Err()
}

func hashEntry(entry *zip.File) (string, error) {
	r, err := entry.Open()
	if err != nil {
		return "", err
	}
	defer r.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func decodeEntry(entry *zip.File, target any) error {
	r, err := entry.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	decoder := json.NewDecoder(r)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("trailing JSON value")
	}
	return nil
}

func matchRenderedEntry(entry *zip.File, write func(io.Writer) error) error {
	var expected strings.Builder
	if err := write(&expected); err != nil {
		return err
	}
	r, err := entry.Open()
	if err != nil {
		return err
	}
	actual, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if string(actual) != expected.String() {
		return fmt.Errorf("generated report does not reconcile with manifest and records")
	}
	return nil
}
