package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerPassesPromptOverStdin(t *testing.T) {
	t.Setenv("ROAST_RUNNER_HELPER", "1")
	command := ExecRunner{}
	result, err := command.RunWithInput(context.Background(), "", os.Args[0], "prompt over stdin", "-test.run=TestRunnerHelperProcess", "--")
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "prompt over stdin" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunnerHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestRunnerHelperProcess") {
		return
	}
	if os.Getenv("ROAST_RUNNER_HELPER") != "1" {
		return
	}
	content, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}
	_, _ = os.Stdout.Write(content)
	os.Exit(0)
}

func TestExecRunnerCanceledProcessReturnsContextError(t *testing.T) {
	t.Setenv("ROAST_RUNNER_SLEEP_HELPER", "1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resultCh := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := (ExecRunner{}).Run(ctx, "", os.Args[0], "-test.run=TestRunnerSleepHelperProcess", "--")
		resultCh <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()
	time.Sleep(25 * time.Millisecond)
	cancel()
	result := <-resultCh
	if !errors.Is(result.err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", result.err)
	}
	if result.result.ExitCode != 0 {
		t.Fatalf("result = %#v, want empty result", result.result)
	}
}

func TestRunnerSleepHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "-test.run=TestRunnerSleepHelperProcess") || os.Getenv("ROAST_RUNNER_SLEEP_HELPER") != "1" {
		return
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}
