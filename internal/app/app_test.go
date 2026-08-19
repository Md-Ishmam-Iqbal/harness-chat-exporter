package app

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportClaudeAndCodexEndToEndUsageFocused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")

	claudeRoot := filepath.Join(home, "claude")
	codexRoot := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "claude", "current", "session-00000000-0000-4000-8000-000000000001.jsonl"),
		filepath.Join(claudeRoot, "projects", "-Users-example-private-project", "session.jsonl"),
	)
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(codexRoot, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	claudeSource := filepath.Join(claudeRoot, "projects", "-Users-example-private-project", "session.jsonl")
	codexSource := filepath.Join(codexRoot, "sessions", "2026", "08", "06", "rollout.jsonl")
	claudeBefore := sourceSnapshot(t, claudeSource)
	codexBefore := sourceSnapshot(t, codexSource)

	output := filepath.Join(home, "exports")
	var stdout, stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07",
		"--harness", "claude,codex", "--claude-path", claudeRoot,
		"--codex-path", codexRoot, "--output", output, "--file-name", "history.zip",
	}, &stdout, &stderr, "test-version")
	if exitCode != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	if after := sourceSnapshot(t, claudeSource); after != claudeBefore {
		t.Fatalf("Claude source changed: before=%#v after=%#v", claudeBefore, after)
	}
	if after := sourceSnapshot(t, codexSource); after != codexBefore {
		t.Fatalf("Codex source changed: before=%#v after=%#v", codexBefore, after)
	}
	if !strings.Contains(stderr.String(), "not redacted") {
		t.Fatalf("missing disclosure warning: %s", stderr.String())
	}
	for _, canary := range []string{"sk-test-disclosure-canary", "private-thinking-canary", "/Users/example/private-project"} {
		if strings.Contains(stdout.String()+stderr.String(), canary) {
			t.Fatalf("console output leaked transcript content %q", canary)
		}
	}

	archivePath := filepath.Join(output, "history.zip")
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode=%04o", info.Mode().Perm())
	}
	files := readZIP(t, archivePath)
	for _, name := range []string{"README.md", "REVIEW_GUIDE.md", "SUMMARY.md", "manifest.json", "sessions.jsonl", "errors.jsonl", "redactions.jsonl", "CHECKSUMS.sha256"} {
		if _, ok := files[name]; !ok {
			t.Errorf("archive is missing %s", name)
		}
	}
	structured := string(files["sessions.jsonl"])
	for _, canary := range []string{"sk-test-disclosure-canary", "anthropic-disclosure-canary", "I will inspect the tests.", "I will inspect it."} {
		if !strings.Contains(structured, canary) {
			t.Errorf("usage archive omitted %q", canary)
		}
	}
	for _, canary := range []string{"encrypted-reasoning-canary", "private-thinking-canary", "future-claude-canary", "future-secret-canary", "signature-canary", `"type":"command"`, `"type":"tool_result"`} {
		if strings.Contains(structured, canary) {
			t.Errorf("usage archive retained %q", canary)
		}
	}
	if len(files["redactions.jsonl"]) != 0 {
		t.Fatalf("redaction report must be empty: %s", files["redactions.jsonl"])
	}
	if !strings.Contains(string(files["errors.jsonl"]), "malformed_record") {
		t.Fatalf("partial parsing was not disclosed: %s", files["errors.jsonl"])
	}
}

func TestExportHelp(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"export", "--help"}, {"preview", "--help"}, {"adapters", "--help"}} {
		var stdout, stderr bytes.Buffer
		if exitCode := Run(context.Background(), args, &stdout, &stderr, "test-version"); exitCode != 0 {
			t.Fatalf("args=%v exit=%d stderr=%s", args, exitCode, stderr.String())
		}
		if !strings.Contains(stdout.String(), "hce export") || stderr.Len() != 0 {
			t.Fatalf("args=%v unexpected help output stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestAdaptersReportsCompiledParserFamilies(t *testing.T) {
	var output bytes.Buffer
	if exitCode := Run(context.Background(), []string{"adapters"}, &output, io.Discard, "test-version"); exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	for _, value := range []string{"claude\tsupported\ttolerant JSONL", "codex\tsupported\trollout JSONL", "opencode\tsupported\tSQLite, legacy root, legacy project", "session v1-v3"} {
		if !strings.Contains(output.String(), value) {
			t.Fatalf("adapter report missing %q: %s", value, output.String())
		}
	}
}

func TestExportDistinguishesNoHarnessFromNoSessions(t *testing.T) {
	t.Run("no harness", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		code := Run(context.Background(), []string{"export", "--harness", "claude", "--output", t.TempDir()}, io.Discard, io.Discard, "test-version")
		if code != 3 {
			t.Fatalf("exit=%d", code)
		}
	})
	t.Run("empty detected harness", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		root := filepath.Join(home, "claude")
		if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
			t.Fatal(err)
		}
		code := Run(context.Background(), []string{"export", "--harness", "claude", "--claude-path", root, "--output", t.TempDir()}, io.Discard, io.Discard, "test-version")
		if code != 4 {
			t.Fatalf("exit=%d", code)
		}
	})
}

