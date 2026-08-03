package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/verdict"
)

type Request struct {
	Prompt             string
	Provenance         verdict.Provenance
	SnapshotDir        string
	AllowedFiles       map[string]struct{}
	SnapshotLineCounts map[string]int
	DiffLineRanges     map[string][]verdict.LineRange
}

type ReviewEngine interface {
	Review(context.Context, Request) ([]byte, error)
}

type PreflightEngine interface {
	Preflight() error
}

type Options struct {
	Command           runner.Runner
	Binary            string
	Model             string
	Thinking          string
	Heartbeat         io.Writer
	HeartbeatInterval time.Duration
}

const (
	EngineCodex  = "codex"
	EngineClaude = "claude"

	DefaultCodexModel  = "gpt-5.6-sol"
	DefaultClaudeModel = "claude-fable-5"
	DefaultCodexThink  = "high"

	defaultHeartbeatInterval = 60 * time.Second
)

// New creates one explicitly selected real engine. It never falls back to a
// different engine when the selected CLI is missing or unavailable.
func New(name string, options Options) (ReviewEngine, string, error) {
	switch name {
	case EngineCodex:
		selected := NewCodex(options)
		return selected, selected.Label(), nil
	case EngineClaude:
		selected := NewClaude(options)
		return selected, selected.Label(), nil
	default:
		return nil, "", fmt.Errorf("[ROAST-ENGINE-NAME] engine %q is unsupported; choose codex, claude, or fake", name)
	}
}

func DefaultModel(name string) string {
	switch name {
	case EngineClaude:
		return DefaultClaudeModel
	case EngineCodex:
		return DefaultCodexModel
	default:
		return ""
	}
}

func DefaultThinking(name string) string {
	if name == EngineCodex {
		return DefaultCodexThink
	}
	return ""
}

func ValidateThinking(name, thinking string) error {
	if thinking == "" {
		return nil
	}
	if name == EngineClaude {
		return nil
	}
	valid := map[string]struct{}{
		"low": {}, "medium": {}, "high": {}, "xhigh": {}, "max": {},
	}
	validValues := "low, medium, high, xhigh, or max"
	if name == EngineCodex {
		valid["none"] = struct{}{}
		valid["minimal"] = struct{}{}
		valid["ultra"] = struct{}{}
		validValues = "none, minimal, low, medium, high, xhigh, max, or ultra"
	}
	if _, ok := valid[thinking]; ok {
		return nil
	}
	return fmt.Errorf("[ROAST-ENGINE-THINKING] %q is not valid for %s; choose one of %s", thinking, name, validValues)
}

func (options Options) normalized(name string) Options {
	if options.Command == nil {
		options.Command = runner.ExecRunner{}
	}
	if options.Binary == "" {
		options.Binary = name
	}
	options.Binary = resolveEngineBinary(options.Binary)
	if options.Model == "" {
		options.Model = DefaultModel(name)
	}
	if options.Thinking == "" {
		options.Thinking = DefaultThinking(name)
	}
	if options.Heartbeat == nil {
		options.Heartbeat = io.Discard
	}
	if options.HeartbeatInterval <= 0 {
		options.HeartbeatInterval = defaultHeartbeatInterval
	}
	return options
}

func resolveEngineBinary(binary string) string {
	if filepath.IsAbs(binary) || !strings.ContainsAny(binary, "/\\") {
		return binary
	}
	absolute, err := filepath.Abs(binary)
	if err != nil {
		return binary
	}
	return absolute
}

func (options Options) label(name string) string {
	options = options.normalized(name)
	label := name + "/" + options.Model
	if options.Thinking != "" {
		label += "/" + options.Thinking
	}
	return label
}

func validateRequest(request Request) error {
	if strings.TrimSpace(request.Prompt) == "" {
		return fmt.Errorf("[ROAST-ENGINE-REQUEST] review prompt is empty; assemble the review prompt before calling the selected engine")
	}
	if request.SnapshotDir == "" {
		return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] review snapshot directory is missing; provide the extracted snapshot so the reviewer has a read-only evidence room")
	}
	info, err := os.Stat(request.SnapshotDir)
	if err != nil {
		return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot open review snapshot directory %q: %w; rebuild the bundle and retry", request.SnapshotDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] review snapshot path %q is not a directory; rebuild the bundle and retry", request.SnapshotDir)
	}
	return nil
}

type reviewCall func(context.Context, string) ([]byte, error)

