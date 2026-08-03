package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Result keeps process output separate from process-launch errors. A non-zero
// exit status is returned in Result so callers can accept commands such as
// git diff --no-index, whose status 1 means "files differ" rather than failure.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type Runner interface {
	Run(ctx context.Context, dir, program string, args ...string) (Result, error)
	RunWithInputStream(ctx context.Context, dir string, input []byte, stdout io.Writer, program string, args ...string) (Result, error)
}

// InputRunner is the optional stdin-capable extension used by engine
// adapters. Keeping it separate preserves the small Runner interface for
// callers that only need command output.
type InputRunner interface {
	RunWithInput(ctx context.Context, dir, program, input string, args ...string) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, program string, args ...string) (Result, error) {
	return run(ctx, dir, nil, nil, program, args...)
}

func (ExecRunner) RunWithInput(ctx context.Context, dir, program, input string, args ...string) (Result, error) {
	return run(ctx, dir, strings.NewReader(input), nil, program, args...)
}

func (ExecRunner) RunWithInputStream(ctx context.Context, dir string, input []byte, stdout io.Writer, program string, args ...string) (Result, error) {
	return run(ctx, dir, bytes.NewReader(input), stdout, program, args...)
}

func run(ctx context.Context, dir string, stdin io.Reader, output io.Writer, program string, args ...string) (Result, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = dir
	command.Stdin = stdin
	var stdout, stderr bytes.Buffer
	if output == nil {
		command.Stdout = &stdout
	} else {
		command.Stdout = output
	}
	command.Stderr = &stderr

	result := Result{Stdout: nil, Stderr: nil, ExitCode: 0}
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			result.Stdout = stdout.Bytes()
			result.Stderr = stderr.Bytes()
			return result, nil
		}
		return Result{}, fmt.Errorf("run %s: %w", program, err)
	}

	result.Stdout = stdout.Bytes()
	result.Stderr = stderr.Bytes()
	return result, nil
}

func Failure(program string, args []string, result Result) error {
	detail := strings.TrimSpace(string(result.Stderr))
	command := strings.TrimSpace(strings.Join(append([]string{program}, args...), " "))
	if detail == "" {
		return fmt.Errorf("%s exited with status %d", command, result.ExitCode)
	}
	return fmt.Errorf("%s exited with status %d: %s", command, result.ExitCode, detail)
}