type sourceState struct {
	size int64
	mode os.FileMode
	data string
}

func sourceSnapshot(t *testing.T, path string) sourceState {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sourceState{size: info.Size(), mode: info.Mode().Perm(), data: string(data)}
}

func TestExportAlwaysExcludesToolResults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(root, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	output := filepath.Join(home, "exports")
	exitCode := Run(context.Background(), []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07",
		"--harness", "codex", "--codex-path", root,
		"--output", output, "--file-name", "without-results.zip",
	}, io.Discard, io.Discard, "test-version")
	if exitCode != 0 {
		t.Fatalf("exit=%d", exitCode)
	}
	structured := string(readZIP(t, filepath.Join(output, "without-results.zip"))["sessions.jsonl"])
	if strings.Contains(structured, `"type":"command_result"`) || strings.Contains(structured, `"type":"tool_result"`) {
		t.Fatalf("tool result remained in excluded export: %s", structured)
	}
}

func TestDirectFormatsPreviewAndCollisionSafeNames(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(root, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	baseArgs := []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07",
		"--harness", "codex", "--codex-path", root,
	}
	outputs := map[string]struct {
		contains string
		excludes string
	}{
		"report.md":    {contains: "# Conversations", excludes: "private-thinking-canary"},
		"report.jsonl": {contains: `"record_type":"session"`, excludes: "private-thinking-canary"},
		"usage.csv":    {contains: "original_bytes,retained_bytes,preview_characters,truncated", excludes: "sk-test-disclosure-canary"},
	}
	for filename, checks := range outputs {
		path := filepath.Join(home, filename)
		args := append(append([]string(nil), baseArgs...), "--output", path)
		if code := Run(context.Background(), args, io.Discard, io.Discard, "test-version"); code != 0 {
			t.Fatalf("%s exit=%d", filename, code)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content), checks.contains) || strings.Contains(string(content), checks.excludes) {
			t.Fatalf("unexpected %s content: %s", filename, content)
		}
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("unexpected %s permissions: %v, %v", filename, info, err)
		}
	}

	markdown := filepath.Join(home, "report.md")
	args := append(append([]string(nil), baseArgs...), "--output", markdown)
	if code := Run(context.Background(), args, io.Discard, io.Discard, "test-version"); code != 0 {
		t.Fatalf("collision export exit=%d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "report-2.md")); err != nil {
		t.Fatalf("collision-safe output was not created: %v", err)
	}
	archivePath := filepath.Join(home, "report.zip")
	archiveArgs := append(append([]string(nil), baseArgs...), "--output", archivePath)
	for range 2 {
		if code := Run(context.Background(), archiveArgs, io.Discard, io.Discard, "test-version"); code != 0 {
			t.Fatalf("collision-safe ZIP export exit=%d", code)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "report-2.zip")); err != nil {
		t.Fatalf("collision-safe ZIP was not created: %v", err)
	}

	previewDirectory := filepath.Join(home, "not-created")
	previewPath := filepath.Join(previewDirectory, "preview.zip")
	previewArgs := append([]string{"preview"}, baseArgs[1:]...)
	previewArgs = append(previewArgs, "--output", previewPath)
	var preview bytes.Buffer
	if code := Run(context.Background(), previewArgs, &preview, io.Discard, "test-version"); code != 0 {
		t.Fatalf("preview exit=%d", code)
	}
	if !strings.Contains(preview.String(), "Preview\n") || !strings.Contains(preview.String(), "Messages:") {
		t.Fatalf("unexpected preview: %s", preview.String())
	}
	if _, err := os.Stat(previewPath); !os.IsNotExist(err) {
		t.Fatalf("preview created output: %v", err)
	}
	if _, err := os.Stat(previewDirectory); !os.IsNotExist(err) {
		t.Fatalf("preview created output directory: %v", err)
	}
}

func TestExportOpenCodeLegacyEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("OPENCODE_DB", "")
	t.Setenv("OPENCODE_DATA_DIR", "")
	root, err := filepath.Abs(filepath.Join("..", "..", "testdata", "opencode", "legacy-root"))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(home, "exports")
	code := Run(context.Background(), []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07", "--harness", "opencode",
		"--opencode-path", root, "--output", output, "--file-name", "opencode.zip",
	}, io.Discard, io.Discard, "test-version")
	if code != 0 { // Warnings do not turn a successfully published export into a failure.
		t.Fatalf("exit=%d", code)
	}
	structured := string(readZIP(t, filepath.Join(output, "opencode.zip"))["sessions.jsonl"])
	for _, canary := range []string{"usage-study-canary", "assistant-disclosure-canary"} {
		if !strings.Contains(structured, canary) {
			t.Fatalf("OpenCode export omitted %q", canary)
		}
	}
	for _, canary := range []string{"private-reasoning-canary", "tool-output-canary", "future-part-canary"} {
		if strings.Contains(structured, canary) {
			t.Fatalf("OpenCode export retained %q", canary)
		}
	}
}