const retryPromptSuffix = "\n\nYour previous response was rejected by the verdict validator. Correct this validation error and return exactly one JSON object matching the requested schema. Do not include markdown fences or commentary.\nValidator error: "

func reviewWithRetry(ctx context.Context, request Request, label string, heartbeat io.Writer, call reviewCall) ([]byte, error) {
	if err := validateRequest(request); err != nil {
		return nil, err
	}
	prompt := promptWithProvenance(request)
	basePrompt := prompt
	var validationErr error
	for attempt := 1; attempt <= 2; attempt++ {
		raw, err := call(ctx, prompt)
		if err != nil {
			return nil, err
		}
		decoded, err := decodeEngineResponse(raw, request)
		if err == nil {
			return decoded, nil
		}
		validationErr = err
		if attempt == 1 {
			writeHeartbeat(heartbeat, "roast: [ROAST-ENGINE] %s returned invalid verdict JSON; retrying once: %s\n", label, err)
			prompt = RetryPromptPrefix(basePrompt) + err.Error()
		}
	}
	return nil, fmt.Errorf("[ROAST-ENGINE-JSON] %s returned invalid verdict JSON after one retry: %w; return exactly one JSON object matching the verdict schema", label, validationErr)
}

func PromptWithProvenance(prompt string, provenance verdict.Provenance) string {
	if provenance == (verdict.Provenance{}) {
		return prompt
	}
	encoded, err := json.Marshal(provenance)
	if err != nil {
		return prompt
	}
	return prompt + "\n\nRuntime provenance contract: copy these provenance values exactly into your JSON response. Do not invent or alter any value.\n" + string(encoded)
}

func RetryPromptPrefix(prompt string) string {
	return prompt + retryPromptSuffix
}

func promptWithProvenance(request Request) string {
	if request.Provenance == (verdict.Provenance{}) {
		return request.Prompt
	}
	return PromptWithProvenance(request.Prompt, request.Provenance)
}

func decodeEngineResponse(raw []byte, request Request) ([]byte, error) {
	decode := func(candidate []byte) ([]byte, error) {
		decoded, err := verdict.Decode(candidate)
		if err != nil {
			return nil, err
		}
		if request.Provenance != (verdict.Provenance{}) && decoded.Provenance != request.Provenance {
			return nil, fmt.Errorf("[ROAST-VERDICT-PROVENANCE] verdict provenance does not describe this bundle; copy the runtime provenance contract exactly and retry")
		}
		if err := verdict.ValidateWithLocations(decoded, request.AllowedFiles, request.SnapshotLineCounts, request.DiffLineRanges); err != nil {
			return nil, err
		}
		encoded, err := verdict.Encode(decoded)
		if err != nil {
			return nil, err
		}
		return encoded, nil
	}

	decoded, rawErr := decode(raw)
	if rawErr == nil {
		return decoded, nil
	}
	if candidate, ok := knownResponseCandidate(raw); ok {
		decoded, wrappedErr := decode(candidate)
		if wrappedErr == nil {
			return decoded, nil
		}
		return nil, wrappedErr
	}
	return nil, rawErr
}

// knownResponseCandidate unwraps only the two documented CLI response
// envelopes. Codex can emit a top-level structured_output value; Claude's JSON
// mode emits an array whose single result event carries that value. Other JSON
// nesting is treated as an invalid verdict.
func knownResponseCandidate(raw []byte) ([]byte, bool) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err == nil {
		return structuredOutputCandidate(envelope, false)
	}

	var events []json.RawMessage
	if err := json.Unmarshal(raw, &events); err != nil {
		return nil, false
	}
	var candidate []byte
	for _, event := range events {
		var eventEnvelope map[string]json.RawMessage
		if err := json.Unmarshal(event, &eventEnvelope); err != nil {
			return nil, false
		}
		current, ok := structuredOutputCandidate(eventEnvelope, true)
		if !ok {
			continue
		}
		if candidate != nil {
			return nil, false
		}
		candidate = current
	}
	return candidate, candidate != nil
}

func structuredOutputCandidate(envelope map[string]json.RawMessage, requireResultType bool) ([]byte, bool) {
	candidate, ok := envelope["structured_output"]
	if !ok || len(candidate) == 0 {
		return nil, false
	}
	if rawType, exists := envelope["type"]; exists || requireResultType {
		var responseType string
		if !exists || json.Unmarshal(rawType, &responseType) != nil || responseType != "result" {
			return nil, false
		}
	}
	return append([]byte(nil), candidate...), true
}

