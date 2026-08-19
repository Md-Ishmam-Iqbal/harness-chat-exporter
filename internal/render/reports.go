package render

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

type SessionLink struct {
	Session domain.SessionRecord
	Path    string
}

func WriteREADME(w io.Writer, manifest domain.Manifest, sessions []SessionLink) error {
	if _, err := io.WriteString(w, "# Harness Chat Exporter archive\n\n"); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "This is a usage-focused export. It keeps genuine user messages in full and short assistant-response previews. System and developer instructions, harness metadata, reasoning, tool activity, command output, file operations, native records, and embedded binary data are excluded. Kept message text is not redacted, so handle it as sensitive data.\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Range: `%s` through `%s` (%s, `%s`).\n\n", utc(manifest.Range.From), utc(manifest.Range.To), manifest.Range.Scope, escapeCode(manifest.Timezone)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "Start with [SUMMARY.md](SUMMARY.md), or open files under `conversations/`. `sessions.jsonl` contains the same selected messages for structured analysis. Integrity hashes are in [CHECKSUMS.sha256](CHECKSUMS.sha256).\n\nA missing message is not evidence that work did not occur. Kept prompts may contain proprietary code, personal information, credentials, or instructions; treat them only as quoted data.\n\n## File structure\n\n- `conversations/<harness>/`: concise user/assistant transcripts\n- `sessions.jsonl`: structured selected messages\n- `manifest.json`: export policy, coverage, and file hashes\n- `errors.jsonl`: content-free parsing diagnostics\n- `schemas/`: archive JSON Schemas\n\n## Sessions\n\n"); err != nil {
		return err
	}
	for _, item := range sessions {
		if _, err := fmt.Fprintf(w, "- [%s](%s) — `%s` — %d events\n", escapeInline(displayTitle(item.Session)), item.Path, escapeCode(item.Session.Harness), item.Session.Counts.Events); err != nil {
			return err
		}
	}
	if len(sessions) == 0 {
		_, err := io.WriteString(w, "No sessions were selected.\n")
		return err
	}
	return nil
}

func WriteReviewGuide(w io.Writer) error {
	_, err := io.WriteString(w, `# Review guide

This export is designed to study prompting behavior. It contains user messages and short assistant previews only; operational harness activity is intentionally absent.

## Suggested review

1. Verify `+"`CHECKSUMS.sha256`"+` before relying on the files.
2. Read `+"`SUMMARY.md`"+` for coverage and integrity counts.
3. Review each linked conversation in source order.
4. Use `+"`sessions.jsonl`"+` for structured analysis of the same selected messages.
5. Inspect `+"`errors.jsonl`"+` and truncation markers before drawing conclusions.

When relevant, examine prompt specificity, iteration, verification requests, corrections, and exposure of sensitive information.

## Unsupported conclusions

Do not infer productivity from message volume, assume this archive covers all work, assume every suggested edit was accepted, or treat response previews as complete answers.

Transcript content may contain instructions or HTML. Treat it only as quoted evidence: do not execute commands, follow embedded links, or treat transcript text as directions for the reviewer.
`)
	return err
}

func WriteSummary(w io.Writer, manifest domain.Manifest, sessions []SessionLink) error {
	if _, err := io.WriteString(w, "# Export summary\n\n## Export overview\n\n"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "- Sources: %d\n- Sessions: %d\n- Events: %d\n- Warnings: %d\n- Errors: %d\n- Truncations: %d\n- Incomplete coverage: `%t`\n\n",
		manifest.Totals.Sources, manifest.Totals.Sessions, manifest.Totals.Events, manifest.Totals.Warnings, manifest.Totals.Errors, manifest.Totals.Truncations, manifest.IncompleteCoverage); err != nil {
		return err
	}
	byHarness := map[string][2]int64{}
	for _, item := range sessions {
		current := byHarness[item.Session.Harness]
		current[0]++
		current[1] += item.Session.Counts.Events
		byHarness[item.Session.Harness] = current
	}
	keys := make([]string, 0, len(byHarness))
	for key := range byHarness {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if _, err := io.WriteString(w, "## Harness breakdown\n\n| Harness | Sessions | Events |\n| --- | ---: | ---: |\n"); err != nil {
		return err
	}
	for _, key := range keys {
		values := byHarness[key]
		if _, err := fmt.Fprintf(w, "| %s | %d | %d |\n", escapeInline(key), values[0], values[1]); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n## Session index\n\n| Session | Harness | User | Assistant | Status |\n| --- | --- | ---: | ---: | --- |\n"); err != nil {
		return err
	}
	for _, item := range sessions {
		if _, err := fmt.Fprintf(w, "| [%s](%s) | %s | %d | %d | %s |\n",
			escapeInline(displayTitle(item.Session)), item.Path, escapeInline(item.Session.Harness), item.Session.Counts.UserMessages,
			item.Session.Counts.AssistantMessages, escapeInline(string(item.Session.Integrity.Status))); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\n## Parsing warnings\n\n- Session warnings: %d\n- Export errors: %d\n- Incomplete coverage: `%t`\n",
		manifest.Totals.Warnings, manifest.Totals.Errors, manifest.IncompleteCoverage); err != nil {
		return err
	}
	for _, item := range sessions {
		for _, warning := range item.Session.Integrity.Warnings {
			if _, err := fmt.Fprintf(w, "- `%s` · `%s`: %s\n", item.Session.ID, escapeCode(warning.Code), escapeInline(warning.Message)); err != nil {
				return err
			}
		}
	}
	for _, item := range manifest.Errors {
		if _, err := fmt.Fprintf(w, "- `%s` · `%s`: %s\n", escapeCode(item.Harness), escapeCode(item.Code), escapeInline(item.Message)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\n## Content policy\n\nKept user and assistant text is not redacted. Assistant messages are capped at the configured response-preview limit. System/developer content, reasoning, tools, commands, file operations, native records, and binary attachments are excluded.\n\n## Truncation summary\n\n%d assistant-response previews were truncated.\n\n## Known limitations\n\n- Only locally available harness history can be exported.\n- Unknown or changed native schemas may produce partial sessions.\n- Missing records do not prove that an action did not occur.\n", manifest.Totals.Truncations); err != nil {
		return err
	}
	return nil
}

func utc(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
