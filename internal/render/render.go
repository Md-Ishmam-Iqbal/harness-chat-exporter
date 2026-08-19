package render

import (
	"context"
	"fmt"
	"html"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Md-Ishmam-Iqbal/harness-chat-exporter/internal/domain"
)

type EventStream func(context.Context, func(domain.EventRecord) error) error

func SessionPath(session domain.SessionRecord) (string, error) {
	harness := safeComponent(session.Harness)
	if harness == "" || harness != session.Harness {
		return "", fmt.Errorf("unsafe harness %q", session.Harness)
	}
	if !validID(session.ID, "hh_ses_") {
		return "", fmt.Errorf("invalid session id %q", session.ID)
	}
	return path.Join("conversations", harness, session.ID+".md"), nil
}

func validID(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	hex := strings.TrimPrefix(value, prefix)
	if len(hex) < 20 || len(hex) > 64 {
		return false
	}
	for _, r := range hex {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func safeComponent(value string) string {
	if value == "" || !utf8.ValidString(value) {
		return ""
	}
	for _, r := range value {
		if !(unicode.IsLower(r) || unicode.IsDigit(r) || r == '-' || r == '_') || r > unicode.MaxASCII {
			return ""
		}
	}
	return value
}

func WriteSession(ctx context.Context, w io.Writer, _ domain.SessionRecord, events EventStream) error {
	if _, err := io.WriteString(w, "# Conversation\n\n"); err != nil {
		return err
	}
	return writeConversationEvents(ctx, w, events, "##")
}

func writeConversationEvents(ctx context.Context, w io.Writer, events EventStream, headingPrefix string) error {
	return events(ctx, func(event domain.EventRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		heading := ""
		switch event.Type {
		case domain.EventUserMessage:
			heading = "User"
		case domain.EventAssistantMessage:
			heading = "Assistant"
		default:
			return nil
		}
		for _, block := range event.Content {
			if block.Text == nil || strings.TrimSpace(*block.Text) == "" {
				continue
			}
			if _, err := fmt.Fprintf(w, "%s %s\n\n", headingPrefix, heading); err != nil {
				return err
			}
			return writeFence(w, "text", []byte(*block.Text))
		}
		return nil
	})
}

func displayTitle(session domain.SessionRecord) string {
	return "Conversation"
}

func writeFence(w io.Writer, language string, content []byte) error {
	fence := strings.Repeat("`", max(3, longestRun(string(content), '`')+1))
	if _, err := fmt.Fprintf(w, "%s%s\n", fence, safeLanguage(language)); err != nil {
		return err
	}
	if _, err := w.Write(content); err != nil {
		return err
	}
	if len(content) == 0 || content[len(content)-1] != '\n' {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%s\n\n", fence)
	return err
}

func safeLanguage(value string) string {
	var b strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func longestRun(value string, target rune) int {
	best, current := 0, 0
	for _, r := range value {
		if r == target {
			current++
			best = max(best, current)
		} else {
			current = 0
		}
	}
	return best
}

func escapeInline(value string) string {
	value = html.EscapeString(value)
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, marker := range []string{"`", "*", "_", "[", "]", "<", ">"} {
		value = strings.ReplaceAll(value, marker, "\\"+marker)
	}
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}

func escapeCode(value string) string {
	return strings.ReplaceAll(html.EscapeString(value), "`", "&#96;")
}