func writeHeartbeat(writer io.Writer, format string, values ...any) {
	if writer == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, format, values...)
}

func runCommandWithHeartbeat(ctx context.Context, options Options, label, prompt, dir, program string, environment map[string]string, args ...string) (runner.Result, error) {
	stop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(options.HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeHeartbeat(options.Heartbeat, "roast: [ROAST-ENGINE] %s review still running (elapsed=%s)\n", label, time.Since(started).Truncate(time.Second))
			case <-stop:
				return
			}
		}
	}()
	defer func() {
		close(stop)
		<-heartbeatDone
	}()

	if len(environment) > 0 {
		inputEnvironmentRunner, ok := options.Command.(runner.InputEnvironmentRunner)
		if !ok {
			return runner.Result{}, environmentIsolationError(label)
		}
		return inputEnvironmentRunner.RunWithInputAndEnvironment(ctx, dir, program, prompt, environment, args...)
	}

	if inputRunner, ok := options.Command.(runner.InputRunner); ok {
		return inputRunner.RunWithInput(ctx, dir, program, prompt, args...)
	}

	// Small embedding and test runners may expose only Runner. The production
	// runner uses stdin, so this fallback is only for callers that own the seam.
	fallbackArgs := append([]string(nil), args...)
	if len(fallbackArgs) > 0 && fallbackArgs[len(fallbackArgs)-1] == "-" {
		fallbackArgs[len(fallbackArgs)-1] = prompt
	} else {
		fallbackArgs = append(fallbackArgs, prompt)
	}
	return options.Command.Run(ctx, dir, program, fallbackArgs...)
}

func runCommandWithEnvironment(ctx context.Context, command runner.Runner, label, dir, program string, environment map[string]string, args ...string) (runner.Result, error) {
	if len(environment) == 0 {
		return command.Run(ctx, dir, program, args...)
	}
	environmentRunner, ok := command.(runner.EnvironmentRunner)
	if !ok {
		return runner.Result{}, environmentIsolationError(label)
	}
	return environmentRunner.RunWithEnvironment(ctx, dir, program, environment, args...)
}

func environmentIsolationError(label string) error {
	return fmt.Errorf("[ROAST-ENGINE-ISOLATION] %s review requires a per-process environment override, but the configured command runner cannot apply one; use runner.ExecRunner or implement runner.EnvironmentRunner and runner.InputEnvironmentRunner; no reviewer was started", label)
}

func commandError(ctx context.Context, label, binary string, result runner.Result, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("[ROAST-ENGINE-CANCELED] %s review canceled: %w", label, ctxErr)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("[ROAST-ENGINE-CANCELED] %s review canceled: %w", label, err)
		}
		return fmt.Errorf("[ROAST-ENGINE-UNAVAILABLE] cannot start %s CLI %q: %v; install that CLI, authenticate it, or choose the explicitly requested other engine; roast never switches engines automatically", label, binary, err)
	}
	if result.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(result.Stdout))
	}
	detail = boundedDiagnostic(detail, 4000)
	if detail == "" {
		return fmt.Errorf("[ROAST-ENGINE-FAILED] %s CLI %q exited with status %d; inspect the CLI installation/authentication and retry the same engine", label, binary, result.ExitCode)
	}
	return fmt.Errorf("[ROAST-ENGINE-FAILED] %s CLI %q exited with status %d: %s; fix that CLI error and retry the same engine", label, binary, result.ExitCode, detail)
}

func boundedDiagnostic(detail string, maximum int) string {
	if len(detail) <= maximum {
		return detail
	}
	half := maximum / 2
	return detail[:half] + "\n...[diagnostic truncated]...\n" + detail[len(detail)-half:]
}

func writeSchemaFile(schema string) (string, func(), error) {
	file, err := os.CreateTemp("", "roast-verdict-schema-*.json")
	if err != nil {
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot create temporary verdict schema: %w; retry with a writable system temp directory", err)
	}
	name := file.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot protect temporary verdict schema: %w", err)
	}
	if _, err := file.WriteString(schema); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot write temporary verdict schema: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot close temporary verdict schema: %w", err)
	}
	return name, cleanup, nil
}

