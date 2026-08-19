package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ishmam-iqbal-sazim/harness-chat-exporter/internal/domain"
)

type Command string

const Usage = `Usage:
  hce [options]                  export the previous week
  hce export [options]
  hce preview [options]
  hce inspect
  hce adapters
  hce version

Export defaults to messages from the rolling previous week.

Common options:
  --since DURATION                 10m, 1h, 3d, or 1w (default 1w)
  --output PATH                    output file or directory (default current directory)
  --harness claude,codex,opencode,pi
  --format zip|markdown|jsonl|csv  inferred from --output when possible
  --full-sessions                  include complete sessions touched in the range

Advanced options:
  --from DATE [--to DATE]
  --session-scope touched|events-only (legacy)
  --max-response-size 512B
  --max-session-size 250MB
  --max-total-size 1GB (normalized JSONL selection budget)
  --max-native-record-size 64MB
  --combined-markdown auto|always|never
  --file-name NAME                 legacy filename used with an output directory
  --strict | --best-effort
  --keep-temp
  --reproducible
  --claude-path PATH (repeatable)
  --codex-path PATH (repeatable)
  --opencode-path PATH (repeatable)
  --pi-path PATH (repeatable)`

const (
	CommandExport   Command = "export"
	CommandPreview  Command = "preview"
	CommandInspect  Command = "inspect"
	CommandAdapters Command = "adapters"
	CommandVersion  Command = "version"
)

type OutputFormat string

const (
	FormatZIP      OutputFormat = "zip"
	FormatMarkdown OutputFormat = "markdown"
	FormatJSONL    OutputFormat = "jsonl"
	FormatCSV      OutputFormat = "csv"
)

type SessionScope string

const (
	ScopeTouched    SessionScope = "touched"
	ScopeEventsOnly SessionScope = "events-only"
)

type Options struct {
	Range                domain.TimeRange
	OutputDirectory      string
	FileName             string
	OutputFormat         OutputFormat
	Harnesses            []string
	SessionScope         SessionScope
	MaxResponseBytes     int64
	MaxSessionBytes      int64
	MaxTotalBytes        int64
	MaxNativeRecordBytes int64
	Strict               bool
	CombinedMarkdown     string
	KeepTemp             bool
	Reproducible         bool
	AdapterRoots         map[string][]string
}

type sizeValue int64

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("path cannot be empty")
	}
	*s = append(*s, value)
	return nil
}

func (s *sizeValue) String() string { return strconv.FormatInt(int64(*s), 10) }
func (s *sizeValue) Set(value string) error {
	n, err := ParseSize(value)
	if err != nil {
		return err
	}
	*s = sizeValue(n)
	return nil
}

func Parse(args []string) (Command, Options, error) {
	if len(args) == 0 {
		options, err := parseExport(nil, time.Now())
		return CommandExport, options, err
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		return "", Options{}, flag.ErrHelp
	}
	// Flags without an explicit command are the short form of `hce export`.
	if strings.HasPrefix(args[0], "-") {
		options, err := parseExport(args, time.Now())
		return CommandExport, options, err
	}
	command := Command(args[0])
	switch command {
	case CommandExport, CommandPreview, CommandInspect, CommandAdapters, CommandVersion:
	default:
		return "", Options{}, fmt.Errorf("unknown command %q", args[0])
	}
	if command != CommandExport && command != CommandPreview {
		if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
			return "", Options{}, flag.ErrHelp
		}
		if len(args) != 1 {
			return "", Options{}, fmt.Errorf("%s does not accept options yet", command)
		}
		return command, Options{}, nil
	}
	options, err := parseExport(args[1:], time.Now())
	return command, options, err
}

