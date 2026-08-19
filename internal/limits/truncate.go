package limits

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	DefaultResponseBytes     int64 = 512
	DefaultNativeRecordBytes int64 = 64 << 20
)

var ErrInvalidLimit = errors.New("limit must not be negative")

// TruncatedText describes a deterministic, UTF-8-safe head/tail reduction.
// RetainedBytes counts bytes copied from the original value; the disclosure
// marker is deliberately not charged against the configured content budget.
type TruncatedText struct {
	Text          string
	OriginalBytes int64
	RetainedBytes int64
	SHA256        string
	Truncated     bool
}

// TruncateUTF8 keeps a deterministic head and tail. Invalid UTF-8 is treated
// as content that cannot be safely emitted as JSON text and is disclosed by
// the same marker rather than leaking malformed bytes into a spool.
func TruncateUTF8(value string, maxBytes int64) (TruncatedText, error) {
	if maxBytes < 0 {
		return TruncatedText{}, ErrInvalidLimit
	}
	originalBytes := int64(len(value))
	if utf8.ValidString(value) && originalBytes <= maxBytes {
		return TruncatedText{Text: value, OriginalBytes: originalBytes, RetainedBytes: originalBytes}, nil
	}

	budget := int(maxBytes)
	if int64(budget) != maxBytes { // int overflow on a 32-bit build
		budget = int(^uint(0) >> 1)
	}
	headBudget := (budget + 1) / 2
	headEnd := validPrefix(value, headBudget)
	tailBudget := budget - headEnd
	tailStart := validSuffix(value, tailBudget, headEnd)
	retained := headEnd + len(value) - tailStart
	marker := fmt.Sprintf("[TRUNCATED: original=%d bytes; retained=%d bytes]", originalBytes, retained)
	sum := sha256.Sum256([]byte(value))
	return TruncatedText{
		Text:          value[:headEnd] + marker + value[tailStart:],
		OriginalBytes: originalBytes,
		RetainedBytes: int64(retained),
		SHA256:        hex.EncodeToString(sum[:]),
		Truncated:     true,
	}, nil
}

func validPrefix(value string, budget int) int {
	end := 0
	for end < len(value) {
		r, size := utf8.DecodeRuneInString(value[end:])
		if r == utf8.RuneError && size == 1 || end+size > budget {
			break
		}
		end += size
	}
	return end
}

func validSuffix(value string, budget, minimum int) int {
	start := len(value)
	used := 0
	for start > minimum {
		r, size := utf8.DecodeLastRuneInString(value[minimum:start])
		if r == utf8.RuneError && size == 1 || used+size > budget {
			break
		}
		start -= size
		used += size
	}
	return start
}
