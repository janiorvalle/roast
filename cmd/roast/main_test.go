package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/janiorvalle/roast/internal/bundle"
	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/target"
	"github.com/janiorvalle/roast/internal/verdict"
)

func runTest(args []string, stdout, stderr *bytes.Buffer) int {
	return runWithDependencies(args, stdout, stderr, runDependencies{
		commands: runner.ExecRunner{},
		scanSecrets: func(context.Context, target.Target, []byte, []byte, runner.Runner) error {
			return nil
		},
	})
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--version"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != version {
		t.Fatalf("version output = %q", stdout.String())
	}
}

func TestRunHelpReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--help"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Usage of roast:") {
		t.Fatalf("stderr = %q, want usage output", stderr.String())
	}
}

func TestRunInstallSkillUsesInstallerAndWritesTranscript(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := false
	exitCode := runWithDependencies([]string{"install-skill"}, &stdout, &stderr, runDependencies{
		repairSkill: func(output io.Writer, force bool) error {
			called = true
			if force {
				t.Fatal("install-skill unexpectedly forced replacement")
			}
			_, err := output.Write([]byte("installed test skill\n"))
			return err
		},
	})
	if exitCode != 0 || !called {
		t.Fatalf("exit code = %d, called = %t, stdout = %s, stderr = %s", exitCode, called, stdout.String(), stderr.String())
	}
	if stdout.String() != "installed test skill\n" || stderr.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestRunVersionDoesNotInstallSkill(t *testing.T) {
	called := false
	var stdout, stderr bytes.Buffer
	exitCode := runWithDependencies([]string{"--version"}, &stdout, &stderr, runDependencies{
		installSkill: func(io.Writer, string) error {
			called = true
			return nil
		},
	})
	if exitCode != 0 || called {
		t.Fatalf("exit code = %d, called = %t, stdout = %s, stderr = %s", exitCode, called, stdout.String(), stderr.String())
	}
}

func TestRunStopsBeforeEngineWhenSecretGateFails(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	var stdout, stderr bytes.Buffer
	called := false
	exitCode := runWithDependencies([]string{"--dirty", "--repo", repo}, &stdout, &stderr, runDependencies{
		commands: runner.ExecRunner{},
		scanSecrets: func(context.Context, target.Target, []byte, []byte, runner.Runner) error {
			called = true
			return errors.New("[ROAST-SECRET-VERIFIED] fixture secret; no review engine was called")
		},
	})
	if exitCode != 1 || !called {
		t.Fatalf("exit code = %d, scanner called = %t, stderr = %s", exitCode, called, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ROAST-SECRET-VERIFIED") || strings.Contains(stderr.String(), "ROAST-ENGINE-FAKE") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunStopsWhenSensitiveChangesAreExcluded(t *testing.T) {
	repo := initMainRepository(t)
	if err := os.WriteFile(filepath.Join(repo, ".env.local"), []byte("TOKEN=fixture-only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	exitCode := runTest([]string{"--dirty", "--repo", repo}, &stdout, &stderr)
	if exitCode != 1 || !strings.Contains(stderr.String(), "ROAST-BUNDLE-SENSITIVE") || !strings.Contains(stderr.String(), ".env.local") {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if strings.Contains(stderr.String(), "ROAST-ENGINE-FAKE") {
		t.Fatalf("engine ran after sensitive exclusion: %s", stderr.String())
	}
}

func TestRunSkipsInvalidUTF8ContextDocumentAndContinues(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("valid context\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMainGit(t, repo, "add", "README.md")
	runMainGit(t, repo, "commit", "-m", "context document")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte{'#', ' ', 'R', 0xe9, 's', 'u', 'm', 0xe9, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	responsePath := cleanResponseFixture(t, repo)
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"ROAST-PROMPT-CONTEXT", `"README.md": not valid UTF-8`, "well done - send it."} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), expected)
		}
	}
	if strings.Contains(stderr.String(), "ROAST-ENGINE-FAILED") {
		t.Fatalf("engine failed after invalid context exclusion: %s", stderr.String())
	}
}

func TestRunSanitizesInvalidUTF8DiffAndReportsAffectedFile(t *testing.T) {
	repo := initMainRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte{'c', 'h', 'a', 'n', 'g', 'e', 'd', ' ', 0xe9, '\n'}, 0o600); err != nil {
		t.Fatal(err)
	}
	responsePath := cleanResponseFixture(t, repo)
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"ROAST-PROMPT-DIFF", `"base.txt"`, "U+FFFD", "well done - send it."} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), expected)
		}
	}
	if strings.Contains(stderr.String(), "ROAST-ENGINE-FAILED") {
		t.Fatalf("engine failed after invalid diff sanitization: %s", stderr.String())
	}
}

