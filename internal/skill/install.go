package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/janiorvalle/roast"
)

type agentHome struct {
	name string
	root string
}

const managedSkillMarker = "<!-- roast-managed-skill: v1 -->\n"

// AutoInstall discovers the agent homes supported by roast and installs the
// bundled skill into each home that already exists.
func AutoInstall(output io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("[ROAST-SKILL-HOME] cannot determine the user home directory: %w; set HOME or USERPROFILE and retry", err)
	}
	return install(home, output, false, "")
}

// AutoInstallForRepository performs automatic installation without writing
// into the repository that the current review is about to inspect.
func AutoInstallForRepository(output io.Writer, repository string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("[ROAST-SKILL-HOME] cannot determine the user home directory: %w; set HOME or USERPROFILE and retry", err)
	}
	return install(home, output, false, repository)
}

// Install writes the bundled skill into detected Claude Code and Codex homes.
// Missing agent homes are intentionally skipped so running roast does not
// create configuration for an agent the user does not have.
func Install(home string, output io.Writer) error {
	return install(home, output, false, "")
}

// ForceInstall replaces an existing skill even when it is not roast-managed.
// Callers should expose this only behind an explicit user action.
func ForceInstall(home string, output io.Writer) error {
	return install(home, output, true, "")
}

// Repair installs the skill into the current user's detected agent homes.
func Repair(output io.Writer, force bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("[ROAST-SKILL-HOME] cannot determine the user home directory: %w; set HOME or USERPROFILE and retry", err)
	}
	return install(home, output, force, "")
}

func install(home string, output io.Writer, force bool, protectedRepository string) error {
	if output == nil {
		output = io.Discard
	}
	if strings.TrimSpace(home) == "" {
		return fmt.Errorf("[ROAST-SKILL-HOME] user home directory is empty; set HOME or USERPROFILE and retry")
	}
	home = filepath.Clean(home)

	source := roast.SkillInstructions()
	var failures []error
	detected := false
	for _, agent := range agentHomes(home) {
		destination := filepath.Join(agent.root, "skills", "roast", "SKILL.md")
		if protectedRepository != "" {
			overlaps, err := pathsOverlap(destination, protectedRepository)
			if err != nil {
				fmt.Fprintf(output, "roast: cannot verify whether %s skill destination %q overlaps review repository %q; skipped automatic install: %v\n", agent.name, destination, protectedRepository, err)
				continue
			}
			if overlaps {
				fmt.Fprintf(output, "roast: %s skill destination %q overlaps review repository %q; skipped automatic install\n", agent.name, destination, protectedRepository)
				continue
			}
		}
		info, err := os.Stat(agent.root)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(output, "roast: %s home not detected at %q; skipped\n", agent.name, agent.root)
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("[ROAST-SKILL-DETECT] cannot inspect %s home %q: %w; make it readable or remove it, then retry", agent.name, agent.root, err))
			continue
		}
		if !info.IsDir() {
			failures = append(failures, fmt.Errorf("[ROAST-SKILL-DETECT] %s home %q is not a directory; replace it with a directory, then retry", agent.name, agent.root))
			continue
		}
		detected = true

		action, err := installFile(destination, source, force)
		if err != nil {
			failures = append(failures, fmt.Errorf("[ROAST-SKILL-INSTALL] cannot install the %s skill at %q: %w; make the agent home writable and retry", agent.name, destination, err))
			continue
		}
		if action == "already up to date" {
			continue
		}
		fmt.Fprintf(output, "roast: %s skill %s at %q\n", agent.name, action, destination)
	}
	if !detected && len(failures) == 0 {
		fmt.Fprintln(output, "roast: no supported agent home detected; nothing installed")
	}
	return errors.Join(failures...)
}

func pathsOverlap(path, repository string) (bool, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	repository, err = filepath.Abs(repository)
	if err != nil {
		return false, err
	}
	lexicalOverlap, err := pathWithin(path, repository)
	if err != nil {
		return false, err
	}
	if lexicalOverlap {
		return true, nil
	}
	path, err = resolveExistingAncestor(path)
	if err != nil {
		return false, err
	}
	repository, err = resolveExistingAncestor(repository)
	if err != nil {
		return false, err
	}
	return pathWithin(path, repository)
}

func pathWithin(path, repository string) (bool, error) {
	relative, err := filepath.Rel(filepath.Clean(repository), filepath.Clean(path))
	if err != nil {
		return false, err
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))), nil
}

func resolveExistingAncestor(path string) (string, error) {
	path = filepath.Clean(path)
	missing := make([]string, 0, 2)
	for {
		if _, err := os.Lstat(path); err == nil {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no existing ancestor for %q", path)
		}
		missing = append(missing, filepath.Base(path))
		path = parent
	}
}

func agentHomes(home string) []agentHome {
	codexRoot := filepath.Join(home, ".codex")
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		codexRoot = configured
	}
	return []agentHome{
		{name: "Claude Code", root: filepath.Join(home, ".claude")},
		{name: "Codex", root: codexRoot},
	}
}

func installFile(destination string, source []byte, force bool) (string, error) {
	info, statErr := os.Lstat(destination)
	if statErr == nil && info.IsDir() {
		return "", fmt.Errorf("destination is a directory")
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 && !force {
		return "is a symlink; kept existing (run `roast install-skill --force` to replace)", nil
	}

	current, readErr := os.ReadFile(destination)
	if readErr == nil && bytes.Equal(current, source) {
		return "already up to date", nil
	}
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if readErr == nil && !force && !bytes.Contains(current, []byte(managedSkillMarker)) {
		return "differs; kept existing (run `roast install-skill --force` to replace)", nil
	}

	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	if err := writeFileAtomically(destination, source); err != nil {
		return "", err
	}
	if errors.Is(readErr, os.ErrNotExist) {
		return "installed", nil
	}
	return "updated", nil
}

func writeFileAtomically(destination string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".roast-skill-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replaceFile(temporaryName, destination)
}

func replaceFile(temporary, destination string) error {
	if err := os.Rename(temporary, destination); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}

	backup, err := os.CreateTemp(filepath.Dir(destination), ".roast-skill-backup-*")
	if err != nil {
		return err
	}
	backupName := backup.Name()
	if err := backup.Close(); err != nil {
		_ = os.Remove(backupName)
		return err
	}
	if err := os.Remove(backupName); err != nil {
		return err
	}
	if err := os.Rename(destination, backupName); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		if restoreErr := os.Rename(backupName, destination); restoreErr != nil {
			return fmt.Errorf("replace failed: %w; restore failed: %v", err, restoreErr)
		}
		return err
	}
	return os.Remove(backupName)
}
