package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/verdict"
)

const validVerdict = "{\"overall\":\"well_done\",\"findings\":[],\"provenance\":{\"target\":\"HEAD\",\"branch\":\"main\",\"tree\":\"abc\",\"engine\":\"test\",\"context\":\"snapshot\"}}"

type scriptedRunner struct {
	results []runner.Result
	err     error
	delay   time.Duration

	programs []string
	dirs     []string
	args     [][]string
	inputs   []string
}

func (command *scriptedRunner) Run(ctx context.Context, dir, program string, args ...string) (runner.Result, error) {
	command.record(dir, program, "", args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInput(ctx context.Context, dir, program, input string, args ...string) (runner.Result, error) {
	command.record(dir, program, input, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInputStream(ctx context.Context, dir string, input []byte, _ io.Writer, program string, args ...string) (runner.Result, error) {
	command.record(dir, program, string(input), args)
	return command.next(ctx)
}

func (command *scriptedRunner) record(dir, program, input string, args []string) {
	command.dirs = append(command.dirs, dir)
	command.programs = append(command.programs, program)
	command.args = append(command.args, append([]string(nil), args...))
	command.inputs = append(command.inputs, input)
}

func (command *scriptedRunner) next(ctx context.Context) (runner.Result, error) {
	if command.delay > 0 {
		timer := time.NewTimer(command.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return runner.Result{}, ctx.Err()
		}
	}
	if command.err != nil {
		return runner.Result{}, command.err
	}
	if len(command.results) == 0 {
		return runner.Result{}, errors.New("scripted runner ran out of results")
	}
	result := command.results[0]
	command.results = command.results[1:]
	return result, nil
}

type runnerOnly struct {
	probeResult       runner.Result
	legacyProbeResult runner.Result
	result            runner.Result
	args              []string
	calls             int
}

func (command *runnerOnly) Run(_ context.Context, _ string, _ string, args ...string) (runner.Result, error) {
	command.calls++
	command.args = append([]string(nil), args...)
	if command.calls <= 2 {
		return command.probeResult, nil
	}
	if command.calls == 3 {
		return command.legacyProbeResult, nil
	}
	return command.result, nil
}

func (command *runnerOnly) RunWithInputStream(_ context.Context, _ string, _ []byte, _ io.Writer, _ string, args ...string) (runner.Result, error) {
	command.args = append([]string(nil), args...)
	return command.result, nil
}

func TestCodexArgumentsUseNativeIsolationFlags(t *testing.T) {
	snapshotDir := t.TempDir()
	selected := NewCodex(Options{Binary: "codex-test", Model: "model-test", Thinking: "high"})
	arguments := selected.arguments(snapshotDir, "schema.json", true)

	for _, expected := range []string{
		"exec", "--model", "model-test", "--ignore-user-config", "--ignore-rules",
		"--strict-config", "--skip-git-repo-check", "--ephemeral", "--output-schema",
		"schema.json", "--cd", snapshotDir, "features.hooks=false",
		"features.plugins=false", "features.apps=false", "features.multi_agent=false",
		"features.multi_agent_v2=false", "skills.include_instructions=false",
		"mcp_servers={}",
		"web_search=\"disabled\"", "default_permissions=\"autoreview\"",
		"permissions.autoreview.filesystem={\":minimal\"=\"read\",\":workspace_roots\"=\"read\"}",
		"agents.enabled=false",
	} {
		if !hasArgument(arguments, expected) {
			t.Fatalf("Codex arguments missing %q: %v", expected, arguments)
		}
	}
	if hasArgument(arguments, "skills.bundled.enabled=false") {
		t.Fatalf("Codex arguments must not delete the shared system-skills cache: %v", arguments)
	}
	if !hasArgumentPrefix(arguments, "projects.") {
		t.Fatalf("Codex arguments missing per-snapshot trust boundary: %v", arguments)
	}
	for _, argument := range arguments {
		lower := strings.ToLower(argument)
		for _, forbidden := range []string{"sandbox", "managed", "auth", "runtime", "allow_unmanaged"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("Codex argument %q retained removed isolation machinery", argument)
			}
		}
	}
}

func TestCodexSchemaAddsStrictSuggestionRequirement(t *testing.T) {
	schema, err := codexSchema()
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Properties map[string]struct {
			Items struct {
				Required []string `json:"required"`
			} `json:"items"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(schema), &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range document.Properties["findings"].Items.Required {
		if field == "suggestion" {
			return
		}
	}
	t.Fatalf("Codex schema does not require suggestion: %s", strings.TrimSpace(schema))
}

func TestCodexReviewRunsInSnapshotWithPromptOnStdin(t *testing.T) {
	snapshotDir := t.TempDir()
	commands := &scriptedRunner{results: []runner.Result{
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("invalid type: boolean `false`, expected struct AgentRoleToml in `agents`")},
		{Stdout: []byte(validVerdict)},
	}}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})

	response, err := selected.Review(context.Background(), Request{Prompt: "review this", SnapshotDir: snapshotDir})
	if err != nil {
		t.Fatal(err)
	}
	if string(response) == "" || len(commands.programs) != 4 || commands.programs[3] != "codex-test" || commands.dirs[3] != snapshotDir || commands.inputs[3] != "review this" {
		t.Fatalf("Codex invocation = programs %q dirs %q inputs %q response %q", commands.programs, commands.dirs, commands.inputs, response)
	}
}

func TestCodexReviewRejectsUnsupportedFeatureOverride(t *testing.T) {
	snapshotDir := t.TempDir()
	commands := &scriptedRunner{results: []runner.Result{
		{ExitCode: 1, Stderr: []byte("unknown field features.multi_agent")},
	}}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})
	if _, err := selected.Review(context.Background(), Request{Prompt: "review this", SnapshotDir: snapshotDir}); err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-CAPABILITY") {
		t.Fatalf("unsupported Codex capability error = %v", err)
	}
}

func TestCodexReviewAcceptsLegacyAgentConfig(t *testing.T) {
	commands := &scriptedRunner{results: []runner.Result{
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("invalid type: boolean `false`, expected struct AgentRoleToml in `agents`")},
		{Stdout: []byte(validVerdict)},
	}}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})
	if _, err := selected.Review(context.Background(), Request{Prompt: "review this", SnapshotDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	for _, argument := range commands.args[3] {
		if argument == "agents.enabled=false" {
			t.Fatalf("legacy Codex arguments retained unsupported agents override: %v", commands.args[3])
		}
	}
}

func TestClaudeArgumentsUseNativeReadPermissions(t *testing.T) {
	snapshotDir := t.TempDir()
	selected := NewClaude(Options{Binary: "claude-test"})
	arguments := selected.arguments(snapshotDir)

	for _, expected := range []string{
		"--safe-mode", "--setting-sources", "", "--strict-mcp-config",
		"--disallowedTools", "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch,mcp__*",
		"--tools", "Read,Grep,Glob", "--allowedTools", "--permission-mode", "dontAsk",
		"--disable-slash-commands", "--no-session-persistence", "--output-format", "json",
		"--json-schema", verdict.Schema(), "--model", DefaultClaudeModel,
	} {
		if !hasArgument(arguments, expected) {
			t.Fatalf("Claude arguments missing %q: %v", expected, arguments)
		}
	}
	rules := claudeReadRules(snapshotDir)
	if !strings.HasPrefix(rules[0], "Read(//") {
		t.Fatalf("Claude absolute permission rule = %q, want // path syntax", rules[0])
	}
	allowedIndex := argumentIndex(arguments, "--allowedTools")
	if allowedIndex < 0 || allowedIndex+1+len(rules) > len(arguments) {
		t.Fatalf("Claude arguments omitted native read permissions: %v", arguments)
	}
	for index, rule := range rules {
		if arguments[allowedIndex+1+index] != rule {
			t.Fatalf("Claude read rule %q at index %d, want %q: %v", arguments[allowedIndex+1+index], index, rule, arguments)
		}
	}
	for _, argument := range arguments {
		lower := strings.ToLower(argument)
		for _, forbidden := range []string{"sandbox-exec", "managed", "auth", "credential", "remote_settings", "allow_unmanaged"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("Claude argument %q retained removed isolation machinery", argument)
			}
		}
	}
}

func TestClaudeReviewRunsInSnapshotWithPromptOnStdin(t *testing.T) {
	snapshotDir := t.TempDir()
	commands := &scriptedRunner{results: []runner.Result{{Stdout: []byte(validVerdict)}}}
	selected := NewClaude(Options{Command: commands, Binary: "claude-test"})

	response, err := selected.Review(context.Background(), Request{Prompt: "review this", SnapshotDir: snapshotDir})
	if err != nil {
		t.Fatal(err)
	}
	if string(response) == "" || commands.programs[0] != "claude-test" || commands.dirs[0] != snapshotDir || commands.inputs[0] != "review this" {
		t.Fatalf("Claude invocation = programs %q dirs %q inputs %q response %q", commands.programs, commands.dirs, commands.inputs, response)
	}
}

func TestPreflightOnlyChecksSelectedBinary(t *testing.T) {
	for _, newEngine := range []func(Options) PreflightEngine{
		func(options Options) PreflightEngine { return NewCodex(options) },
		func(options Options) PreflightEngine { return NewClaude(options) },
	} {
		selected := newEngine(Options{Binary: os.Args[0]})
		if err := selected.Preflight(); err != nil {
			t.Fatalf("preflight rejected the current platform or environment: %v", err)
		}
		if err := newEngine(Options{Binary: "roast-binary-that-is-not-installed"}).Preflight(); err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-UNAVAILABLE") {
			t.Fatalf("missing binary preflight error = %v", err)
		}
	}
}

func TestKnownEngineResponseEnvelopesOnly(t *testing.T) {
	request := Request{}
	if _, err := decodeEngineResponse([]byte(validVerdict), request); err != nil {
		t.Fatalf("direct verdict rejected: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{
		"type":              "result",
		"structured_output": json.RawMessage(validVerdict),
		"result":            "ignored text",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEngineResponse(envelope, request); err != nil {
		t.Fatalf("Claude result envelope rejected: %v", err)
	}
	arrayEnvelope, err := json.Marshal([]map[string]any{
		{"type": "system", "subtype": "init"},
		{"type": "result", "structured_output": json.RawMessage(validVerdict)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEngineResponse(arrayEnvelope, request); err != nil {
		t.Fatalf("Claude JSON event array rejected: %v", err)
	}
	nested, err := json.Marshal(map[string]any{"output": map[string]any{"overall": "well_done"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeEngineResponse(nested, request); err == nil {
		t.Fatal("generic nested JSON was accepted as a verdict")
	}
}

func TestEngineRetriesInvalidJSONWithValidatorFeedback(t *testing.T) {
	commands := &scriptedRunner{results: []runner.Result{
		{Stdout: []byte("not json")},
		{Stdout: []byte(validVerdict)},
	}}
	selected := NewClaude(Options{Command: commands, Binary: "claude-test"})

	response, err := selected.Review(context.Background(), Request{Prompt: "original prompt", SnapshotDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands.inputs) != 2 || !strings.Contains(commands.inputs[1], "Validator error: [ROAST-VERDICT-JSON]") || !strings.Contains(commands.inputs[1], "original prompt") {
		t.Fatalf("retry inputs = %q", commands.inputs)
	}
	if !bytes.Contains(response, []byte("\"overall\": \"well_done\"")) {
		t.Fatalf("response = %s", response)
	}
}

func TestEngineStopsAfterOneInvalidJSONRetry(t *testing.T) {
	commands := &scriptedRunner{results: []runner.Result{
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		{ExitCode: 1, Stderr: []byte("invalid type: boolean `false`, expected struct AgentRoleToml in `agents`")},
		{Stdout: []byte("first")},
		{Stdout: []byte("second")},
		{Stdout: []byte(validVerdict)},
	}}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})

	_, err := selected.Review(context.Background(), Request{Prompt: "review", SnapshotDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-JSON") || !strings.Contains(err.Error(), "after one retry") {
		t.Fatalf("error = %v", err)
	}
	if len(commands.programs) != 5 || len(commands.inputs) != 5 || commands.inputs[0] != "" || commands.inputs[1] != "" || commands.inputs[2] != "" {
		t.Fatalf("calls = programs %v inputs %q, want three capability probes and exactly two review calls", commands.programs, commands.inputs)
	}
}

func TestEngineEmitsHeartbeatDuringLongCall(t *testing.T) {
	commands := &scriptedRunner{delay: 35 * time.Millisecond, results: []runner.Result{{Stdout: []byte(validVerdict)}}}
	var heartbeat bytes.Buffer
	selected := NewClaude(Options{
		Command:           commands,
		Binary:            "claude-test",
		Heartbeat:         &heartbeat,
		HeartbeatInterval: 5 * time.Millisecond,
	})

	if _, err := selected.Review(context.Background(), Request{Prompt: "review", SnapshotDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(heartbeat.String(), "claude review still running") {
		t.Fatalf("heartbeat = %q", heartbeat.String())
	}
}

func TestEngineUnavailableDoesNotSwitchEngines(t *testing.T) {
	commands := &scriptedRunner{err: errors.New("executable file not found")}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})

	_, err := selected.Review(context.Background(), Request{Prompt: "review", SnapshotDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-UNAVAILABLE") || !strings.Contains(err.Error(), "never switches engines") {
		t.Fatalf("error = %v", err)
	}
	if len(commands.programs) != 1 || len(commands.inputs) != 1 || commands.inputs[0] != "" {
		t.Fatalf("calls = programs %v inputs %d, unavailable engines must not be retried or replaced", commands.programs, len(commands.inputs))
	}
}

func TestPrepareSnapshotIsReadOnlyAndCleansUp(t *testing.T) {
	root, cleanup, err := PrepareSnapshot(tarSnapshot(t, map[string]string{"README.md": "read me\n", "internal/example.go": "package example\n"}))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil || string(content) != "read me\n" {
		t.Fatalf("README = %q, err = %v", content, err)
	}
	info, err := os.Stat(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("snapshot file is writable: %o", info.Mode().Perm())
	}
	cleanup()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot root still exists after cleanup: %v", err)
	}
}

func TestPrepareSnapshotRejectsUnsafePath(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute"} {
		_, cleanup, err := PrepareSnapshot(tarSnapshot(t, map[string]string{name: "secret"}))
		cleanup()
		if err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-SNAPSHOT") {
			t.Fatalf("path %q error = %v", name, err)
		}
	}
}

func TestEngineDefaults(t *testing.T) {
	_, label, err := New(EngineCodex, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if label != "codex/gpt-5.6-sol/high" {
		t.Fatalf("Codex label = %q", label)
	}
	_, label, err = New(EngineClaude, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if label != "claude/claude-fable-5" {
		t.Fatalf("Claude label = %q", label)
	}
}

func TestRunnerOnlyFallbackUsesPositionalPrompt(t *testing.T) {
	commands := &runnerOnly{
		probeResult:       runner.Result{ExitCode: 1, Stderr: []byte("Failed to read output schema file")},
		legacyProbeResult: runner.Result{ExitCode: 1, Stderr: []byte("invalid type: boolean `false`, expected struct AgentRoleToml in `agents`")},
		result:            runner.Result{Stdout: []byte(validVerdict)},
	}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})

	if _, err := selected.Review(context.Background(), Request{Prompt: "legacy prompt", SnapshotDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(commands.args) == 0 || commands.args[len(commands.args)-1] != "legacy prompt" {
		t.Fatalf("fallback args = %v", commands.args)
	}
	if hasArgument(commands.args, "-") {
		t.Fatalf("fallback retained stdin sentinel: %v", commands.args)
	}
}

func hasArgument(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if argument == expected {
			return true
		}
	}
	return false
}

func hasArgumentPrefix(arguments []string, expected string) bool {
	for _, argument := range arguments {
		if strings.HasPrefix(argument, expected) {
			return true
		}
	}
	return false
}

func argumentIndex(arguments []string, expected string) int {
	for index, argument := range arguments {
		if argument == expected {
			return index
		}
	}
	return -1
}

func tarSnapshot(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	for name, content := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

var _ runner.Runner = (*scriptedRunner)(nil)
var _ runner.InputRunner = (*scriptedRunner)(nil)
