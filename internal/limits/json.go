package limits

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

const maxJSONTraversalDepth = 256

// EncodeMalformedRecord retains exact malformed source bytes in a valid JSON
// value. UTF-8 input remains readable; arbitrary bytes use standard base64.
func EncodeMalformedRecord(raw []byte) json.RawMessage {
	type encoded struct {
		Encoding string `json:"encoding"`
		Data     string `json:"data"`
	}
	value := encoded{Encoding: "utf-8", Data: string(raw)}
	if !utf8.Valid(raw) {
		value.Encoding = "base64"
		value.Data = base64.StdEncoding.EncodeToString(raw)
	}
	result, _ := json.Marshal(value)
	return result
}

// CapJSONStrings applies the tool-result field cap to every string leaf in a
// normalized result payload. Unchanged payloads retain their original bytes.
func CapJSONStrings(raw json.RawMessage, maxBytes int64, fieldPath string) (json.RawMessage, []domain.Truncation, error) {
	return capJSON(raw, maxBytes, fieldPath, true)
}

// CapNativeToolResult caps recognized body fields in a retained native event.
// It intentionally leaves unrelated native fields byte-for-byte untouched when
// no recognized field needs truncation.
func CapNativeToolResult(raw json.RawMessage, maxBytes int64) (json.RawMessage, []domain.Truncation, error) {
	return capJSON(raw, maxBytes, "native", false)
}

func capJSON(raw json.RawMessage, maxBytes int64, fieldPath string, insideBody bool) (json.RawMessage, []domain.Truncation, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return raw, nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON result payload")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, fmt.Errorf("invalid JSON result payload")
	}
	if !insideBody {
		if _, object := value.(map[string]any); !object {
			insideBody = true
		}
	}
	if err := checkDepth(value, 0); err != nil {
		return nil, nil, err
	}
	if insideBody {
		changed, truncations, err := capRootBody(&value, raw, maxBytes, fieldPath)
		if err != nil {
			return nil, nil, err
		}
		if !changed {
			return raw, nil, nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, nil, fmt.Errorf("encode capped JSON result payload")
		}
		return encoded, truncations, nil
	}
	changed, truncations, err := capValue(&value, maxBytes, fieldPath, 0)
	if err != nil {
		return nil, nil, err
	}
	if !changed {
		return raw, nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode capped JSON result payload")
	}
	return encoded, truncations, nil
}

func capValue(value *any, maxBytes int64, path string, depth int) (bool, []domain.Truncation, error) {
	if depth > maxJSONTraversalDepth {
		return false, nil, fmt.Errorf("JSON result payload exceeds nesting limit")
	}
	switch current := (*value).(type) {
	case []any:
		changed := false
		var all []domain.Truncation
		for index := range current {
			itemChanged, items, err := capValue(&current[index], maxBytes, fmt.Sprintf("%s[%d]", path, index), depth+1)
			if err != nil {
				return false, nil, err
			}
			changed = changed || itemChanged
			all = append(all, items...)
		}
		return changed, all, nil
	case map[string]any:
		changed := false
		var all []domain.Truncation
		keys := make([]string, 0, len(current))
		for key := range current {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := current[key]
			childPath := path + "." + key
			var itemChanged bool
			var items []domain.Truncation
			var err error
			if resultBodyKey(key) {
				itemChanged, items, err = capDecodedBody(&child, maxBytes, childPath)
			} else {
				itemChanged, items, err = capValue(&child, maxBytes, childPath, depth+1)
			}
			if err != nil {
				return false, nil, err
			}
			if itemChanged {
				current[key] = child
			}
			changed = changed || itemChanged
			all = append(all, items...)
		}
		return changed, all, nil
	default:
		return false, nil, nil
	}
}

func capRootBody(value *any, raw json.RawMessage, maxBytes int64, path string) (bool, []domain.Truncation, error) {
	if text, ok := (*value).(string); ok {
		return capTextBody(value, text, maxBytes, path)
	}
	result, err := TruncateUTF8(string(raw), maxBytes)
	if err != nil {
		return false, nil, err
	}
	if !result.Truncated {
		return false, nil, nil
	}
	*value = result.Text
	return true, []domain.Truncation{toTruncation(path, result)}, nil
}

func capDecodedBody(value *any, maxBytes int64, path string) (bool, []domain.Truncation, error) {
	if text, ok := (*value).(string); ok {
		return capTextBody(value, text, maxBytes, path)
	}
	encoded, err := json.Marshal(*value)
	if err != nil {
		return false, nil, fmt.Errorf("encode JSON result body")
	}
	result, err := TruncateUTF8(string(encoded), maxBytes)
	if err != nil {
		return false, nil, err
	}
	if !result.Truncated {
		return false, nil, nil
	}
	*value = result.Text
	return true, []domain.Truncation{toTruncation(path, result)}, nil
}

func capTextBody(value *any, text string, maxBytes int64, path string) (bool, []domain.Truncation, error) {
	result, err := TruncateUTF8(text, maxBytes)
	if err != nil {
		return false, nil, err
	}
	if !result.Truncated {
		return false, nil, nil
	}
	*value = result.Text
	return true, []domain.Truncation{toTruncation(path, result)}, nil
}

func toTruncation(path string, result TruncatedText) domain.Truncation {
	return domain.Truncation{
		FieldPath: path, OriginalBytes: result.OriginalBytes,
		RetainedBytes: result.RetainedBytes, SHA256: result.SHA256,
	}
}

func checkDepth(value any, depth int) error {
	if depth > maxJSONTraversalDepth {
		return fmt.Errorf("JSON result payload exceeds nesting limit")
	}
	switch current := value.(type) {
	case []any:
		for _, child := range current {
			if err := checkDepth(child, depth+1); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, child := range current {
			if err := checkDepth(child, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func resultBodyKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	switch key {
	case "output", "result", "tool_result", "toolresult", "content", "text", "stdout", "stderr":
		return true
	default:
		return false
	}
}
