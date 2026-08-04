package engine

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/verdict"
)

type CodexEngine struct {
	options Options
}

const codexHomeVariable = "CODEX_HOME"

const codexAuthRefreshWindow = 10 * time.Minute

type codexHomeStage struct {
	environment map[string]string
	home        string
}

func NewCodex(options Options) *CodexEngine {
	return &CodexEngine{options: options.normalized(EngineCodex)}
}

func (engine *CodexEngine) Label() string {
	return engine.options.label(EngineCodex)
}

func (engine *CodexEngine) Preflight() error {
	if _, err := exec.LookPath(engine.options.Binary); err != nil {
		return fmt.Errorf("[ROAST-ENGINE-UNAVAILABLE] cannot locate Codex CLI %q; install it or set ROAST_CODEX_BINARY to an executable path; no other engine will be selected: %w", engine.options.Binary, err)
	}
	return nil
}

func (engine *CodexEngine) Review(ctx context.Context, request Request) ([]byte, error) {
	if err := validatePromptBudget(request); err != nil {
		return nil, err
	}
	if err := ValidateThinking(EngineCodex, engine.options.Thinking); err != nil {
		return nil, err
	}
	schema, err := codexSchema()
	if err != nil {
		return nil, err
	}
	schemaPath, cleanup, err := writeSchemaFile(schema)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	stage, err := stageCodexHomeForReview(ctx, engine.options)
	if err != nil {
		return nil, err
	}
	defer stage.cleanup()
	for _, override := range []string{"features.multi_agent=false", "features.multi_agent_v2=false"} {
		if _, err := engine.requireConfigOverride(ctx, override, stage.environment); err != nil {
			return nil, err
		}
	}
	agentsEnabled, err := engine.requireConfigOverride(ctx, "agents.enabled=false", stage.environment)
	if err != nil {
		return nil, err
	}

	call := func(ctx context.Context, prompt string) ([]byte, error) {
		result, runErr := runCommandWithHeartbeat(
			ctx,
			engine.options,
			EngineCodex,
			prompt,
			request.SnapshotDir,
			engine.options.Binary,
			stage.environment,
			engine.arguments(request.SnapshotDir, schemaPath, agentsEnabled)...,
		)
		if commandErr := commandError(ctx, EngineCodex, engine.options.Binary, result, runErr); commandErr != nil {
			return nil, commandErr
		}
		return result.Stdout, nil
	}
	return reviewWithRetry(ctx, request, engine.Label(), engine.options.Heartbeat, call)
}

func (engine *CodexEngine) requireConfigOverride(ctx context.Context, override string, environment map[string]string) (bool, error) {
	probeDir, err := os.MkdirTemp("", "roast-codex-capability-*")
	if err != nil {
		return false, fmt.Errorf("[ROAST-ENGINE-CAPABILITY] cannot create private Codex capability probe directory: %w; retry with a writable system temp directory", err)
	}
	defer os.RemoveAll(probeDir)
	probeSchema := filepath.Join(probeDir, "missing-schema.json")
	result, err := runCommandWithEnvironment(
		ctx,
		engine.options.Command,
		EngineCodex,
		"",
		engine.options.Binary,
		environment,
		"exec",
		"--ignore-user-config",
		"--strict-config",
		"--ephemeral",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--color", "never",
		"--cd", probeDir,
		"-c", override,
		"--output-schema", probeSchema,
		"roast capability probe",
	)
	if err != nil {
		return false, commandError(ctx, EngineCodex, engine.options.Binary, result, err)
	}
	diagnostic := strings.ToLower(string(result.Stderr))
	if override == "agents.enabled=false" && strings.Contains(diagnostic, "expected struct agentroletoml") {
		// Codex 0.144.x predates the unified agents.enabled setting. Its
		// multi-agent selector is feature-driven, so the two feature probes above
		// are the complete native disablement contract for that CLI generation.
		return false, nil
	}
	if !strings.Contains(diagnostic, "failed to read output schema file") {
		return false, fmt.Errorf("[ROAST-ENGINE-CAPABILITY] Codex CLI %q does not support required native setting %q; upgrade Codex and retry; roast will not run an unisolated review or switch engines", engine.options.Binary, override)
	}
	return true, nil
}

func stageCodexHome() (codexHomeStage, error) {
	sourceHome, err := codexHome()
	if err != nil {
		return codexHomeStage{}, err
	}
	return stageCodexHomeFromSource(sourceHome)
}

