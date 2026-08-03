package engine

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/janiorvalle/roast/internal/verdict"
)

type ClaudeEngine struct {
	options Options
}

func NewClaude(options Options) *ClaudeEngine {
	return &ClaudeEngine{options: options.normalized(EngineClaude)}
}

func (engine *ClaudeEngine) Label() string {
	return engine.options.label(EngineClaude)
}

func (engine *ClaudeEngine) Preflight() error {
	if _, err := exec.LookPath(engine.options.Binary); err != nil {
		return fmt.Errorf("[ROAST-ENGINE-UNAVAILABLE] cannot locate Claude CLI %q; install it or set ROAST_CLAUDE_BINARY to an executable path; no other engine will be selected: %w", engine.options.Binary, err)
	}
	return nil
}

func (engine *ClaudeEngine) Review(ctx context.Context, request Request) ([]byte, error) {
	if err := ValidateThinking(EngineClaude, engine.options.Thinking); err != nil {
		return nil, err
	}

	// The selected CLI owns native policy and authentication enforcement. Roast
	// deliberately keeps the user's existing login and environment unchanged.
	// Empty setting sources intentionally exclude user auth helpers as well;
	// copying or selectively reconstructing them would violate the adapter
	// contract's no-auth-staging boundary.
	call := func(ctx context.Context, prompt string) ([]byte, error) {
		result, runErr := runCommandWithHeartbeat(
			ctx,
			engine.options,
			EngineClaude,
			prompt,
			request.SnapshotDir,
			engine.options.Binary,
			engine.arguments(request.SnapshotDir)...,
		)
		if commandErr := commandError(ctx, EngineClaude, engine.options.Binary, result, runErr); commandErr != nil {
			return nil, commandErr
		}
		return result.Stdout, nil
	}
	return reviewWithRetry(ctx, request, engine.Label(), engine.options.Heartbeat, call)
}

func (engine *ClaudeEngine) arguments(snapshotDir string) []string {
	arguments := []string{
		"--safe-mode",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--disallowedTools", "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch,mcp__*",
		"--tools", "Read,Grep,Glob",
		"--allowedTools",
	}
	arguments = append(arguments, claudeReadRules(snapshotDir)...)
	arguments = append(arguments,
		"--permission-mode", "dontAsk",
		"--disable-slash-commands",
		"--no-chrome",
		"--print",
		"--no-session-persistence",
		"--output-format", "json",
		"--json-schema", verdict.Schema(),
		"--model", engine.options.Model,
	)
	if engine.options.Thinking != "" {
		arguments = append(arguments, "--effort", engine.options.Thinking)
	}
	return arguments
}

func claudeReadRules(snapshotDir string) []string {
	absolute := escapeClaudePermissionPattern(claudeAbsolutePath(snapshotDir))
	return []string{
		"Read(" + absolute + "/**)",
		"Grep(" + absolute + "/**)",
		"Glob(" + absolute + "/**)",
	}
}

func claudeAbsolutePath(directory string) string {
	absolute := claudeCanonicalPath(directory)
	absolute = filepath.ToSlash(absolute)
	if runtime.GOOS == "windows" && len(absolute) >= 2 && absolute[1] == ':' {
		absolute = "/" + strings.ToLower(absolute[:1]) + absolute[2:]
	}
	return "//" + strings.TrimPrefix(absolute, "/")
}

func claudeCanonicalPath(directory string) string {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		absolute = filepath.Clean(directory)
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

func escapeClaudePermissionPattern(value string) string {
	var escaped strings.Builder
	for _, character := range value {
		switch character {
		case '\\', ',', '*', '?', '[', ']', '(', ')', '!':
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(character)
	}
	return escaped.String()
}
