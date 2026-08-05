//go:build !windows

package upgrade

import "os"

import "context"

func replaceFileAtomically(replacement, destination string) error {
	return os.Rename(replacement, destination)
}

func cleanupReplacedExecutable(context.Context, string, int) error {
	return nil
}