func stageCodexHomeForReview(ctx context.Context, options Options) (codexHomeStage, error) {
	if codexEnvironmentAuthAvailable() {
		return stageCodexHomeWithoutAuth()
	}
	sourceHome, err := codexHome()
	if err != nil {
		return codexHomeStage{}, err
	}
	auth, err := readCodexAuth(filepath.Join(sourceHome, "auth.json"))
	if err != nil {
		return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot inspect Codex authentication: %w; ensure auth.json is readable or use an environment-based Codex login; no reviewer was started", err)
	}
	needsRefresh, err := codexAuthNeedsRefresh(auth, time.Now())
	if err != nil {
		return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot inspect Codex authentication expiry: %w; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started", err)
	}
	if needsRefresh {
		if err := refreshCodexAuth(ctx, options, sourceHome); err != nil {
			return codexHomeStage{}, err
		}
		refreshedAuth, err := readCodexAuth(filepath.Join(sourceHome, "auth.json"))
		if err != nil {
			return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] Codex reported a refreshed login, but roast could not reread auth.json: %w; run `codex login` and retry; no reviewer was started", err)
		}
		stillNeedsRefresh, err := codexAuthNeedsRefresh(refreshedAuth, time.Now())
		if err != nil || stillNeedsRefresh {
			return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] Codex did not leave a usable access token in auth.json; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started")
		}
	}
	return stageCodexHomeFromSource(sourceHome)
}

func stageCodexHomeFromSource(sourceHome string) (codexHomeStage, error) {
	stage, err := newCodexHomeStage()
	if err != nil {
		return codexHomeStage{}, err
	}
	authSource := filepath.Join(sourceHome, "auth.json")
	authDestination := filepath.Join(stage.home, "auth.json")
	if err := copyCodexAuth(authSource, authDestination); err != nil {
		stage.cleanup()
		return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot stage Codex authentication from %q: %w; ensure auth.json is readable or use an environment-based Codex login; no reviewer was started", authSource, err)
	}
	return stage, nil
}

func stageCodexHomeWithoutAuth() (codexHomeStage, error) {
	return newCodexHomeStage()
}

func newCodexHomeStage() (codexHomeStage, error) {
	stagedHome, err := os.MkdirTemp("", "roast-codex-home-*")
	if err != nil {
		return codexHomeStage{}, fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot create private Codex HOME: %w; retry with a writable system temp directory; no reviewer was started", err)
	}
	return codexHomeStage{
		environment: map[string]string{codexHomeVariable: stagedHome},
		home:        stagedHome,
	}, nil
}

func codexEnvironmentAuthAvailable() bool {
	for _, name := range []string{"CODEX_ACCESS_TOKEN", "CODEX_API_KEY"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func (stage codexHomeStage) cleanup() {
	_ = os.RemoveAll(stage.home)
}

func codexHome() (string, error) {
	configured := strings.TrimSpace(os.Getenv(codexHomeVariable))
	if configured == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot determine the user's Codex HOME: %w; set CODEX_HOME to the real Codex home and retry; no reviewer was started", err)
		}
		configured = filepath.Join(home, ".codex")
	}
	absolute, err := filepath.Abs(configured)
	if err != nil {
		return "", fmt.Errorf("[ROAST-ENGINE-ISOLATION] cannot resolve CODEX_HOME %q: %w; set CODEX_HOME to an absolute path and retry; no reviewer was started", configured, err)
	}
	return filepath.Clean(absolute), nil
}

func copyCodexAuth(source, destination string) error {
	content, err := readCodexAuth(source)
	if err != nil {
		return err
	}
	if content == nil {
		return nil
	}
	content, err = codexAuthForIsolatedHome(content)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, content, 0o600)
}

func readCodexAuth(source string) ([]byte, error) {
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("auth.json is not a regular file")
	}
	return os.ReadFile(source)
}

