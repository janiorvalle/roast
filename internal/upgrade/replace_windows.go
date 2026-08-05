//go:build windows

package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

const (
	windowsAccessDenied     syscall.Errno = 5
	windowsSharingViolation syscall.Errno = 32
	windowsInvalidParameter syscall.Errno = 87
	windowsSynchronize      uintptr       = 0x00100000
	windowsWaitObject       uintptr       = 0
	windowsWaitTimeout      uintptr       = 258
)

var (
	windowsKernel32            = syscall.NewLazyDLL("kernel32.dll")
	windowsOpenProcess         = windowsKernel32.NewProc("OpenProcess")
	windowsWaitForSingleObject = windowsKernel32.NewProc("WaitForSingleObject")
	windowsCloseHandle         = windowsKernel32.NewProc("CloseHandle")
)

func replaceFileAtomically(replacement, destination string) error {
	backup := destination + ".roast-previous-" + strconv.Itoa(os.Getpid())
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(destination, backup); err != nil {
		return err
	}
	if err := os.Rename(replacement, destination); err != nil {
		if restoreErr := os.Rename(backup, destination); restoreErr != nil {
			return errors.Join(err, restoreErr)
		}
		return err
	}

	cleanup := exec.Command(destination, "__upgrade-cleanup", backup, strconv.Itoa(os.Getpid()))
	cleanup.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008}
	if err := cleanup.Start(); err != nil {
		removeErr := os.Remove(destination)
		restoreErr := os.Rename(backup, destination)
		return errors.Join(fmt.Errorf("start cleanup process: %w", err), removeErr, restoreErr)
	}
	_ = cleanup.Process.Release()
	return nil
}

func cleanupReplacedExecutable(ctx context.Context, path string, parentProcessID int) error {
	if err := waitForProcessExit(ctx, parentProcessID); err != nil {
		return err
	}
	timeout := time.NewTimer(30 * time.Second)
	defer timeout.Stop()
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		var number syscall.Errno
		if !errors.As(err, &number) || (number != windowsSharingViolation && number != windowsAccessDenied) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout.C:
			return fmt.Errorf("timed out after 30 seconds waiting to remove %q: %w", path, err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func waitForProcessExit(ctx context.Context, processID int) error {
	handle, _, callErr := windowsOpenProcess.Call(windowsSynchronize, 0, uintptr(processID))
	if handle == 0 {
		if errors.Is(callErr, windowsInvalidParameter) {
			return nil
		}
		return fmt.Errorf("open upgrading process %d: %w", processID, callErr)
	}
	defer windowsCloseHandle.Call(handle)
	for {
		result, _, waitErr := windowsWaitForSingleObject.Call(handle, 500)
		switch result {
		case windowsWaitObject:
			return nil
		case windowsWaitTimeout:
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		default:
			return fmt.Errorf("wait for upgrading process %d: %w", processID, waitErr)
		}
	}
}
