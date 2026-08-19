package pi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

func TestDetectionPrecedenceAndRelativeSettings(t *testing.T) {
	home := t.TempDir()
	agentDir := filepath.Join(home, ".pi", "agent")
	explicit := filepath.Join(home, "explicit")
	environment := filepath.Join(agentDir, "environment")
	settings := filepath.Join(agentDir, "settings-sessions")
	fallback := filepath.Join(agentDir, "sessions")
	for _, directory := range []string{agentDir, explicit, environment, settings, fallback} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), []byte("{\"sessionDir\":\"settings-sessions\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := New(explicit).Detect(context.Background(), domain.Environment{
		HomeDir: home, Variables: map[string]string{"PI_CODING_AGENT_SESSION_DIR": "environment"},
	})
	if result.Status != domain.DetectionDetected {
		t.Fatalf("status = %s warnings=%#v", result.Status, result.Warnings)
	}
	var origins []string
	for _, root := range result.Roots {
		origins = append(origins, root.Origin)
	}
	want := []string{"explicit", "PI_CODING_AGENT_SESSION_DIR", "settings.json", "default"}
	if strings.Join(origins, ",") != strings.Join(want, ",") {
		t.Fatalf("origins = %v, want %v", origins, want)
	}
	canonicalEnvironment, _ := filepath.EvalSymlinks(environment)
	canonicalSettings, _ := filepath.EvalSymlinks(settings)
	if result.Roots[1].Canonical != canonicalEnvironment || result.Roots[2].Canonical != canonicalSettings {
		t.Fatalf("relative roots did not resolve against agent dir: %#v", result.Roots)
	}
}

func TestDiscoverSkipsSymlinksAndProbesHeaders(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "--private-project--")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := fixturePath("v3", "full.jsonl")
	destination := filepath.Join(project, "v3.jsonl")
	copyFile(t, fixture, destination)
	external := filepath.Join(t.TempDir(), "escaped.jsonl")
	copyFile(t, fixture, external)
	if err := os.Symlink(external, filepath.Join(project, "escape.jsonl")); err != nil {
		t.Fatal(err)
	}

	detected := New(root).Detect(context.Background(), domain.Environment{HomeDir: t.TempDir(), Variables: map[string]string{}})
	if len(detected.Roots) != 1 {
		t.Fatalf("detected roots = %#v", detected.Roots)
	}
	var references []domain.SessionReference
	err := New().Discover(context.Background(), domain.DiscoveryOptions{Roots: detected.Roots}, func(reference domain.SessionReference) error {
		references = append(references, reference)
		return nil
	})
	if err != nil || len(references) != 1 {
		t.Fatalf("discover refs=%d err=%v", len(references), err)
	}
	reference := references[0]
	if reference.NativeSessionID != "pi-v3-session" || reference.SourceVersion != "3" ||
		reference.Metadata["cwd"] != "/private/pi-v3-project" ||
		reference.Metadata["parent_session"] == "" || reference.Metadata["subagent"] != "" {
		t.Fatalf("reference = %#v", reference)
	}
	probe := New().Probe(context.Background(), reference)
	if !probe.Supported || probe.SourceVersion != "3" || probe.NativeSessionID != "pi-v3-session" {
		t.Fatalf("probe = %#v", probe)
	}
}

func TestVersionedFixtures(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		file       string
		partial    bool
		assertions func(*testing.T, []domain.NativeEvent, domain.ParseResult)
	}{
		{name: "v1 linear", version: "v1", file: "linear.jsonl", assertions: assertV1},
		{name: "v2 branched", version: "v2", file: "branched.jsonl", partial: true, assertions: assertV2},
		{name: "v3 extension roles", version: "v3", file: "full.jsonl", assertions: assertV3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := fixturePath(test.version, test.file)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			reference := domain.SessionReference{
				HarnessID: "pi", CanonicalSourceRoot: filepath.Dir(path),
				CanonicalSourceIdentity: path, DisplayPath: path,
				NativeSessionID: strings.TrimSuffix(test.file, filepath.Ext(test.file)),
				SizeBytes:       info.Size(),
			}
			var events []domain.NativeEvent
			result := New().Parse(context.Background(), reference, func(_ context.Context, event domain.NativeEvent) error {
				events = append(events, event)
				return nil
			})
			if result.Err != nil || result.Partial != test.partial {
				t.Fatalf("result = %#v", result)
			}
			if len(events) == 0 || events[0].NativeType != "session" {
				t.Fatalf("header was not emitted first: %#v", events)
			}
			test.assertions(t, events, result)
		})
	}
}

