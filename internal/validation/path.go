package validation

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

func ArchivePath(name string) error {
	if name == "" || !utf8.ValidString(name) || strings.ContainsRune(name, 0) {
		return fmt.Errorf("archive path is empty or invalid UTF-8")
	}
	if strings.Contains(name, "\\") || strings.HasPrefix(name, "/") || path.IsAbs(name) {
		return fmt.Errorf("archive path %q is not slash-relative", name)
	}
	if path.Clean(name) != name || name == "." || strings.HasSuffix(name, "/") {
		return fmt.Errorf("archive path %q is not clean", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("archive path %q contains an unsafe segment", name)
		}
	}
	return nil
}