func TestRunShortCircuitsEmptyDiffBeforeSecretScanAndEngine(t *testing.T) {
	repo := initMainRepository(t)
	var stdout, stderr bytes.Buffer
	scannerCalled := false
	missingVerdict := filepath.Join(t.TempDir(), "missing-verdict.json")
	exitCode := runWithDependencies([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", missingVerdict}, &stdout, &stderr, runDependencies{
		commands: runner.ExecRunner{},
		scanSecrets: func(context.Context, target.Target, []byte, []byte, runner.Runner) error {
			scannerCalled = true
			return nil
		},
	})
	if exitCode != 0 || scannerCalled {
		t.Fatalf("exit code = %d, scanner called = %t, stdout = %s, stderr = %s", exitCode, scannerCalled, stdout.String(), stderr.String())
	}
	for _, expected := range []string{"nothing to review", "make a change", "--dirty", "--base", "--commit", "PR"} {
		if !strings.Contains(stderr.String(), expected) {
			t.Fatalf("stderr = %q, missing %q", stderr.String(), expected)
		}
	}
}

func TestRunBuildsBundleDirectory(t *testing.T) {
	repo := t.TempDir()
	runMainGit(t, repo, "init", "-b", "main")
	runMainGit(t, repo, "config", "user.email", "test@example.com")
	runMainGit(t, repo, "config", "user.name", "Roast Test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMainGit(t, repo, "add", "base.txt")
	runMainGit(t, repo, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	responsePath := cleanResponseFixture(t, repo)
	destination := filepath.Join(t.TempDir(), "review-bundle")
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--bundle-dir", destination, "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	for _, name := range []string{"diff.patch", "snapshot.tar", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(destination, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	if !strings.Contains(stdout.String(), "target: uncommitted changes") || !strings.Contains(stdout.String(), "bundle: "+destination) {
		t.Fatalf("stdout = %s", stdout.String())
	}
}

func TestRunRejectsRepositoryBundleDirectory(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	responsePath := cleanResponseFixture(t, repo)
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", canonicalRepo, "--engine", "fake", "--bundle-dir", filepath.Join(canonicalRepo, "review-bundle"), "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "inside repository") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunRejectsNonNumericPositionalTarget(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"not-a-pr"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ROAST-CLI-PR") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunRejectsRemovedCostApprovalFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--yes"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "ROAST-COST-GUARD") {
		t.Fatalf("removed cost guard still appeared: %s", stderr.String())
	}
}

func TestResolveEngineBinarySettingUsesEngineSpecificEnvironment(t *testing.T) {
	t.Setenv("ROAST_CLAUDE_BINARY", "/custom/claude")
	t.Setenv("ROAST_BINARY", "/generic/engine")
	if got := resolveEngineSetting("claude", "", "BINARY"); got != "/custom/claude" {
		t.Fatalf("Claude binary = %q", got)
	}
	if got := resolveEngineSetting("codex", "/flag/codex", "BINARY"); got != "/flag/codex" {
		t.Fatalf("flag binary = %q", got)
	}
	if got := resolveEngineSetting("fake", "", "BINARY"); got != "/generic/engine" {
		t.Fatalf("generic binary = %q", got)
	}
}

func TestRunFakeEngineCleanPlainOutput(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	responsePath := cleanResponseFixture(t, repo)
	verdictPath := filepath.Join(t.TempDir(), "verdict.json")
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--plain", "--fake-verdict", responsePath, "--json-output", verdictPath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "clean: no findings") || strings.Contains(stderr.String(), "well done") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	encoded, err := os.ReadFile(verdictPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := verdict.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Overall != verdict.OverallWellDone || len(decoded.Findings) != 0 {
		t.Fatalf("verdict = %#v", decoded)
	}
}

func TestRunFakeEngineFiltersAndFailsOnIncludedFinding(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	provenance := testProvenance(t, repo)
	responsePath := filepath.Join(t.TempDir(), "response.json")
	filteredPath := filepath.Join(t.TempDir(), "filtered.json")
	writeVerdictFixture(t, responsePath, verdict.Verdict{
		Overall: verdict.OverallRaw,
		Findings: []verdict.Finding{
			{Priority: verdict.PriorityP3, File: "base.txt", Line: 1, Title: "minor note", Rationale: "the low priority case"},
		},
		Provenance: provenance,
	})
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath, "--json-output", filteredPath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("filtered exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "well done - send it.") || !strings.Contains(stdout.String(), "verdict: "+filteredPath) {
		t.Fatalf("stdout = %s, stderr = %s", stdout.String(), stderr.String())
	}

	writeVerdictFixture(t, responsePath, verdict.Verdict{
		Overall: verdict.OverallRaw,
		Findings: []verdict.Finding{
			{Priority: verdict.PriorityP2, File: "base.txt", Line: 1, Title: "real defect", Rationale: "the input reaches a wrong result"},
		},
		Provenance: provenance,
	})
	stdout.Reset()
	stderr.Reset()
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("raw exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "RAW.") || !strings.Contains(stderr.String(), "P2 base.txt:1: real defect") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunRejectsFindingOutsideSnapshot(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	provenance := testProvenance(t, repo)
	responsePath := filepath.Join(t.TempDir(), "response.json")
	writeVerdictFixture(t, responsePath, verdict.Verdict{
		Overall: verdict.OverallRaw,
		Findings: []verdict.Finding{
			{Priority: verdict.PriorityP1, File: "missing.go", Line: 1, Title: "missing file", Rationale: "it is not present"},
		},
		Provenance: provenance,
	})
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ROAST-VERDICT-FILE") || !strings.Contains(stderr.String(), "choose a tracked snapshot file") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestRunReportsCopyableProvenanceMismatch(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	provenance := testProvenance(t, repo)
	provenance.Tree = "wrong"
	responsePath := filepath.Join(t.TempDir(), "response.json")
	writeVerdictFixture(t, responsePath, verdict.Verdict{
		Overall:    verdict.OverallWellDone,
		Findings:   []verdict.Finding{},
		Provenance: provenance,
	})
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--plain", "--fake-verdict", responsePath}, &stdout, &stderr); exitCode != 1 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `context: snapshot + 0 project docs []; priorities=P0, P1, P2; context-glob=""; extra-prompt=""; isolation=fake`) {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if strings.Contains(stderr.String(), `\"`) {
		t.Fatalf("provenance hint still uses Go-escaped quotes: %s", stderr.String())
	}
}

func TestEnsureBundleUnchangedRejectsWorktreeDrift(t *testing.T) {
	repo := initMainRepository(t)
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Dirty: true, RepoDir: repo, Runner: runner.ExecRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := bundle.Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("changed during review\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureBundleUnchanged(context.Background(), reviewTarget, reviewBundle, runner.ExecRunner{}); err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-STALE") {
		t.Fatalf("stale bundle error = %v", err)
	}
}

func TestEnsureBundleUnchangedRejectsMovedTargetRef(t *testing.T) {
	repo := initMainRepository(t)
	runMainGit(t, repo, "switch", "-c", "feature")
	if err := os.WriteFile(filepath.Join(repo, "feature.txt"), []byte("feature\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMainGit(t, repo, "add", "feature.txt")
	runMainGit(t, repo, "commit", "-m", "feature")
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Base: "main", RepoDir: repo, Runner: runner.ExecRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := bundle.Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	runMainGit(t, repo, "update-ref", "refs/heads/main", "HEAD")
	if err := ensureBundleUnchanged(context.Background(), reviewTarget, reviewBundle, runner.ExecRunner{}); err == nil || !strings.Contains(err.Error(), "ROAST-BUNDLE-STALE") {
		t.Fatalf("moved target error = %v", err)
	}
}

func TestBundleFingerprintIncludesSecretScanInputs(t *testing.T) {
	baseline := bundle.Bundle{
		Diff:                   []byte("review diff"),
		Snapshot:               []byte("review snapshot"),
		SecretScanDiff:         []byte("secret diff"),
		SecretScanSnapshot:     []byte("secret snapshot"),
		ExcludedSensitivePaths: []string{".env.local"},
	}
	baseFingerprint := bundleFingerprint(baseline)
	variants := []bundle.Bundle{
		{Diff: baseline.Diff, Snapshot: baseline.Snapshot, SecretScanDiff: []byte("changed secret diff"), SecretScanSnapshot: baseline.SecretScanSnapshot, ExcludedSensitivePaths: baseline.ExcludedSensitivePaths},
		{Diff: baseline.Diff, Snapshot: baseline.Snapshot, SecretScanDiff: baseline.SecretScanDiff, SecretScanSnapshot: []byte("changed secret snapshot"), ExcludedSensitivePaths: baseline.ExcludedSensitivePaths},
		{Diff: baseline.Diff, Snapshot: baseline.Snapshot, SecretScanDiff: baseline.SecretScanDiff, SecretScanSnapshot: baseline.SecretScanSnapshot, ExcludedSensitivePaths: []string{".env.production"}},
	}
	for index, variant := range variants {
		if got := bundleFingerprint(variant); got == baseFingerprint {
			t.Fatalf("variant %d fingerprint = %q, want it to differ from %q", index, got, baseFingerprint)
		}
	}
	if got := bundleFingerprint(bundle.Bundle{Diff: baseline.Diff, Snapshot: baseline.Snapshot, SecretScanDiff: baseline.SecretScanDiff, SecretScanSnapshot: baseline.SecretScanSnapshot, ExcludedSensitivePaths: []string{".env.local"}}); got != baseFingerprint {
		t.Fatalf("same inputs with equivalent path order changed fingerprint: %q != %q", got, baseFingerprint)
	}
}

func TestDisplayProvenanceValueEscapesControlsWithoutEscapingQuotes(t *testing.T) {
	got := displayProvenanceValue("quoted \"value\"\n\x1b[31m\u202e\u2028\u2029")
	if got != `quoted "value"\u000a\u001b[31m\u202e\u2028\u2029` {
		t.Fatalf("displayed provenance = %q", got)
	}
}

func TestRunReplacesHardlinkedVerdictOutputAtomically(t *testing.T) {
	repo := initMainRepository(t)
	writeMainChange(t, repo)
	responsePath := cleanResponseFixture(t, repo)
	outputDirectory := t.TempDir()
	outputPath := filepath.Join(outputDirectory, "verdict.json")
	if err := os.Link(filepath.Join(repo, "base.txt"), outputPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if exitCode := runTest([]string{"--dirty", "--repo", repo, "--engine", "fake", "--fake-verdict", responsePath, "--json-output", outputPath}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, stdout = %s, stderr = %s", exitCode, stdout.String(), stderr.String())
	}
	repositoryContent, err := os.ReadFile(filepath.Join(repo, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(repositoryContent) != "base\n" {
		t.Fatalf("repository file was modified through hard link: %q", repositoryContent)
	}
}

func initMainRepository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runMainGit(t, repo, "init", "-b", "main")
	runMainGit(t, repo, "config", "user.email", "test@example.com")
	runMainGit(t, repo, "config", "user.name", "Roast Test")
	if err := os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runMainGit(t, repo, "add", "base.txt")
	runMainGit(t, repo, "commit", "-m", "base")
	return repo
}

func writeMainChange(t *testing.T, repo string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cleanResponseFixture(t *testing.T, repo string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clean-response.json")
	writeVerdictFixture(t, path, verdict.Verdict{Overall: verdict.OverallWellDone, Findings: []verdict.Finding{}, Provenance: testProvenance(t, repo)})
	return path
}

func testProvenance(t *testing.T, repo string) verdict.Provenance {
	t.Helper()
	reviewTarget, err := target.Resolve(context.Background(), target.Options{Dirty: true, RepoDir: repo, Runner: runner.ExecRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	reviewBundle, err := bundle.Build(context.Background(), reviewTarget, runner.ExecRunner{})
	if err != nil {
		t.Fatal(err)
	}
	return verdict.Provenance{
		Target:  "HEAD (uncommitted)",
		Branch:  "WORKTREE",
		Tree:    bundleFingerprint(reviewBundle),
		Engine:  "fake/canned",
		Context: contextWithIsolation(nil, verdict.PriorityP2, "", "", "fake"),
	}
}

func mainGitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func writeVerdictFixture(t *testing.T, path string, reviewVerdict verdict.Verdict) {
	t.Helper()
	encoded, err := verdict.Encode(reviewVerdict)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func runMainGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
