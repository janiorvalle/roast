package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/janiorvalle/roast/internal/verdict"
)

type CodexEngine struct {
	options Options
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
	for _, override := range []string{"features.multi_agent=false", "features.multi_agent_v2=false"} {
		if _, err := engine.requireConfigOverride(ctx, override); err != nil {
			return nil, err
		}
	}
	agentsEnabled, err := engine.requireConfigOverride(ctx, "agents.enabled=false")
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
			engine.arguments(request.SnapshotDir, schemaPath, agentsEnabled)...,
		)
		if commandErr := commandError(ctx, EngineCodex, engine.options.Binary, result, runErr); commandErr != nil {
			return nil, commandErr
		}
		return result.Stdout, nil
	}
	return reviewWithRetry(ctx, request, engine.Label(), engine.options.Heartbeat, call)
}

func (engine *CodexEngine) requireConfigOverride(ctx context.Context, override string) (bool, error) {
	probeDir, err := os.MkdirTemp("", "roast-codex-capability-*")
	if err != nil {
		return false, fmt.Errorf("[ROAST-ENGINE-CAPABILITY] cannot create private Codex capability probe directory: %w; retry with a writable system temp directory", err)
	}
	defer os.RemoveAll(probeDir)
	probeSchema := filepath.Join(probeDir, "missing-schema.json")
	result, err := engine.options.Command.Run(
		ctx,
		"",
		engine.options.Binary,
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
	// Keep isolation and authentication in Codex's native controls; roast does
	// not reconstruct the user's runtime or second-guess its login state.
	// `--ignore-user-config` intentionally also ignores user auth-policy
	// settings; preserving those would require the auth staging this adapter
	// contract explicitly forbids.
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
