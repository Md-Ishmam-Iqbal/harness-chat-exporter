package validation

import "testing"

func TestArchivePath(t *testing.T) {
	for _, valid := range []string{"README.md", "schemas/session.schema.json", "conversations/codex/id.md"} {
		if err := ArchivePath(valid); err != nil {
			t.Errorf("rejected %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", ".", "/absolute", "../escape", "a/../b", "a//b", "a\\b", "a\x00b", "trailing/"} {
		if err := ArchivePath(invalid); err == nil {
			t.Errorf("accepted unsafe path %q", invalid)
		}
	}
}

func FuzzArchivePathNeverAcceptsTraversal(f *testing.F) {
	for _, seed := range []string{"../x", "/x", "a/../../x", "safe/name", "a\\..\\b", "\x00"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if ArchivePath(value) == nil {
			if value == "" || value[0] == '/' {
				t.Fatalf("accepted absolute/empty path %q", value)
			}
			for _, part := range splitSlash(value) {
				if part == ".." || part == "." || part == "" {
					t.Fatalf("accepted unsafe segment in %q", value)
				}
			}
		}
	})
}

func splitSlash(value string) []string {
	var result []string
	start := 0
	for index, r := range value {
		if r == '/' {
			result = append(result, value[start:index])
			start = index + 1
		}
	}
	return append(result, value[start:])
}