func codexAuthNeedsRefresh(content []byte, now time.Time) (bool, error) {
	if len(content) == 0 {
		return false, nil
	}
	var document struct {
		Tokens *struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return false, fmt.Errorf("auth.json is not valid JSON: %w", err)
	}
	if document.Tokens == nil || strings.TrimSpace(document.Tokens.RefreshToken) == "" {
		return false, nil
	}
	accessToken := strings.TrimSpace(document.Tokens.AccessToken)
	if accessToken == "" {
		return true, nil
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		return true, nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
	}
	if err != nil {
		return true, nil
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 {
		return true, nil
	}
	return !now.Add(codexAuthRefreshWindow).Before(time.Unix(claims.ExpiresAt, 0)), nil
}

func refreshCodexAuth(ctx context.Context, options Options, sourceHome string) error {
	sessionRunner, ok := options.Command.(runner.InputEnvironmentSessionRunner)
	if !ok {
		return fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] Codex's cached access token is expiring, but the configured command runner cannot use Codex's native app-server auth refresh; use runner.ExecRunner or implement runner.InputEnvironmentSessionRunner, then retry; no reviewer was started")
	}
	input := strings.Join([]string{
		`{"method":"initialize","id":1,"params":{"clientInfo":{"name":"roast","version":"1"},"capabilities":{}}}`,
		`{"method":"initialized"}`,
		`{"method":"account/read","id":2,"params":{"refreshToken":true}}`,
	}, "\n") + "\n"
	var responseError string
	responseSeen := false
	stop := func(line []byte) bool {
		var response struct {
			ID    json.RawMessage `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &response); err != nil || strings.TrimSpace(string(response.ID)) != "2" {
			return false
		}
		responseSeen = true
		if len(response.Error) > 0 && string(response.Error) != "null" {
			responseError = strings.TrimSpace(string(response.Error))
		}
		return true
	}
	result, err := sessionRunner.RunWithInputAndEnvironmentUntil(
		ctx,
		sourceHome,
		options.Binary,
		input,
		map[string]string{codexHomeVariable: sourceHome},
		stop,
		"app-server",
		"--listen", "stdio://",
		"--disable", "apps",
		"--disable", "hooks",
		"--disable", "multi_agent",
		"--disable", "plugins",
	)
	if err != nil {
		return fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] native Codex app-server auth refresh could not start: %v; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] native Codex app-server auth refresh exited with status %d; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started", result.ExitCode)
	}
	if !responseSeen {
		return fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] native Codex app-server did not return an account/read response; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started")
	}
	if responseError != "" {
		return fmt.Errorf("[ROAST-ENGINE-AUTH-REFRESH] native Codex app-server could not refresh the login: %s; run `codex login` or set CODEX_ACCESS_TOKEN/CODEX_API_KEY and retry; no reviewer was started", responseError)
	}
	return nil
}

func codexAuthForIsolatedHome(content []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("auth.json is not valid JSON: %w", err)
	}
	tokensRaw, ok := document["tokens"]
	if !ok {
		return content, nil
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(tokensRaw, &tokens); err != nil {
		return nil, fmt.Errorf("auth.json tokens are not a JSON object: %w", err)
	}
	if _, ok := tokens["refresh_token"]; !ok {
		return content, nil
	}
	tokens["refresh_token"] = json.RawMessage(`""`)
	updatedTokens, err := json.Marshal(tokens)
	if err != nil {
		return nil, fmt.Errorf("cannot prepare refreshless Codex auth: %w", err)
	}
	document["tokens"] = updatedTokens
	updated, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cannot encode refreshless Codex auth: %w", err)
	}
	return updated, nil
}

func codexSchema() (string, error) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(verdict.Schema()), &schema); err != nil {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot derive Codex's strict verdict schema: %w", err)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] verdict schema has no object properties; update the shared schema before running Codex")
	}
	findings, ok := properties["findings"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] verdict schema has no findings definition; update the shared schema before running Codex")
	}
	items, ok := findings["items"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] verdict schema has no finding item definition; update the shared schema before running Codex")
	}
	required, ok := items["required"].([]any)
	if !ok {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] verdict schema has no finding required list; update the shared schema before running Codex")
	}
	for _, field := range required {
		if field == "suggestion" {
			return verdict.Schema(), nil
		}
	}
	items["required"] = append(required, "suggestion")
	encoded, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("[ROAST-ENGINE-SCHEMA] cannot encode Codex's strict verdict schema: %w", err)
	}
	return string(encoded), nil
}

func (engine *CodexEngine) arguments(snapshotDir, schemaPath string, agentsEnabled bool) []string {
	// Keep review isolation in Codex's native controls. `--ignore-user-config`
	// still discovers user instructions under CODEX_HOME, so Review stages a
	// private HOME containing only auth.json before these arguments are run.
	// ChatGPT refresh tokens are single-use; the staged copy omits that token so
	// an isolated review can use its cached access token without consuming the
	// user's live refresh credential or writing anything back to the source home.
	// Do not pass skills.config overrides: Codex may reconcile them by mutating
	// the shared CODEX_HOME system-skills cache. Disabling skill instructions is
	// enough to keep repository and bundled skills out of this review.
	arguments := []string{
		"exec",
		"--model", engine.options.Model,
		"-c", fmt.Sprintf("model_reasoning_effort=%s", strconv.Quote(engine.options.Thinking)),
		"-c", "project_doc_max_bytes=0",
		"-c", "features.shell_snapshot=false",
		"-c", "features.hooks=false",
		"-c", "features.plugins=false",
		"-c", "features.apps=false",
		"-c", "include_apps_instructions=false",
		"-c", "skills.include_instructions=false",
		"-c", "mcp_servers={}",
		"-c", "web_search=\"disabled\"",
		"-c", fmt.Sprintf("projects.%s.trust_level=%s", strconv.Quote(snapshotDir), strconv.Quote("untrusted")),
		"-c", fmt.Sprintf("shell_environment_policy.inherit=%s", strconv.Quote("core")),
		"-c", "shell_environment_policy.ignore_default_excludes=false",
		"-c", "shell_environment_policy.experimental_use_profile=false",
		"-c", "allow_login_shell=false",
		"-c", fmt.Sprintf("default_permissions=%s", strconv.Quote("autoreview")),
		"-c", "permissions.autoreview.filesystem={\":minimal\"=\"read\",\":workspace_roots\"=\"read\"}",
		"-c", "features.multi_agent=false",
		"-c", "features.multi_agent_v2=false",
	}
	if agentsEnabled {
		arguments = append(arguments, "-c", "agents.enabled=false")
	}
	arguments = append(arguments,
		"--ignore-user-config",
		"--ignore-rules",
		"--strict-config",
		"--skip-git-repo-check",
		"--ephemeral",
		"--color", "never",
		"--cd", snapshotDir,
		"--output-schema", schemaPath,
		"-",
	)
	return arguments
}