func TestExportPiSessionEndToEnd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	fixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "pi", "v3", "full.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(home, "exports")
	code := Run(context.Background(), []string{
		"export", "--from", "2026-08-03", "--to", "2026-08-04", "--harness", "pi",
		"--pi-path", fixture, "--output", output, "--file-name", "pi.zip",
	}, io.Discard, io.Discard, "test-version")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	structured := string(readZIP(t, filepath.Join(output, "pi.zip"))["sessions.jsonl"])
	for _, canary := range []string{"v3-user-canary", "running command"} {
		if !strings.Contains(structured, canary) {
			t.Fatalf("Pi export omitted %q", canary)
		}
	}
	for _, canary := range []string{"v3-private-reasoning-canary", "v3-command-canary", "v3-result-canary", "v3-extension-state-canary", "v3-future-native-canary"} {
		if strings.Contains(structured, canary) {
			t.Fatalf("Pi export retained %q", canary)
		}
	}
	if !strings.Contains(structured, `"code":"parent_session_missing"`) {
		t.Fatalf("Pi integrity warning was not preserved: %s", structured)
	}
}

func TestAllFourHarnessesShareOnePortableArchive(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "OPENCODE_DB", "OPENCODE_DATA_DIR", "PI_CODING_AGENT_SESSION_DIR"} {
		t.Setenv(key, "")
	}
	t.Setenv("HOME", home)
	claudeRoot := filepath.Join(home, "claude")
	codexRoot := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "claude", "current", "session-00000000-0000-4000-8000-000000000001.jsonl"),
		filepath.Join(claudeRoot, "projects", "project", "session.jsonl"),
	)
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(codexRoot, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	opencodeRoot, err := filepath.Abs(filepath.Join("..", "..", "testdata", "opencode", "legacy-root"))
	if err != nil {
		t.Fatal(err)
	}
	piFixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "pi", "v3", "full.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(home, "exports")
	code := Run(context.Background(), []string{
		"export", "--from", "2026-08-03", "--to", "2026-08-07",
		"--claude-path", claudeRoot, "--codex-path", codexRoot,
		"--opencode-path", opencodeRoot, "--pi-path", piFixture,
		"--output", output, "--file-name", "all-four.zip",
	}, io.Discard, io.Discard, "test-version")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	files := readZIP(t, filepath.Join(output, "all-four.zip"))
	structured := string(files["sessions.jsonl"])
	for _, harness := range []string{"claude", "codex", "opencode", "pi"} {
		if !strings.Contains(structured, `"harness":"`+harness+`"`) {
			t.Fatalf("combined archive omitted %s", harness)
		}
	}
	if !strings.Contains(string(files["SUMMARY.md"]), "## Session index") || !strings.Contains(string(files["CHECKSUMS.sha256"]), "sessions.jsonl") {
		t.Fatal("combined archive reports or checksums are incomplete")
	}
}

func TestStrictExportDoesNotPublishPartialArchive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(root, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	output := filepath.Join(home, "exports")
	exitCode := Run(context.Background(), []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07", "--harness", "codex",
		"--codex-path", root, "--strict", "--output", output, "--file-name", "strict.zip",
	}, io.Discard, io.Discard, "test-version")
	if exitCode != 7 {
		t.Fatalf("exit=%d", exitCode)
	}
	if _, err := os.Stat(filepath.Join(output, "strict.zip")); !os.IsNotExist(err) {
		t.Fatalf("strict export published an archive: %v", err)
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("strict export left temporary files: %#v", entries)
	}
}

func TestReproducibleExportProducesIdenticalArchiveBytes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	root := filepath.Join(home, "codex")
	copyFixture(t,
		filepath.Join("..", "..", "testdata", "codex", "current", "rollout-2026-08-06T10-00-00-019fd000-0000-7000-8000-000000000001.jsonl"),
		filepath.Join(root, "sessions", "2026", "08", "06", "rollout.jsonl"),
	)
	output := filepath.Join(home, "exports")
	baseArgs := []string{
		"export", "--from", "2026-08-06", "--to", "2026-08-07", "--harness", "codex",
		"--codex-path", root, "--reproducible", "--output", output,
	}
	for _, filename := range []string{"first.zip", "second.zip"} {
		args := append(append([]string(nil), baseArgs...), "--file-name", filename)
		if code := Run(context.Background(), args, io.Discard, io.Discard, "test-version"); code != 0 {
			t.Fatalf("%s exit=%d", filename, code)
		}
	}
	first, err := os.ReadFile(filepath.Join(output, "first.zip"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(output, "second.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("equivalent reproducible exports differ")
	}
}

func copyFixture(t *testing.T, source, destination string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readZIP(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	result := map[string][]byte{}
	for _, file := range reader.File {
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		result[file.Name] = data
	}
	return result
}
