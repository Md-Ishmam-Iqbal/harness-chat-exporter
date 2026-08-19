package limits

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateUTF8DeterministicHeadTail(t *testing.T) {
	input := "ab🙂cd世界ef"
	first, err := TruncateUTF8(input, 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TruncateUTF8(input, 10)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("not deterministic: %#v != %#v", first, second)
	}
	if !first.Truncated || !utf8.ValidString(first.Text) {
		t.Fatalf("expected valid truncated text: %#v", first)
	}
	if !strings.HasPrefix(first.Text, "ab") || !strings.HasSuffix(first.Text, "界ef") {
		t.Fatalf("head/tail not retained: %q", first.Text)
	}
	wantMarker := "[TRUNCATED: original=16 bytes; retained=10 bytes]"
	if !strings.Contains(first.Text, wantMarker) {
		t.Fatalf("missing exact marker %q in %q", wantMarker, first.Text)
	}
	sum := sha256.Sum256([]byte(input))
	if first.OriginalBytes != int64(len(input)) || first.RetainedBytes != 10 || first.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("incorrect metadata: %#v", first)
	}
}

func TestTruncateUTF8LeavesValueWithinLimitUnchanged(t *testing.T) {
	result, err := TruncateUTF8("credential-canary", 64)
	if err != nil {
		t.Fatal(err)
	}
	if result.Truncated || result.Text != "credential-canary" || result.RetainedBytes != int64(len(result.Text)) {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestTruncateUTF8HandlesMalformedInput(t *testing.T) {
	input := string([]byte{'a', 0xff, 'b', 0xfe, 'c'})
	result, err := TruncateUTF8(input, 4)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || !utf8.ValidString(result.Text) || !strings.Contains(result.Text, "original=5 bytes") {
		t.Fatalf("malformed bytes were not safely represented: %#v", result)
	}
}

func TestCapNativeToolResultDoesNotCapUnrelatedFields(t *testing.T) {
	raw := json.RawMessage(`{"credential":"cred-canary","result":{"output":"abcdefghij","metadata":"reasoning-canary"},"input":"input-canary"}`)
	capped, truncations, err := CapNativeToolResult(raw, 4)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(capped, &value); err != nil {
		t.Fatal(err)
	}
	if value["credential"] != "cred-canary" || value["input"] != "input-canary" {
		t.Fatalf("unrelated native fields changed: %s", capped)
	}
	result := value["result"].(string)
	if strings.Contains(result, "abcdefghij") || !strings.Contains(result, "[TRUNCATED:") {
		t.Fatalf("native result duplicate was not capped: %s", capped)
	}
	if len(truncations) != 1 || truncations[0].FieldPath != "native.result" || truncations[0].OriginalBytes <= 4 {
		t.Fatalf("truncations are not stable/path-aware: %#v", truncations)
	}
}

func TestEncodeMalformedRecordRetainsExactBytes(t *testing.T) {
	utf8Raw := []byte("not json: disclosure-canary")
	var text struct{ Encoding, Data string }
	if err := json.Unmarshal(EncodeMalformedRecord(utf8Raw), &text); err != nil {
		t.Fatal(err)
	}
	if text.Encoding != "utf-8" || text.Data != string(utf8Raw) {
		t.Fatalf("UTF-8 raw record changed: %#v", text)
	}
	binaryRaw := []byte{0xff, 0, 0xfe, 'x'}
	if err := json.Unmarshal(EncodeMalformedRecord(binaryRaw), &text); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.StdEncoding.DecodeString(text.Data)
	if err != nil || string(decoded) != string(binaryRaw) || text.Encoding != "base64" {
		t.Fatalf("binary raw record changed: %#v %v", text, err)
	}
}

func TestCapJSONRejectsExcessiveDepthWithoutContent(t *testing.T) {
	raw := json.RawMessage(strings.Repeat("[", maxJSONTraversalDepth+2) + `"secret-canary"` + strings.Repeat("]", maxJSONTraversalDepth+2))
	_, _, err := CapJSONStrings(raw, 4, "tool.output")
	if err == nil || strings.Contains(err.Error(), "secret-canary") {
		t.Fatalf("expected content-free depth error, got %v", err)
	}
}

func FuzzTruncateUTF8(f *testing.F) {
	f.Add("hello🙂world", int64(5))
	f.Add(string([]byte{0xff, 'x'}), int64(1))
	f.Fuzz(func(t *testing.T, value string, max int64) {
		if max < 0 {
			max = 0
		}
		max %= 1024
		result, err := TruncateUTF8(value, max)
		if err != nil {
			t.Fatal(err)
		}
		if !utf8.ValidString(result.Text) {
			t.Fatalf("invalid UTF-8 output for %x", []byte(value))
		}
		if result.RetainedBytes > max || result.OriginalBytes != int64(len(value)) {
			t.Fatalf("invalid accounting: %#v max=%d", result, max)
		}
	})
}