func assertV1(t *testing.T, events []domain.NativeEvent, _ domain.ParseResult) {
	t.Helper()
	var native strings.Builder
	counts := map[domain.EventType]int{}
	for _, event := range events {
		native.Write(event.Native)
		counts[event.Type]++
		if event.NativeType != "session" && event.Branch != nil {
			t.Fatalf("v1 event unexpectedly marked as a tree: %#v", event.Branch)
		}
	}
	for _, canary := range []string{"v1-user-canary", "v1-reasoning-canary", "v1-tool-result-canary"} {
		if !strings.Contains(native.String(), canary) {
			t.Fatalf("missing native canary %q", canary)
		}
	}
	if counts[domain.EventUserMessage] != 1 || counts[domain.EventAssistantMessage] != 1 ||
		counts[domain.EventToolCall] != 1 || counts[domain.EventToolResult] != 1 ||
		counts[domain.EventModelChange] != 1 {
		t.Fatalf("v1 counts = %#v", counts)
	}
}

func assertV2(t *testing.T, events []domain.NativeEvent, result domain.ParseResult) {
	t.Helper()
	if result.Malformed != 1 || !hasWarning(result.Warnings, "pi_malformed_record") {
		t.Fatalf("v2 malformed recovery = %#v", result)
	}
	byID := eventMap(events)
	alternate := byID["v2alt000"]
	active := byID["v2active"]
	summary := byID["v2sum000"]
	if alternate.Branch == nil || alternate.Branch.ActivePath == nil || *alternate.Branch.ActivePath ||
		alternate.Branch.BranchID == nil || *alternate.Branch.BranchID != "v2alt000" {
		t.Fatalf("alternate branch = %#v", alternate.Branch)
	}
	if active.Branch == nil || active.Branch.ActivePath == nil || !*active.Branch.ActivePath ||
		active.Branch.Label == nil || *active.Branch.Label != "chosen-path" {
		t.Fatalf("active branch = %#v", active.Branch)
	}
	if summary.Type != domain.EventBranch || summary.Branch == nil || summary.Branch.BranchID == nil ||
		*summary.Branch.BranchID != "v2sum000" {
		t.Fatalf("branch summary = %#v", summary)
	}
	hook := byID["v2hook00"]
	if hook.Type != domain.EventMetadata || len(hook.Content) == 0 || hook.Content[0].Visibility == nil ||
		*hook.Content[0].Visibility != "hidden" {
		t.Fatalf("v2 hook message = %#v", hook)
	}
}

func assertV3(t *testing.T, events []domain.NativeEvent, _ domain.ParseResult) {
	t.Helper()
	byID := eventMap(events)
	if events[0].Type != domain.EventMetadata {
		t.Fatalf("parent session header type = %s", events[0].Type)
	}
	assistant := byID["v3assist"]
	if assistant.Type != domain.EventAssistantMessage || assistant.Model == nil ||
		assistant.Model.Name != "claude-opus" || assistant.Usage == nil ||
		assistant.Usage.InputTokens == nil || *assistant.Usage.InputTokens != 12 {
		t.Fatalf("assistant mapping = %#v", assistant)
	}
	var reasoningFound bool
	for _, block := range assistant.Content {
		if block.Text != nil && *block.Text == "v3-private-reasoning-canary" &&
			block.Visibility != nil && *block.Visibility == "harness_stored" {
			reasoningFound = true
		}
	}
	if !reasoningFound {
		t.Fatal("stored reasoning was not retained with visibility metadata")
	}
	call := byID["v3assist:tool:2"]
	if call.Type != domain.EventCommand || call.Tool == nil || call.Tool.CallID != "v3-call" ||
		call.Command == nil || !strings.Contains(call.Command.Text, "v3-command-canary") {
		t.Fatalf("command call = %#v", call)
	}
	result := byID["v3result"]
	if result.Type != domain.EventCommandResult || result.Tool == nil || result.Tool.CallID != "v3-call" ||
		result.Command == nil || result.Command.ExitCode == nil || *result.Command.ExitCode != 0 {
		t.Fatalf("command result = %#v", result)
	}
	if byID["v3compact"].Type != domain.EventCompaction || byID["v3future"].Type != domain.EventUnknown {
		t.Fatalf("compaction/unknown mapping = %#v %#v", byID["v3compact"], byID["v3future"])
	}
	custom := byID["v3customm"]
	if len(custom.Content) == 0 || custom.Content[0].Visibility == nil || *custom.Content[0].Visibility != "hidden" {
		t.Fatalf("custom message visibility = %#v", custom)
	}
	allNative := ""
	for _, event := range events {
		allNative += string(event.Native)
	}
	for _, canary := range []string{"v3-private-reasoning-canary", "v3-extension-state-canary", "v3-opaque-canary", "v3-future-native-canary"} {
		if !strings.Contains(allNative, canary) {
			t.Fatalf("missing full-disclosure canary %q", canary)
		}
	}
}

func TestParentBeforeChildForOutOfOrderInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out-of-order.jsonl")
	data := strings.Join([]string{
		"{\"type\":\"session\",\"version\":3,\"id\":\"out-order\",\"timestamp\":\"2026-08-03T00:00:00Z\",\"cwd\":\"/x\"}",
		"{\"type\":\"message\",\"id\":\"child\",\"parentId\":\"parent\",\"timestamp\":\"2026-08-03T00:00:02Z\",\"message\":{\"role\":\"user\",\"content\":\"child\"}}",
		"{\"type\":\"message\",\"id\":\"parent\",\"parentId\":null,\"timestamp\":\"2026-08-03T00:00:01Z\",\"message\":{\"role\":\"user\",\"content\":\"parent\"}}",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, result := parsePath(t, New(), path)
	if result.Err != nil || len(events) != 3 || events[1].NativeEventID != "parent" || events[2].NativeEventID != "child" {
		t.Fatalf("order=%v result=%#v", eventIDs(events), result)
	}
}

func TestInvalidGraphsRemainRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid-graph.jsonl")
	data := strings.Join([]string{
		"{\"type\":\"session\",\"version\":3,\"id\":\"invalid\",\"timestamp\":\"2026-08-03T00:00:00Z\",\"cwd\":\"/x\"}",
		"{\"type\":\"message\",\"id\":\"dup\",\"parentId\":null,\"message\":{\"role\":\"user\",\"content\":\"first\"}}",
		"{\"type\":\"message\",\"id\":\"dup\",\"parentId\":null,\"message\":{\"role\":\"user\",\"content\":\"second\"}}",
		"{\"type\":\"message\",\"id\":\"missing\",\"parentId\":\"not-there\",\"message\":{\"role\":\"user\",\"content\":\"kept\"}}",
		"{\"type\":\"message\",\"id\":\"cycle-a\",\"parentId\":\"cycle-b\",\"message\":{\"role\":\"user\",\"content\":\"a\"}}",
		"{\"type\":\"message\",\"id\":\"cycle-b\",\"parentId\":\"cycle-a\",\"message\":{\"role\":\"user\",\"content\":\"b\"}}",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	events, result := parsePath(t, New(), path)
	if result.Err != nil || !result.Partial || len(events) != 6 {
		t.Fatalf("events=%d result=%#v", len(events), result)
	}
	for _, code := range []string{"pi_duplicate_entry_id", "pi_missing_parent", "pi_multiple_roots", "pi_parent_cycle"} {
		if !hasWarning(result.Warnings, code) {
			t.Fatalf("missing warning %s: %#v", code, result.Warnings)
		}
	}
	seen := map[string]bool{}
	for _, id := range eventIDs(events) {
		if seen[id] {
			t.Fatalf("duplicate event identity remained: %v", eventIDs(events))
		}
		seen[id] = true
	}
}

func TestUnsupportedFutureVersionRetainsUnknownRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.jsonl")
	data := strings.Join([]string{
		"{\"type\":\"session\",\"version\":99,\"id\":\"future\",\"timestamp\":\"2026-08-03T00:00:00Z\",\"cwd\":\"/x\",\"futureHeader\":\"kept\"}",
		"{\"type\":\"message\",\"id\":\"future-entry\",\"parentId\":null,\"timestamp\":\"2026-08-03T00:00:01Z\",\"message\":{\"role\":\"user\",\"content\":\"future-content-canary\"},\"futureField\":{\"opaque\":true}}",
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := New().Probe(context.Background(), domain.SessionReference{CanonicalSourceIdentity: path})
	if !probe.Supported || !hasWarning(probe.Warnings, "pi_unsupported_version") {
		t.Fatalf("future probe = %#v", probe)
	}
	events, result := parsePath(t, New(), path)
	if result.Err != nil || !result.Partial || !hasWarning(result.Warnings, "pi_unsupported_version") ||
		len(events) != 2 || events[1].Type != domain.EventUnknown ||
		!strings.Contains(string(events[1].Native), "future-content-canary") {
		t.Fatalf("future events=%#v result=%#v", events, result)
	}
}

func TestConcurrentAppendAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mutable.jsonl")
	copyFile(t, fixturePath("v1", "linear.jsonl"), path)
	adapter := New()
	adapter.afterIndex = func() {
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString("{\"type\":\"message\",\"message\":{\"role\":\"user\",\"content\":\"late\"}}\n"); err != nil {
			t.Fatal(err)
		}
	}
	_, result := parsePath(t, adapter, path)
	if !result.SourceChanged || !result.Partial || !hasWarning(result.Warnings, "pi_concurrent_change") {
		t.Fatalf("concurrent append = %#v", result)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := New().Parse(ctx, domain.SessionReference{CanonicalSourceIdentity: path}, func(context.Context, domain.NativeEvent) error { return nil })
	if cancelled.Err == nil || cancelled.Err.Code != "cancelled" {
		t.Fatalf("cancelled parse = %#v", cancelled)
	}
}

func TestSinkErrorStopsParse(t *testing.T) {
	sentinel := errors.New("sink stopped")
	result := New().Parse(context.Background(), domain.SessionReference{CanonicalSourceIdentity: fixturePath("v1", "linear.jsonl")},
		func(context.Context, domain.NativeEvent) error { return sentinel })
	if result.Err == nil || result.Err.Code != "normalization_error" {
		t.Fatalf("sink error result = %#v", result)
	}
}

func TestParseLeavesSourceUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "read-only.jsonl")
	copyFile(t, fixturePath("v3", "full.jsonl"), path)
	beforeData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, result := parsePath(t, New(), path)
	if result.Err != nil {
		t.Fatalf("parse = %#v", result)
	}
	afterData, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeData) != string(afterData) || before.Size() != after.Size() ||
		before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("Pi source content or metadata changed during parse")
	}
}

