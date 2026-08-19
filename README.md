# Harness Chat Exporter

`hce` is a local, read-only CLI that exports coding-agent conversation history
to an ordinary ZIP archive. It preserves evidence for later human or LLM review;
it does not upload, judge, score, or execute anything from a transcript.

Licensed under the [MIT License](LICENSE).

## Disclosure policy

Exports are usage-focused. User messages are kept in full and assistant
messages are capped to a short preview (512 bytes by default). Consecutive
assistant updates are combined into one response before truncation. System/developer
instructions, reasoning, tool activity, commands, file operations, native
records, and embedded binary data are excluded. Use `--max-response-size` to
choose a different preview limit.

Treat every generated archive as sensitive. ZIP files and temporary files are
created with owner-only permissions where the platform supports them.

## Install and run

### macOS, Ubuntu, Arch, and other Linux distributions

```bash
curl -fsSL https://raw.githubusercontent.com/Md-Ishmam-Iqbal/harness-chat-exporter/main/install.sh | sh
```

### Windows PowerShell

```powershell
irm https://raw.githubusercontent.com/Md-Ishmam-Iqbal/harness-chat-exporter/main/install.ps1 | iex
```

Then run:

```bash
hce
```

That is the complete default workflow. `hce` automatically discovers supported
harnesses, exports messages from the previous week, and chooses a collision-safe
ZIP filename in the current directory. No configuration file is required.

The installers detect Intel/AMD or ARM automatically, verify the release
checksum, and install the native binary. Linux packages are statically built and
work across Ubuntu, Arch, and other modern distributions without Go or a runtime.
The Windows installer uses a per-user directory and adds it to the user `PATH`;
the macOS/Linux installer uses `/usr/local/bin` and asks for `sudo` only when
needed.

Alternative installation with Go:

```bash
go install github.com/Md-Ishmam-Iqbal/harness-chat-exporter/cmd/hce@latest
```

Go 1.25 or newer is required only when installing or building from source. To
choose a custom installation directory, set `HCE_INSTALL_DIR` before running the
installer. Re-running the same installer upgrades an existing installation.

The default export covers the rolling seven days ending when the command starts
and includes only messages inside that range. Use `--full-sessions` when earlier
context from every matching conversation is required.
The default output is written to the current directory:

```text
harness-chat-exporter_YYYY-MM-DD_YYYY-MM-DD.zip
```

Examples:

```bash
hce --since 10m
hce --since 1h
hce --since 3d
hce --since 1w
hce --output usage.zip
hce --output ./exports/usage.zip
hce --output usage.md
hce --output usage.jsonl
hce --output usage.csv
hce --full-sessions
hce preview --since 1w
hce --from 2026-08-01 --to 2026-08-06
hce --harness claude,codex
hce --max-response-size 2KB
hce --reproducible
hce inspect
hce adapters
hce version
```

`--output` accepts either a complete filename or a directory. The format is
inferred from `.zip`, `.md`, `.jsonl`, or `.csv`; `--format` can make the choice
explicit. Existing files are never overwritten—a
second export receives a suffix such as `usage-2.zip`.

Markdown and JSONL contain the selected conversation text directly. CSV is a
content-free usage report with one row per message: timestamp, date, harness,
session, role, original/retained length, and response-truncation status.

`hce preview` performs the same local selection and reports its exact session
and message counts without publishing an output file. Run commands from any
directory; the working directory affects only the default output location.

## Storage discovery

The exporter reads these standard locations and supports explicit command-line
overrides:

| Harness | Standard source | Override |
| --- | --- | --- |
| Claude Code | `$CLAUDE_CONFIG_DIR` or `~/.claude/projects/` | `--claude-path` |
| Codex | `$CODEX_HOME` or `~/.codex/sessions/` | `--codex-path` |
| OpenCode | `$OPENCODE_DATA_DIR` or `~/.local/share/opencode/` | `--opencode-path` |
| Pi | `$PI_CODING_AGENT_SESSION_DIR` or `~/.pi/agent/sessions/` | `--pi-path` |

No YAML or other exporter configuration file is loaded. CLI flags, harness
environment variables, and built-in defaults are the complete configuration
surface.

### Compatibility status

| Harness | Status | Covered storage generations |
| --- | --- | --- |
| Claude Code | Supported | Tolerant project/subagent transcript JSONL |
| Codex | Supported | Live and archived rollout JSONL, including current response/event families |
| OpenCode | Supported | Current SQLite, root legacy session/message/part, project-local legacy storage |
| Pi | Supported | Version-aware session v1, v2, and v3 trees |

OpenCode SQLite has also been smoke-tested read-only against a local OpenCode
1.18 store. Harness formats are external and may change; unknown records are
excluded from usage transcripts and coverage warnings are reported instead of
being guessed.

## Archive contents

Each archive contains:

```text
README.md
REVIEW_GUIDE.md
SUMMARY.md
manifest.json
sessions.jsonl
errors.jsonl
redactions.jsonl
CHECKSUMS.sha256
conversations/
schemas/
```

`sessions.jsonl` is session-first and streamable. It contains only selected user
messages and assistant previews. Per-session Markdown files are for direct
review. `redactions.jsonl` is empty because kept message text is not redacted.

## Security boundary

- Export is local and makes no network requests.
- Native histories are opened read-only and are never modified or locked.
- Transcript commands, URLs, templates, and embedded markup are never executed.
- Markdown rendering escapes raw HTML.
- Discovery does not recursively follow arbitrary symlinks outside approved
  roots.
- Archive paths are validated against traversal before publication.
- A final archive is published atomically and an existing destination is never
  overwritten.

Exit status `0` means the requested export or preview succeeded, including when
non-fatal coverage warnings were reported. `1` is reserved for warnings from
diagnostic commands such as `inspect`. `2` is invalid input, `3` means no
supported harness was detected, `4` means no sessions matched, `5` is a fatal
source-read failure, `6` is output failure, `7` is strict-mode failure, and `8`
means cancellation. ZIP manifests retain detailed coverage information.

## Known limitations

- Native release binaries are provided for 64-bit Intel/AMD and ARM systems on
  Linux, macOS, and Windows. Other CPU architectures require a source build.
- A single native logical record over 64 MiB is skipped with explicit partial
  coverage. Session files themselves are streamed and may be much larger.
- Git enrichment is limited to metadata already stored by a harness; repository
  state is not reconstructed.
- The exporter cannot include cloud-only, deleted, inaccessible, or encrypted
  content beyond the opaque bytes actually stored locally.