// PrepareSnapshot extracts only regular files from the bundle into a private,
// read-only directory. The cleanup function restores write permissions before
// removing it because Unix directory permissions otherwise block cleanup.
func PrepareSnapshot(snapshot []byte) (string, func(), error) {
	root, err := os.MkdirTemp("", "roast-snapshot-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot create temporary snapshot directory: %w; retry with a writable system temp directory", err)
	}
	cleanup := func() {
		makeWritable(root)
		_ = os.RemoveAll(root)
	}
	if err := extractSnapshot(root, snapshot); err != nil {
		cleanup()
		return "", func() {}, err
	}
	if err := makeReadOnly(root); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot make review snapshot read-only: %w; retry the review", err)
	}
	return root, cleanup, nil
}

func extractSnapshot(root string, snapshot []byte) error {
	archive := tar.NewReader(bytes.NewReader(snapshot))
	seen := map[string]struct{}{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot read snapshot tar: %w; rebuild the review bundle and retry", err)
		}
		if header.Typeflag == tar.TypeXHeader || header.Typeflag == tar.TypeXGlobalHeader || header.Typeflag == tar.TypeGNULongName || header.Typeflag == tar.TypeGNULongLink {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] snapshot entry %q is not a regular file; rebuild the review bundle and retry", header.Name)
		}
		name, err := safeSnapshotPath(root, header.Name)
		if err != nil {
			return err
		}
		if _, exists := seen[header.Name]; exists {
			return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] snapshot contains duplicate file %q; rebuild the review bundle and retry", header.Name)
		}
		seen[header.Name] = struct{}{}
		if err := ensureSnapshotParents(root, name); err != nil {
			return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot create parent for %q: %w", header.Name, err)
		}
		if err := writeSnapshotFile(name, archive); err != nil {
			return fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] cannot materialize file %q: %w", header.Name, err)
		}
	}
}

func ensureSnapshotParents(root, destination string) error {
	parent := filepath.Dir(destination)
	relative, err := filepath.Rel(root, parent)
	if err != nil {
		return fmt.Errorf("cannot resolve parent path: %w", err)
	}
	if relative == "." {
		return nil
	}

	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return err
		}
		exact := false
		for _, entry := range entries {
			if entry.Name() == component {
				exact = true
				break
			}
		}
		candidate := filepath.Join(current, component)
		info, statErr := os.Stat(candidate)
		switch {
		case statErr == nil:
			if !exact {
				return fmt.Errorf("directory name %q aliases an existing entry", component)
			}
			if !info.IsDir() {
				return fmt.Errorf("parent %q is not a directory", component)
			}
		case errors.Is(statErr, os.ErrNotExist):
			if err := os.Mkdir(candidate, 0o700); err != nil {
				return err
			}
		default:
			return statErr
		}
		current = candidate
	}
	return nil
}

func writeSnapshotFile(name string, content io.Reader) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("destination aliases an existing file")
		}
		return err
	}
	if _, err := io.Copy(file, content); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

func safeSnapshotPath(root, name string) (string, error) {
	if name == "" || (runtime.GOOS == "windows" && strings.Contains(name, "\\")) || path.IsAbs(name) || path.Clean(name) != name || name == "." || name == ".." || strings.HasPrefix(name, "../") {
		return "", fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] snapshot path %q is unsafe; rebuild the review bundle and retry", name)
	}
	if runtime.GOOS == "windows" {
		for _, component := range strings.Split(name, "/") {
			if !safeSnapshotComponent(component) {
				return "", fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] snapshot path %q uses a Windows-reserved or non-portable name; rebuild the review bundle and retry", name)
			}
		}
	}
	destination := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, destination)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("[ROAST-ENGINE-SNAPSHOT] snapshot path %q escapes the review directory; rebuild the review bundle and retry", name)
	}
	return destination, nil
}

func safeSnapshotComponent(component string) bool {
	if component == "" || component == "." || component == ".." || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return false
	}
	if strings.ContainsAny(component, "<>:\"|?*") {
		return false
	}
	for _, character := range component {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	normalized := strings.ToUpper(strings.TrimRight(component, " ."))
	if dot := strings.IndexByte(normalized, '.'); dot >= 0 {
		normalized = normalized[:dot]
	}
	switch normalized {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		"COM\u00b9", "COM\u00b2", "COM\u00b3", "LPT\u00b9", "LPT\u00b2", "LPT\u00b3":
		return false
	default:
		return true
	}
}

func makeReadOnly(root string) error {
	return filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(name, 0o500)
		}
		return os.Chmod(name, 0o400)
	})
}

func makeWritable(root string) {
	_ = filepath.Walk(root, func(name string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			_ = os.Chmod(name, 0o700)
		} else {
			_ = os.Chmod(name, 0o600)
		}
		return nil
	})
}