func TestDeepTreeUsesIterativeTraversal(t *testing.T) {
	const depth = 20000
	index := sessionIndex{header: sessionHeader{Version: 3}}
	index.records = append(index.records, indexedRecord{line: 1, header: true})
	parent := ""
	for i := 0; i < depth; i++ {
		id := "node-" + decimal(i)
		index.records = append(index.records, indexedRecord{
			line: int64(i + 2), node: nodeMeta{id: id, parentID: parent, typeName: "message"},
		})
		parent = id
	}
	analysis := analyzeGraph(index)
	if analysis.partial || len(analysis.order) != depth+1 {
		t.Fatalf("deep traversal order=%d partial=%v warnings=%#v", len(analysis.order), analysis.partial, analysis.warnings)
	}
}

func parsePath(t *testing.T, adapter *Adapter, path string) ([]domain.NativeEvent, domain.ParseResult) {
	t.Helper()
	var events []domain.NativeEvent
	result := adapter.Parse(context.Background(), domain.SessionReference{CanonicalSourceIdentity: path}, func(_ context.Context, event domain.NativeEvent) error {
		events = append(events, event)
		return nil
	})
	return events, result
}

func eventMap(events []domain.NativeEvent) map[string]domain.NativeEvent {
	result := make(map[string]domain.NativeEvent, len(events))
	for _, event := range events {
		result[event.NativeEventID] = event
	}
	return result
}

func eventIDs(events []domain.NativeEvent) []string {
	result := make([]string, len(events))
	for i, event := range events {
		result[i] = event.NativeEventID
	}
	return result
}

func hasWarning(warnings []domain.Warning, code string) bool {
	for _, value := range warnings {
		if value.Code == code {
			return true
		}
	}
	return false
}

func fixturePath(parts ...string) string {
	values := []string{"..", "..", "..", "testdata", "pi"}
	return filepath.Join(append(values, parts...)...)
}

func copyFile(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func decimal(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
