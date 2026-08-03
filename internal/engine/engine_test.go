package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

	programs      []string
	dirs          []string
	args          [][]string
	inputs        []string
	environments  []map[string]string
	sessionAction func()
}

func (command *scriptedRunner) Run(ctx context.Context, dir, program string, args ...string) (runner.Result, error) {
	command.record(dir, program, "", nil, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInput(ctx context.Context, dir, program, input string, args ...string) (runner.Result, error) {
	command.record(dir, program, input, nil, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInputStream(ctx context.Context, dir string, input []byte, _ io.Writer, program string, args ...string) (runner.Result, error) {
	command.record(dir, program, string(input), nil, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithEnvironment(ctx context.Context, dir, program string, environment map[string]string, args ...string) (runner.Result, error) {
	command.record(dir, program, "", environment, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInputAndEnvironment(ctx context.Context, dir, program, input string, environment map[string]string, args ...string) (runner.Result, error) {
	command.record(dir, program, input, environment, args)
	return command.next(ctx)
}

func (command *scriptedRunner) RunWithInputAndEnvironmentUntil(ctx context.Context, dir, program, input string, environment map[string]string, stop func([]byte) bool, args ...string) (runner.Result, error) {
	command.record(dir, program, input, environment, args)
	if command.sessionAction != nil {
		command.sessionAction()
	}
	result, err := command.next(ctx)
	if err != nil {
		return result, err
	}
	for _, line := range bytes.Split(result.Stdout, []byte("\n")) {
		if len(line) > 0 && stop(line) {
			break
		}
	}
	return result, nil
}

func (command *scriptedRunner) record(dir, program, input string, environment map[string]string, args []string) {
	command.dirs = append(command.dirs, dir)
	command.programs = append(command.programs, program)
	command.args = append(command.args, append([]string(nil), args...))
	command.inputs = append(command.inputs, input)
	command.environments = append(command.environments, cloneEnvironment(environment))
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

func (command *runnerOnly) RunWithEnvironment(ctx context.Context, _ string, program string, _ map[string]string, args ...string) (runner.Result, error) {
	return command.Run(ctx, "", program, args...)
}

func (command *runnerOnly) RunWithInputAndEnvironment(_ context.Context, _ string, _ string, input string, _ map[string]string, args ...string) (runner.Result, error) {
	command.args = append([]string(nil), args...)
	if len(command.args) > 0 && command.args[len(command.args)-1] == "-" {
		command.args[len(command.args)-1] = input
	} else {
		command.args = append(command.args, input)
	}
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

func TestStageCodexHomeCopiesOnlyAuthentication(t *testing.T) {
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte(`{"access_token":"fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, "AGENTS.md"), []byte("operator instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexHomeVariable, sourceHome)

	stage, err := stageCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	stagedHome := stage.environment[codexHomeVariable]
	if stagedHome == "" || stagedHome == sourceHome {
		t.Fatalf("staged environment = %#v, want a separate CODEX_HOME", stage.environment)
	}
	entries, err := os.ReadDir(stagedHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("staged CODEX_HOME entries = %#v, want only auth.json", entries)
	}
	auth, err := os.ReadFile(filepath.Join(stagedHome, "auth.json"))
	if err != nil || string(auth) != `{"access_token":"fixture"}` {
		t.Fatalf("staged auth = %q, err = %v", auth, err)
	}
	info, err := os.Stat(filepath.Join(stagedHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("staged auth permissions = %o, want 600", info.Mode().Perm())
	}
	if sourceAuth, err := os.ReadFile(filepath.Join(sourceHome, "auth.json")); err != nil || string(sourceAuth) != `{"access_token":"fixture"}` {
		t.Fatalf("source auth after staging = %q, err = %v", sourceAuth, err)
	}
	stage.cleanup()
	if _, err := os.Stat(stagedHome); !os.IsNotExist(err) {
		t.Fatalf("staged CODEX_HOME still exists after cleanup: %v", err)
	}
}

func TestStageCodexHomeSupportsSymlinkedAuthentication(t *testing.T) {
	sourceHome := t.TempDir()
	realAuth := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(realAuth, []byte(`{"access_token":"fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAuth, filepath.Join(sourceHome, "auth.json")); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	t.Setenv(codexHomeVariable, sourceHome)
	stage, err := stageCodexHome()
	if err != nil {
		t.Fatal(err)
	}
	defer stage.cleanup()
	if auth, err := os.ReadFile(filepath.Join(stage.environment[codexHomeVariable], "auth.json")); err != nil || string(auth) != `{"access_token":"fixture"}` {
		t.Fatalf("staged symlink auth = %q, err = %v", auth, err)
	}
	info, err := os.Lstat(filepath.Join(sourceHome, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("Codex auth symlink was changed")
	}
}

func TestCodexAuthForIsolatedHomeRemovesRefreshToken(t *testing.T) {
	source := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"access","refresh_token":"refresh","account_id":"account"}}`)
	isolated, err := codexAuthForIsolatedHome(source)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(isolated, &document); err != nil {
		t.Fatal(err)
	}
	if document.Tokens["access_token"] != "access" || document.Tokens["account_id"] != "account" || document.Tokens["refresh_token"] != "" {
		t.Fatalf("isolated auth tokens = %#v, want access/account preserved and refresh empty", document.Tokens)
	}
	if string(source) == string(isolated) {
		t.Fatal("isolated auth unexpectedly retained the source refresh token")
	}
}

func TestStageCodexHomeRefreshesExpiringAuthWithNativeCodex(t *testing.T) {
	sourceHome := t.TempDir()
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	t.Setenv("CODEX_API_KEY", "")
	expiringToken := codexTestJWT(time.Now().Add(time.Minute))
	refreshedToken := codexTestJWT(time.Now().Add(time.Hour))
	expiringAuth := []byte(fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":"fixture-refresh"}}`, expiringToken))
	refreshedAuth := []byte(fmt.Sprintf(`{"tokens":{"access_token":%q,"refresh_token":"new-refresh"}}`, refreshedToken))
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), expiringAuth, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexHomeVariable, sourceHome)
	commands := &scriptedRunner{
		sessionAction: func() {
			if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), refreshedAuth, 0o600); err != nil {
				t.Fatal(err)
			}
		},
		results: []runner.Result{{Stdout: []byte(`{"id":2,"result":{"account":{"type":"chatgpt"}}}`)}},
	}

	stage, err := stageCodexHomeForReview(context.Background(), Options{Command: commands, Binary: "codex-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.cleanup()
	if len(commands.args) != 1 || len(commands.args[0]) < 2 || commands.args[0][0] != "app-server" || commands.args[0][1] != "--listen" {
		t.Fatalf("native refresh args = %v", commands.args)
	}
	if commands.environments[0][codexHomeVariable] != sourceHome {
		t.Fatalf("native refresh environment = %#v, want source CODEX_HOME", commands.environments[0])
	}
	stagedAuth, err := os.ReadFile(filepath.Join(stage.home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tokens map[string]string `json:"tokens"`
	}
	if err := json.Unmarshal(stagedAuth, &document); err != nil {
		t.Fatal(err)
	}
	if document.Tokens["access_token"] != refreshedToken || document.Tokens["refresh_token"] != "" {
		t.Fatalf("staged refreshed auth = %#v, want refreshed access token and empty refresh token", document.Tokens)
	}
}

func TestStageCodexHomeUsesEnvironmentAuthWithoutReadingAuthFile(t *testing.T) {
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexHomeVariable, sourceHome)
	t.Setenv("CODEX_ACCESS_TOKEN", "at-fixture")
	t.Setenv("CODEX_API_KEY", "")

	stage, err := stageCodexHomeForReview(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer stage.cleanup()
	entries, err := os.ReadDir(stage.home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("environment-auth staged home entries = %#v, want empty", entries)
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
	sourceHome := testCodexHome(t)
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
	if len(commands.environments) != 4 {
		t.Fatalf("Codex environments = %d, want one isolated environment per invocation", len(commands.environments))
	}
	for index, environment := range commands.environments {
		stagedHome := environment[codexHomeVariable]
		if stagedHome == "" || stagedHome == sourceHome {
			t.Fatalf("Codex environment %d = %#v, want a private CODEX_HOME", index, environment)
		}
	}
}

func TestCodexReviewRejectsUnsupportedFeatureOverride(t *testing.T) {
	snapshotDir := t.TempDir()
	testCodexHome(t)
	commands := &scriptedRunner{results: []runner.Result{
		{ExitCode: 1, Stderr: []byte("unknown field features.multi_agent")},
	}}
	selected := NewCodex(Options{Command: commands, Binary: "codex-test"})
	if _, err := selected.Review(context.Background(), Request{Prompt: "review this", SnapshotDir: snapshotDir}); err == nil || !strings.Contains(err.Error(), "ROAST-ENGINE-CAPABILITY") {
		t.Fatalf("unsupported Codex capability error = %v", err)
	}
}

func TestCodexReviewAcceptsLegacyAgentConfig(t *testing.T) {
	testCodexHome(t)
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

func TestClaudeArgumentsDisableUserInstructions(t *testing.T) {
	arguments := NewClaude(Options{Binary: "claude-test"}).arguments(t.TempDir())
	if !hasArgument(arguments, "--safe-mode") {
		t.Fatalf("Claude arguments missing native CLAUDE.md isolation: %v", arguments)
	}
	settingSources := argumentIndex(arguments, "--setting-sources")
	if settingSources < 0 || settingSources+1 >= len(arguments) || arguments[settingSources+1] != "" {
		t.Fatalf("Claude arguments load user setting sources: %v", arguments)
	}
	for _, argument := range []string{"--system-prompt", "--append-system-prompt", "--settings"} {
		if hasArgument(arguments, argument) {
			t.Fatalf("Claude arguments explicitly load custom instructions with %q: %v", argument, arguments)
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
	testCodexHome(t)
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
	testCodexHome(t)
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

func testCodexHome(t *testing.T) string {
	t.Helper()
	t.Setenv("CODEX_ACCESS_TOKEN", "")
	t.Setenv("CODEX_API_KEY", "")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"test":"auth"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexHomeVariable, home)
	return home
}

func codexTestJWT(expiresAt time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiresAt.Unix())))
	return "header." + payload + ".signature"
}

func cloneEnvironment(environment map[string]string) map[string]string {
	if environment == nil {
		return nil
	}
	clone := make(map[string]string, len(environment))
	for name, value := range environment {
		clone[name] = value
	}
	return clone
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
var _ runner.InputEnvironmentSessionRunner = (*scriptedRunner)(nil)