func parseExport(args []string, now time.Time) (Options, error) {
	fs := flag.NewFlagSet("hce export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var since, from, to, harnesses, scope, output, format string
	var strict, bestEffort, fullSessions bool
	var claudeRoots, codexRoots, opencodeRoots, piRoots stringList
	var maxResponse, maxSession, maxTotal, maxRecord sizeValue
	var options Options

	fs.StringVar(&since, "since", "1w", "rolling duration (for example 10m, 1h, 3d, or 1w) or timestamp")
	fs.StringVar(&from, "from", "", "range start")
	fs.StringVar(&to, "to", "", "range end")
	fs.StringVar(&output, "output", ".", "output file or directory")
	fs.StringVar(&options.FileName, "file-name", "", "archive filename")
	fs.StringVar(&harnesses, "harness", "", "comma-separated harness IDs")
	fs.StringVar(&format, "format", "", "zip, markdown, jsonl, or csv")
	fs.StringVar(&scope, "session-scope", string(ScopeEventsOnly), "touched or events-only")
	fs.BoolVar(&fullSessions, "full-sessions", false, "include complete sessions touched in the range")
	maxResponse = sizeValue(512)
	maxSession = sizeValue(250 << 20)
	maxTotal = sizeValue(1 << 30)
	maxRecord = sizeValue(64 << 20)
	fs.Var(&maxResponse, "max-response-size", "assistant response preview byte limit")
	fs.Var(&maxSession, "max-session-size", "session soft byte limit")
	fs.Var(&maxTotal, "max-total-size", "total export byte limit")
	fs.Var(&maxRecord, "max-native-record-size", "native record byte limit")
	fs.BoolVar(&strict, "strict", false, "publish only complete exports")
	fs.BoolVar(&bestEffort, "best-effort", false, "publish partial exports")
	fs.StringVar(&options.CombinedMarkdown, "combined-markdown", "auto", "auto, always, or never")
	fs.BoolVar(&options.KeepTemp, "keep-temp", false, "retain workspace on failure")
	fs.BoolVar(&options.Reproducible, "reproducible", false, "normalize volatile archive metadata")
	fs.Var(&claudeRoots, "claude-path", "explicit Claude storage root (repeatable)")
	fs.Var(&codexRoots, "codex-path", "explicit Codex storage root (repeatable)")
	fs.Var(&opencodeRoots, "opencode-path", "explicit OpenCode storage root or database (repeatable)")
	fs.Var(&piRoots, "pi-path", "explicit Pi session root (repeatable)")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if fs.NArg() != 0 {
		return Options{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	sinceSet := false
	scopeSet := false
	formatSet := false
	fs.Visit(func(flag *flag.Flag) {
		switch flag.Name {
		case "since":
			sinceSet = true
		case "session-scope":
			scopeSet = true
		case "format":
			formatSet = true
		}
	})
	if from != "" && sinceSet {
		return Options{}, errors.New("--since cannot be combined with --from")
	}
	if to != "" && from == "" {
		return Options{}, errors.New("--to requires --from")
	}
	if strict && bestEffort {
		return Options{}, errors.New("--strict conflicts with --best-effort")
	}

	rangeValue, err := ResolveRange(now, since, from, to)
	if err != nil {
		return Options{}, err
	}
	options.Range = rangeValue
	if fullSessions {
		if scopeSet && scope != string(ScopeTouched) {
			return Options{}, errors.New("--full-sessions conflicts with --session-scope events-only")
		}
		scope = string(ScopeTouched)
	}
	options.SessionScope = SessionScope(scope)
	if options.SessionScope != ScopeTouched && options.SessionScope != ScopeEventsOnly {
		return Options{}, fmt.Errorf("invalid --session-scope %q", scope)
	}
	if options.CombinedMarkdown != "auto" && options.CombinedMarkdown != "always" && options.CombinedMarkdown != "never" {
		return Options{}, fmt.Errorf("invalid --combined-markdown %q", options.CombinedMarkdown)
	}
	options.Strict = strict
	options.MaxResponseBytes = int64(maxResponse)
	options.MaxSessionBytes = int64(maxSession)
	options.MaxTotalBytes = int64(maxTotal)
	options.MaxNativeRecordBytes = int64(maxRecord)
	options.AdapterRoots = map[string][]string{
		"claude": claudeRoots, "codex": codexRoots,
		"opencode": opencodeRoots, "pi": piRoots,
	}
	options.Harnesses, err = parseHarnesses(harnesses)
	if err != nil {
		return Options{}, err
	}
	options.OutputDirectory, options.FileName, options.OutputFormat, err = resolveOutput(output, options.FileName, format, formatSet)
	if err != nil {
		return Options{}, err
	}
	return options, nil
}

func resolveOutput(output, legacyFileName, format string, formatSet bool) (string, string, OutputFormat, error) {
	if strings.TrimSpace(output) == "" {
		return "", "", "", errors.New("--output cannot be empty")
	}
	selected, err := parseOutputFormat(format)
	if err != nil {
		return "", "", "", err
	}
	outputExtFormat, outputIsFile := formatFromExtension(filepath.Ext(output))
	if outputIsFile {
		if legacyFileName != "" {
			return "", "", "", errors.New("--file-name cannot be used when --output is a file")
		}
		if formatSet && selected != outputExtFormat {
			return "", "", "", fmt.Errorf("--format %s conflicts with output extension %s", selected, filepath.Ext(output))
		}
		return filepath.Dir(output), filepath.Base(output), outputExtFormat, nil
	}
	if selected == "" {
		selected = FormatZIP
	}
	if legacyFileName != "" {
		if filepath.Base(legacyFileName) != legacyFileName {
			return "", "", "", errors.New("--file-name must be a plain filename")
		}
		nameFormat, ok := formatFromExtension(filepath.Ext(legacyFileName))
		if !ok {
			return "", "", "", errors.New("--file-name must end in .zip, .md, .jsonl, or .csv")
		}
		if formatSet && selected != nameFormat {
			return "", "", "", fmt.Errorf("--format %s conflicts with --file-name extension", selected)
		}
		selected = nameFormat
	}
	return output, legacyFileName, selected, nil
}

func parseOutputFormat(value string) (OutputFormat, error) {
	switch OutputFormat(strings.ToLower(strings.TrimSpace(value))) {
	case "":
		return "", nil
	case FormatZIP, FormatMarkdown, FormatJSONL, FormatCSV:
		return OutputFormat(strings.ToLower(strings.TrimSpace(value))), nil
	default:
		return "", fmt.Errorf("invalid --format %q", value)
	}
}

func formatFromExtension(extension string) (OutputFormat, bool) {
	switch strings.ToLower(extension) {
	case ".zip":
		return FormatZIP, true
	case ".md", ".markdown":
		return FormatMarkdown, true
	case ".jsonl":
		return FormatJSONL, true
	case ".csv":
		return FormatCSV, true
	default:
		return "", false
	}
}

func ResolveRange(now time.Time, since, from, to string) (domain.TimeRange, error) {
	if from == "" {
		duration, err := parseDuration(since)
		if err == nil {
			return domain.TimeRange{From: now.Add(-duration), To: now}, nil
		}
		start, parseErr := parseTime(since, now.Location())
		if parseErr != nil {
			return domain.TimeRange{}, fmt.Errorf("invalid --since: %w", parseErr)
		}
		return domain.TimeRange{From: start, To: now}, nil
	}
	start, err := parseTime(from, now.Location())
	if err != nil {
		return domain.TimeRange{}, fmt.Errorf("invalid --from: %w", err)
	}
	end := now
	if to != "" {
		end, err = parseTime(to, now.Location())
		if err != nil {
			return domain.TimeRange{}, fmt.Errorf("invalid --to: %w", err)
		}
	}
	if end.Before(start) {
		return domain.TimeRange{}, errors.New("range end precedes range start")
	}
	return domain.TimeRange{From: start, To: end}, nil
}

func parseDuration(value string) (time.Duration, error) {
	for _, item := range []struct {
		suffix string
		unit   time.Duration
	}{{"w", 7 * 24 * time.Hour}, {"d", 24 * time.Hour}} {
		if !strings.HasSuffix(value, item.suffix) {
			continue
		}
		amount, err := strconv.ParseFloat(strings.TrimSuffix(value, item.suffix), 64)
		if err != nil || amount <= 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		result := time.Duration(amount * float64(item.unit))
		if result <= 0 {
			return 0, fmt.Errorf("invalid duration %q", value)
		}
		return result, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	return d, nil
}

func parseTime(value string, location *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.ParseInLocation(time.DateOnly, value, location)
}

func ParseSize(value string) (int64, error) {
	text := strings.TrimSpace(strings.ToUpper(value))
	multipliers := []struct {
		suffix string
		value  int64
	}{{"GIB", 1 << 30}, {"GB", 1 << 30}, {"MIB", 1 << 20}, {"MB", 1 << 20}, {"KIB", 1 << 10}, {"KB", 1 << 10}, {"B", 1}}
	for _, item := range multipliers {
		if strings.HasSuffix(text, item.suffix) {
			number := strings.TrimSpace(strings.TrimSuffix(text, item.suffix))
			numeric, err := strconv.ParseFloat(number, 64)
			if err != nil || numeric <= 0 {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			result := int64(numeric * float64(item.value))
			if result <= 0 {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			return result, nil
		}
	}
	numeric, err := strconv.ParseInt(text, 10, 64)
	if err != nil || numeric <= 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return numeric, nil
}

func parseHarnesses(value string) ([]string, error) {
	if value == "" {
		return []string{"claude", "codex", "opencode", "pi"}, nil
	}
	allowed := map[string]bool{"claude": true, "codex": true, "opencode": true, "pi": true}
	seen := map[string]bool{}
	result := []string{}
	for _, item := range strings.Split(value, ",") {
		id := strings.ToLower(strings.TrimSpace(item))
		if !allowed[id] {
			return nil, fmt.Errorf("unsupported harness %q", item)
		}
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result, nil
}

func DefaultFileName(r domain.TimeRange) string {
	return DefaultFileNameFor(r, FormatZIP)
}

func DefaultFileNameFor(r domain.TimeRange, format OutputFormat) string {
	extension := map[OutputFormat]string{FormatZIP: ".zip", FormatMarkdown: ".md", FormatJSONL: ".jsonl", FormatCSV: ".csv"}[format]
	return fmt.Sprintf("harness-chat-exporter_%s_%s%s", r.From.Format(time.DateOnly), r.To.Format(time.DateOnly), extension)
}
