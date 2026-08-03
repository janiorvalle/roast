package skill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/roast"
)

func TestInstallWritesSkillToDetectedAgentHomes(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	for _, directory := range []string{".claude", ".codex"} {
		if err := os.Mkdir(filepath.Join(home, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	var output bytes.Buffer
	if err := Install(home, &output); err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{".claude", ".codex"} {
		path := filepath.Join(home, directory, "skills", "roast", "SKILL.md")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, roast.SkillInstructions()) {
			t.Fatalf("installed %s does not match the bundled skill", path)
		}
	}
	if !strings.Contains(output.String(), "Claude Code skill installed") || !strings.Contains(output.String(), "Codex skill installed") {
		t.Fatalf("install transcript = %s", output.String())
	}
}

func TestInstallUpdatesChangedSkillAndIsIdempotent(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	claudeSkill := filepath.Join(home, ".claude", "skills", "roast", "SKILL.md")
	codexSkill := filepath.Join(home, ".codex", "skills", "roast", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(claudeSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(codexSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	managedOldSkill := []byte("---\nname: roast\n---\n\n" + managedSkillMarker + "old skill\n")
	if err := os.WriteFile(claudeSkill, managedOldSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexSkill, roast.SkillInstructions(), 0o644); err != nil {
		t.Fatal(err)
	}

	var firstOutput bytes.Buffer
	if err := Install(home, &firstOutput); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(firstOutput.String(), "Claude Code skill updated") || strings.Contains(firstOutput.String(), "Codex skill already up to date") {
		t.Fatalf("first install transcript = %s", firstOutput.String())
	}

	var secondOutput bytes.Buffer
	if err := Install(home, &secondOutput); err != nil {
		t.Fatal(err)
	}
	if secondOutput.Len() != 0 {
		t.Fatalf("second install transcript = %s", secondOutput.String())
	}
}

func TestInstallSkipsUndetectedAgentHomes(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	var output bytes.Buffer
	if err := Install(home, &output); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("Claude Code home was created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("Codex home was created: %v", err)
	}
	if !strings.Contains(output.String(), "no supported agent home detected") {
		t.Fatalf("skip transcript = %s", output.String())
	}
}

func TestInstallReportsInvalidAgentHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Install(home, &output)
	if err == nil || !strings.Contains(err.Error(), "ROAST-SKILL-DETECT") || !strings.Contains(err.Error(), "Claude Code") {
		t.Fatalf("install error = %v", err)
	}
}

func TestInstallPreservesUnmanagedSkill(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "skills", "roast", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("a user-authored skill\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Install(home, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, original) || !strings.Contains(output.String(), "kept existing") {
		t.Fatalf("content = %q, transcript = %s", content, output.String())
	}
}

func TestInstallPreservesManagedSkillSymlink(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	destination := filepath.Join(home, ".claude", "skills", "roast", "SKILL.md")
	outside := filepath.Join(home, "shared-skill.md")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(managedSkillMarker+"old skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var output bytes.Buffer
	if err := Install(home, &output); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("skill symlink was replaced: info=%v, err=%v", info, err)
	}
	if !strings.Contains(output.String(), "is a symlink; kept existing") {
		t.Fatalf("symlink transcript = %s", output.String())
	}
}

func TestInstallUsesCodexHomeOverride(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Install(home, &output); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(codexHome, "skills", "roast", "SKILL.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("Codex skill at CODEX_HOME = %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); !os.IsNotExist(err) {
		t.Fatalf("default Codex home was created: %v", err)
	}
}

func TestAutoInstallForRepositorySkipsDestinationsInsideRepository(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	repository := filepath.Join(home, ".codex")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := install(home, &output, false, repository); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repository, "skills", "roast", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("repository skill was created: %v", err)
	}
	if !strings.Contains(output.String(), "overlaps review repository") {
		t.Fatalf("skip transcript = %s", output.String())
	}
}

func TestAutoInstallForRepositorySkipsInRepositoryDestinationSymlink(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	repository := filepath.Join(home, "repo")
	destination := filepath.Join(repository, ".codex", "skills", "roast", "SKILL.md")
	outside := filepath.Join(home, "outside-skill.md")
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(managedSkillMarker+"old skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(repository, ".codex"))
	var output bytes.Buffer
	if err := install(home, &output, false, repository); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(destination); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination symlink was replaced: info=%v, err=%v", info, err)
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != managedSkillMarker+"old skill\n" {
		t.Fatalf("symlink target changed: %q", content)
	}
}

func TestBundledSkillStaysUnderOneHundredLines(t *testing.T) {
	if lines := bytes.Count(roast.SkillInstructions(), []byte("\n")) + 1; lines >= 100 {
		t.Fatalf("bundled skill has %d lines", lines)
	}
}
