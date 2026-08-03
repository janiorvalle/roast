package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// EnvironmentRunner is the optional command seam for invocations that need a
// per-process environment override without mutating the parent process.
type EnvironmentRunner interface {
	RunWithEnvironment(ctx context.Context, dir, program string, environment map[string]string, args ...string) (Result, error)
}

// InputEnvironmentRunner combines stdin input with a per-process environment
// override for commands such as the Codex reviewer.
type InputEnvironmentRunner interface {
	RunWithInputAndEnvironment(ctx context.Context, dir, program, input string, environment map[string]string, args ...string) (Result, error)
}

// InputEnvironmentSessionRunner supports short-lived protocols that return a
// response before the child process exits, such as Codex's app-server auth
// endpoint. The callback decides when the response needed by the caller has
// arrived; stdin stays open until then so the child can continue processing.
type InputEnvironmentSessionRunner interface {
	RunWithInputAndEnvironmentUntil(ctx context.Context, dir, program, input string, environment map[string]string, stop func([]byte) bool, args ...string) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir, program string, args ...string) (Result, error) {
	return run(ctx, dir, nil, nil, nil, program, args...)
}

func (ExecRunner) RunWithInput(ctx context.Context, dir, program, input string, args ...string) (Result, error) {
	return run(ctx, dir, strings.NewReader(input), nil, nil, program, args...)
}

func (ExecRunner) RunWithInputStream(ctx context.Context, dir string, input []byte, stdout io.Writer, program string, args ...string) (Result, error) {
	return run(ctx, dir, bytes.NewReader(input), stdout, nil, program, args...)
}

func (ExecRunner) RunWithEnvironment(ctx context.Context, dir, program string, environment map[string]string, args ...string) (Result, error) {
	return run(ctx, dir, nil, nil, environment, program, args...)
}

func (ExecRunner) RunWithInputAndEnvironment(ctx context.Context, dir, program, input string, environment map[string]string, args ...string) (Result, error) {
	return run(ctx, dir, strings.NewReader(input), nil, environment, program, args...)
}

func (ExecRunner) RunWithInputAndEnvironmentUntil(ctx context.Context, dir, program, input string, environment map[string]string, stop func([]byte) bool, args ...string) (Result, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = dir
	command.Env = environmentWithOverrides(environment)

	stdin, err := command.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("run %s: %w", program, err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("run %s: %w", program, err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return Result{}, fmt.Errorf("run %s: %w", program, err)
	}

	var output bytes.Buffer
	scanDone := make(chan error, 1)
	stopReached := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		stopped := false
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			_, _ = output.Write(line)
			_ = output.WriteByte('\n')
			if !stopped && stop != nil && stop(line) {
				stopped = true
				close(stopReached)
			}
		}
		scanDone <- scanner.Err()
	}()

	if _, err := io.WriteString(stdin, input); err != nil {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return Result{}, fmt.Errorf("run %s: %w", program, err)
	}
	if stop == nil {
		_ = stdin.Close()
	}

	var scanErr error
	scanFinished := false
	select {
	case <-stopReached:
	case scanErr = <-scanDone:
		scanFinished = true
	case <-ctx.Done():
		_ = stdin.Close()
		_, _ = command.Process.Wait()
		return Result{}, ctx.Err()
	}

	_ = stdin.Close()
	if !scanFinished {
		scanErr = <-scanDone
	}
	if scanErr != nil {
		_, _ = command.Process.Wait()
		return Result{}, fmt.Errorf("read %s output: %w", program, scanErr)
	}
	waitErr := command.Wait()
	result := Result{Stdout: output.Bytes(), Stderr: stderr.Bytes()}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, ctxErr
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return Result{}, fmt.Errorf("run %s: %w", program, waitErr)
	}
	return result, nil
}

func run(ctx context.Context, dir string, stdin io.Reader, output io.Writer, environment map[string]string, program string, args ...string) (Result, error) {
	command := exec.CommandContext(ctx, program, args...)
	command.Dir = dir
	command.Stdin = stdin
	if len(environment) > 0 {
		command.Env = environmentWithOverrides(environment)
	}
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

func environmentWithOverrides(overrides map[string]string) []string {
	environment := os.Environ()
	for name, value := range overrides {
		prefix := name + "="
		filtered := environment[:0]
		for _, entry := range environment {
			entryName, _, hasValue := strings.Cut(entry, "=")
			if !hasValue || !strings.EqualFold(entryName, name) {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, prefix+value)
	}
	return environment
}

func Failure(program string, args []string, result Result) error {
	detail := strings.TrimSpace(string(result.Stderr))
	command := strings.TrimSpace(strings.Join(append([]string{program}, args...), " "))
	if detail == "" {
		return fmt.Errorf("%s exited with status %d", command, result.ExitCode)
	}
	return fmt.Errorf("%s exited with status %d: %s", command, result.ExitCode, detail)
}
