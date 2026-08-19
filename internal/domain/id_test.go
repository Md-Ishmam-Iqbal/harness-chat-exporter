package domain

import (
	"testing"
	"time"
)

func TestStableDigestsDoNotUseContent(t *testing.T) {
	first := SessionDigest("codex", "native", "/root")
	second := SessionDigest("codex", "native", "/root")
	if first != second || len(first) != 64 {
		t.Fatalf("digest is not stable: %q %q", first, second)
	}
	if first == SessionDigest("claude", "native", "/root") {
		t.Fatal("harness must participate in identity")
	}
}

func TestReferenceDigestFallsBackToCanonicalSourceIdentity(t *testing.T) {
	first := SessionReferenceDigest(SessionReference{HarnessID: "pi", CanonicalSourceRoot: "/root", CanonicalSourceIdentity: "/root/one.jsonl"})
	second := SessionReferenceDigest(SessionReference{HarnessID: "pi", CanonicalSourceRoot: "/root", CanonicalSourceIdentity: "/root/two.jsonl"})
	if first == second {
		t.Fatal("references without native IDs collided")
	}
}

func TestTimeRangeIsInclusive(t *testing.T) {
	start := mustTime(t, "2026-08-01T00:00:00Z")
	end := mustTime(t, "2026-08-02T00:00:00Z")
	r := TimeRange{From: start, To: end}
	if !r.Contains(start) || !r.Contains(end) || r.Contains(start.Add(-1)) || r.Contains(end.Add(1)) {
		t.Fatal("range containment must be inclusive")
	}
}

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	result, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
