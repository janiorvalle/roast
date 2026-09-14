package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"
	"unicode/utf8"

	"github.com/janiorvalle/roast"
	"github.com/janiorvalle/roast/internal/bundle"
	"github.com/janiorvalle/roast/internal/engine"
	"github.com/janiorvalle/roast/internal/output"
	"github.com/janiorvalle/roast/internal/prompt"
	"github.com/janiorvalle/roast/internal/runner"
	"github.com/janiorvalle/roast/internal/secrets"
	"github.com/janiorvalle/roast/internal/skill"
	"github.com/janiorvalle/roast/internal/target"
	"github.com/janiorvalle/roast/internal/upgrade"
	"github.com/janiorvalle/roast/internal/verdict"
)

var version = "dev"

func main() {
	// Keep exit codes stable for the later review gate: usage is 2, target or
	// bundle failures are 1, and a built bundle is 0.
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type runDependencies struct {
	commands     runner.Runner
	scanSecrets  func(context.Context, target.Target, []byte, []byte, runner.Runner) error
	installSkill func(io.Writer, string) error
	repairSkill  func(io.Writer, bool) error
	upgrade      func(context.Context, string, io.Writer) error
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithDependencies(args, stdout, stderr, runDependencies{
		commands:     runner.ExecRunner{},
		scanSecrets:  secrets.Scan,
		installSkill: skill.AutoInstallForRepository,
		repairSkill:  skill.Repair,
		upgrade:      upgrade.Run,
	})
}

func runWithDependencies(args []string, stdout, stderr io.Writer, dependencies runDependencies) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if args == nil {
		args = []string{}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if len(args) > 0 && args[0] == "install-skill" {
		installer := dependencies.repairSkill
		if installer == nil {
			installer = func(io.Writer, bool) error { return nil }
		}
		return runInstallSkill(args[1:], stdout, stderr, installer)
	}
	if len(args) > 0 && args[0] == "__upgrade-cleanup" {
		return runUpgradeCleanup(ctx, args[1:], stderr)
	}
	if len(args) > 0 && args[0] == "upgrade" {
		upgrader := dependencies.upgrade
		if upgrader == nil {
			upgrader = upgrade.Run
		}
		return runUpgrade(ctx, args[1:], stdout, stderr, upgrader)
	}
	if dependencies.installSkill == nil {
		dependencies.installSkill = func(io.Writer, string) error { return nil }
	}

	flags := flag.NewFlagSet("roast", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dirty := flags.Bool("dirty", false, "review only uncommitted changes")
	base := flags.String("base", "", "review the current HEAD against a Git ref")
	commit := flags.String("commit", "", "review one commit and its parent")
	repo := flags.String("repo", ".", "Git repository to review")
	bundleDir := flags.String("bundle-dir", "", "write diff.patch, snapshot.tar, and manifest.json to this directory")
	engineName := flags.String("engine", engine.EngineCodex, "review engine: codex, claude, or fake")
	model := flags.String("model", "", "review model (also ROAST_MODEL or ROAST_<ENGINE>_MODEL)")
	thinking := flags.String("thinking", "", "review thinking/effort level (also ROAST_THINKING or ROAST_<ENGINE>_THINKING)")
	maxPriority := flags.String("max-priority", "P1", "include findings at this priority or higher: P0, P1, P2, or P3")
	plain := flags.Bool("plain", false, "use plain output instead of chef voice")
	jsonOutput := flags.String("json-output", "", "write the filtered verdict JSON to this path")
	fakeVerdict := flags.String("fake-verdict", "", "read canned verdict JSON from this file (fake engine only)")
	contextGlob := flags.String("context-glob", "", "include snapshot files matching this context-document glob")
	extraPrompt := flags.String("extra-prompt", "", "append extra review guidance to the prompt")
	intentText := flags.String("intent", "", "the task's intent; the review is judged against this frame")
	intentFile := flags.String("intent-file", "", "read the task's intent from this file (outside the repository)")
	showVersion := flags.Bool("version", false, "print the version")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, version)
		return 0
	}
	maximum, err := verdict.ParsePriority(strings.ToUpper(strings.TrimSpace(*maxPriority)))
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 2
	}
	if *engineName != "fake" && *engineName != engine.EngineCodex && *engineName != engine.EngineClaude {
		fmt.Fprintf(stderr, "roast: [ROAST-ENGINE-NAME] engine %q is unsupported; choose codex, claude, or fake\n", *engineName)
		return 2
	}
	if *intentText != "" && *intentFile != "" {
		fmt.Fprintln(stderr, "roast: [ROAST-INTENT-SOURCE] pass the task's intent once, as --intent <text> or --intent-file <path>, not both")
		return 2
	}
	if flagGiven(flags, "intent") && strings.TrimSpace(*intentText) == "" {
		fmt.Fprintln(stderr, "roast: [ROAST-INTENT-EMPTY] --intent is empty; state the task's intent (the request in one sentence, the intended behavior, the owner boundary, the files touched) or omit the flag")
		return 2
	}
	selectedModel := resolveEngineSetting(*engineName, *model, "MODEL")
	selectedThinking := resolveEngineSetting(*engineName, *thinking, "THINKING")
	selectedBinary := resolveEngineSetting(*engineName, "", "BINARY")
	if *engineName != "fake" {
		if err := engine.ValidateThinking(*engineName, selectedThinking); err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 2
		}
	}

	positional := flags.Args()
	if len(positional) > 1 {
		fmt.Fprintln(stderr, "roast: [ROAST-CLI-ARGS] expected at most one pull request number; example: roast 42")
		return 2
	}
	pullRequest := ""
	if len(positional) == 1 {
		if _, err := strconv.Atoi(positional[0]); err != nil {
			fmt.Fprintf(stderr, "roast: [ROAST-CLI-PR] %q is not a pull request number; example: roast 42\n", positional[0])
			return 2
		}
		pullRequest = positional[0]
	}
	commands := dependencies.commands
	if commands == nil {
		commands = runner.ExecRunner{}
	}
	var selectedEngine engine.ReviewEngine
	engineLabel := "fake/canned"
	if *engineName != "fake" {
		selectedEngine, engineLabel, err = engine.New(*engineName, engine.Options{
			Command:   commands,
			Binary:    selectedBinary,
			Model:     selectedModel,
			Thinking:  selectedThinking,
			Heartbeat: stderr,
		})
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 2
		}
	}
	reviewTarget, err := target.Resolve(ctx, target.Options{
		Dirty:       *dirty,
		Base:        *base,
		Commit:      *commit,
		PullRequest: pullRequest,
		RepoDir:     *repo,
		Runner:      commands,
	})
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	if err := dependencies.installSkill(stderr, reviewTarget.RepoDir); err != nil {
		fmt.Fprintf(stderr, "roast: [ROAST-SKILL-AUTO] %v; continuing review without changing the review target\n", err)
	}
	reviewBundle, err := bundle.Build(ctx, reviewTarget, commands)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	if len(reviewBundle.SecretScanDiff) == 0 {
		fmt.Fprintln(stderr, "roast: nothing to review: make a change or pass --dirty, --base <ref>, --commit <ref>, or a PR number")
		return 0
	}
	if dependencies.scanSecrets != nil {
		if err := dependencies.scanSecrets(ctx, reviewTarget, reviewBundle.SecretScanDiff, reviewBundle.SecretScanSnapshot, commands); err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
	}
	if len(reviewBundle.ExcludedSensitivePaths) > 0 {
		excludedPaths := make([]string, 0, len(reviewBundle.ExcludedSensitivePaths))
		for _, path := range reviewBundle.ExcludedSensitivePaths {
			excludedPaths = append(excludedPaths, displayProvenanceValue(path))
		}
		fmt.Fprintf(stderr, "roast: [ROAST-BUNDLE-SENSITIVE] sensitive changed paths were excluded from the review (%s); remove those files from the change, then rerun roast; no review engine was called\n", strings.Join(excludedPaths, ", "))
		return 1
	}
	if *bundleDir != "" {
		// Bundle.Write rejects repository-local destinations before creating any
		// artifacts, so the post-engine fingerprint cannot observe its output.
		if err := reviewBundle.Write(*bundleDir); err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
	}
	fakeVerdictPath := *fakeVerdict
	jsonOutputPath := *jsonOutput
	if fakeVerdictPath != "" {
		if *engineName != "fake" {
			fmt.Fprintf(stderr, "roast: [ROAST-CLI-FAKE] --fake-verdict is only valid with --engine fake; remove it or select the fake engine explicitly\n")
			return 2
		}
		checkedPath, err := checkedRepositoryArtifact(fakeVerdictPath, reviewTarget.RepoDir, "fake verdict")
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
		fakeVerdictPath = checkedPath
	}
	if jsonOutputPath != "" {
		checkedPath, err := checkedRepositoryArtifact(jsonOutputPath, reviewTarget.RepoDir, "verdict output")
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
		jsonOutputPath = checkedPath
	}
	reviewIntent, err := resolveIntent(*intentText, *intentFile, reviewTarget.RepoDir)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}

	template := roast.DefaultPromptTemplate()
	contextSelection, err := prompt.ContextDocuments(reviewBundle.Snapshot, *contextGlob)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	contextDocuments := contextSelection.Documents
	if notice := contextSelection.ExclusionNotice(); notice != "" {
		fmt.Fprintf(stderr, "roast: %s\n", notice)
	}
	promptDiff, invalidDiffPaths := bundle.SanitizeDiffForPrompt(reviewBundle.Diff, reviewBundle.Snapshot)
	if len(invalidDiffPaths) > 0 {
		displayedPaths := make([]string, 0, len(invalidDiffPaths))
		for _, path := range invalidDiffPaths {
			displayedPaths = append(displayedPaths, strconv.Quote(displayProvenanceValue(path)))
		}
		fmt.Fprintf(stderr, "roast: [ROAST-PROMPT-DIFF] replaced invalid UTF-8 sequences in %s with U+FFFD in the review prompt; the raw diff remains available to local secret scanning\n", strings.Join(displayedPaths, ", "))
	}
	snapshotFiles, err := verdict.SnapshotFiles(reviewBundle.Snapshot)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	lineCounts, err := verdict.SnapshotLineCounts(reviewBundle.Snapshot)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	diffFiles := verdict.DiffFiles(reviewBundle.Diff)
	diffRanges := verdict.DiffLineRanges(reviewBundle.Diff)
	for file := range diffFiles {
		snapshotFiles[file] = struct{}{}
	}
	branch := reviewTarget.HeadRef
	if branch == "" {
		branch = reviewTarget.Label()
	}
	provenance := verdict.Provenance{
		Target:  reviewTarget.Range,
		Branch:  branch,
		Tree:    bundleFingerprint(reviewBundle),
		Engine:  engineLabel,
		Context: contextWithIsolation(contextDocuments, maximum, *contextGlob, *extraPrompt, *engineName) + intentProvenance(reviewIntent),
	}
	promptData := prompt.Data{
		Template:           template,
		ContextDocuments:   contextDocuments,
		VerdictSchema:      verdict.Schema(),
		IncludedPriorities: verdict.IncludedPriorities(maximum),
		Target:             reviewTarget.Range,
		Branch:             branch,
		ExtraPrompt:        *extraPrompt,
		Intent:             reviewIntent,
	}
	promptFrame, err := prompt.Assemble(promptData)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	maximumPromptBytes, err := prompt.MaximumPromptBytes(os.Getenv("ROAST_MAX_PROMPT_BYTES"))
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	sections := bundle.PromptDiffSections(promptDiff)
	widestLabel := prompt.DiffChunk{Index: len(sections), Count: len(sections)}
	chunks, err := prompt.SplitDiff(sections, prompt.ChunkBudget{
		MaximumBytes:    maximumPromptBytes,
		WholeFrameBytes: len(promptFrame) + engineFrameBytes(*engineName, provenance),
		ChunkFrameBytes: len(promptFrame) + engineFrameBytes(*engineName, provenanceForChunk(provenance, widestLabel)),
	})
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	if *engineName != "fake" {
		if preflight, ok := selectedEngine.(engine.PreflightEngine); ok {
			if err := preflight.Preflight(); err != nil {
				fmt.Fprintf(stderr, "roast: %v\n", err)
				return 1
			}
		}
	}
	var snapshotDir string
	var cleanupSnapshot func()
	if *engineName != "fake" {
		snapshotDir, cleanupSnapshot, err = engine.PrepareSnapshot(reviewBundle.Snapshot)
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
		defer func() {
			if cleanupSnapshot != nil {
				cleanupSnapshot()
			}
		}()
	}
	if *engineName == "fake" {
		cannedResponses, readErr := readFakeVerdicts(fakeVerdictPath)
		if readErr != nil {
			fmt.Fprintf(stderr, "roast: %v\n", readErr)
			return 1
		}
		selectedEngine = engine.NewFakeSequence(cannedResponses)
	}

	chunkVerdicts := make([]verdict.Verdict, 0, len(chunks))
	for _, chunk := range chunks {
		chunkProvenance := provenanceForChunk(provenance, chunk)
		promptData.Diff = chunk.PromptDiff()
		reviewPrompt, err := prompt.Assemble(promptData)
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", err)
			return 1
		}
		if chunk.Count > 1 {
			fmt.Fprintf(stderr, "roast: [ROAST-CHUNK] reviewing chunk %d/%d: %d files, %d diff bytes\n", chunk.Index, chunk.Count, len(chunk.Sections), chunk.DiffBytes())
		}
		engineResponse, err := selectedEngine.Review(ctx, engine.Request{
			Prompt:             reviewPrompt,
			Provenance:         chunkProvenance,
			SnapshotDir:        snapshotDir,
			AllowedFiles:       snapshotFiles,
			SnapshotLineCounts: lineCounts,
			DiffLineRanges:     diffRanges,
			MaximumPromptBytes: maximumPromptBytes,
			DiffContributions:  chunk.Contributions(),
		})
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", chunkError(err, chunk))
			return 1
		}
		engineVerdict, err := verdict.Decode(engineResponse)
		if err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", chunkError(err, chunk))
			return 1
		}
		if engineVerdict.Provenance != chunkProvenance {
			fmt.Fprintf(stderr, "roast: [ROAST-VERDICT-PROVENANCE] %s verdict provenance does not describe this bundle; return these exact values in the verdict and retry:\n  target: %s\n  branch: %s\n  tree: %s\n  engine: %s\n  context: %s\n", displayProvenanceValue(engineLabel), displayProvenanceValue(chunkProvenance.Target), displayProvenanceValue(chunkProvenance.Branch), displayProvenanceValue(chunkProvenance.Tree), displayProvenanceValue(chunkProvenance.Engine), displayProvenanceValue(chunkProvenance.Context))
			return 1
		}
		if err := verdict.ValidateWithLocations(engineVerdict, snapshotFiles, lineCounts, diffRanges); err != nil {
			fmt.Fprintf(stderr, "roast: %v\n", chunkError(err, chunk))
			return 1
		}
		chunkVerdicts = append(chunkVerdicts, engineVerdict)
	}
	if cleanupSnapshot != nil {
		cleanupSnapshot()
		cleanupSnapshot = nil
	}
	if err := ensureBundleUnchanged(ctx, reviewTarget, reviewBundle, commands); err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	mergedVerdict := verdict.Merge(chunkVerdicts, provenanceForChange(provenance, chunks))
	if err := verdict.ValidateWithLocations(mergedVerdict, snapshotFiles, lineCounts, diffRanges); err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	filteredVerdict, err := verdict.Filter(mergedVerdict, maximum)
	if err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	if jsonOutputPath != "" {
		encoded, encodeErr := verdict.Encode(filteredVerdict)
		if encodeErr != nil {
			fmt.Fprintf(stderr, "roast: %v\n", encodeErr)
			return 1
		}
		if err := bundle.WritePrivateFile(jsonOutputPath, encoded); err != nil {
			fmt.Fprintf(stderr, "roast: [ROAST-VERDICT-WRITE] cannot write verdict %q: %v; choose a writable output path and retry\n", *jsonOutput, err)
			return 1
		}
	}

	fmt.Fprintf(stdout, "target: %s\n", reviewTarget.Label())
	fmt.Fprintf(stdout, "diff bytes: %d\n", len(reviewBundle.Diff))
	fmt.Fprintf(stdout, "snapshot bytes: %d\n", len(reviewBundle.Snapshot))
	fmt.Fprintf(stdout, "chunks: %s\n", prompt.ChunkSummary(chunks))
	if *bundleDir != "" {
		fmt.Fprintf(stdout, "bundle: %s\n", *bundleDir)
	}
	if jsonOutputPath != "" {
		fmt.Fprintf(stdout, "verdict: %s\n", *jsonOutput)
	}
	if err := output.Print(stderr, filteredVerdict, *plain); err != nil {
		fmt.Fprintf(stderr, "roast: [ROAST-OUTPUT] cannot write verdict summary: %v\n", err)
		return 1
	}
	if filteredVerdict.Overall == verdict.OverallRaw {
		return 1
	}
	return 0
}

func runInstallSkill(args []string, stdout, stderr io.Writer, installer func(io.Writer, bool) error) int {
	force := false
	for _, arg := range args {
		if arg == "--force" {
			force = true
			continue
		}
		fmt.Fprintln(stderr, "roast: [ROAST-SKILL-ARGS] unknown install-skill argument "+fmt.Sprintf("%q", arg)+"; use `roast install-skill [--force]`")
		return 2
	}
	if err := installer(stdout, force); err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	return 0
}

func runUpgrade(ctx context.Context, args []string, stdout, stderr io.Writer, upgrader func(context.Context, string, io.Writer) error) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "roast: [ROAST-UPGRADE-ARGS] upgrade accepts no arguments; use `roast upgrade`\n")
		return 2
	}
	if err := upgrader(ctx, version, stdout); err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	return 0
}

func runUpgradeCleanup(ctx context.Context, args []string, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "roast: [ROAST-UPGRADE-CLEANUP] internal cleanup expected a backup path and parent process ID; rerun `roast upgrade`")
		return 2
	}
	parentProcessID, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "roast: [ROAST-UPGRADE-CLEANUP] parent process ID %q is invalid; expected a positive integer; rerun `roast upgrade`\n", args[1])
		return 2
	}
	if err := upgrade.CleanupPreviousExecutable(ctx, args[0], parentProcessID); err != nil {
		fmt.Fprintf(stderr, "roast: %v\n", err)
		return 1
	}
	return 0
}

func ensureBundleUnchanged(ctx context.Context, reviewTarget target.Target, reviewed bundle.Bundle, commands runner.Runner) error {
	current, err := bundle.Build(ctx, reviewTarget, commands)
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-STALE] cannot verify the reviewed tree after the engine call: %w; retry roast", err)
	}
	expected := bundleFingerprint(reviewed)
	actual := bundleFingerprint(current)
	if actual != expected {
		return fmt.Errorf("[ROAST-BUNDLE-STALE] the reviewed tree changed during the engine call (before %s, after %s); rerun roast against the current worktree", expected, actual)
	}
	if err := ensureTargetReferencesUnchanged(ctx, reviewTarget, commands); err != nil {
		return err
	}
	return nil
}

func ensureTargetReferencesUnchanged(ctx context.Context, reviewed target.Target, commands runner.Runner) error {
	var options target.Options
	switch reviewed.Kind {
	case target.KindDirty:
		options.Dirty = true
	case target.KindBase:
		options.Base = reviewed.BaseRef
	case target.KindCommit:
		options.Commit = reviewed.HeadRef
	case target.KindPullRequest:
		options.PullRequest = strconv.Itoa(reviewed.PRNumber)
	default:
		return nil
	}
	options.RepoDir = reviewed.RepoDir
	options.Runner = commands
	current, err := target.Resolve(ctx, options)
	if err != nil {
		return fmt.Errorf("[ROAST-BUNDLE-STALE] cannot verify the selected target after the engine call: %w; retry roast", err)
	}
	if current.BaseSHA != reviewed.BaseSHA || current.HeadSHA != reviewed.HeadSHA {
		return fmt.Errorf("[ROAST-BUNDLE-STALE] the selected target refs moved during the engine call (before %s..%s, after %s..%s); rerun roast against the current target", reviewed.BaseSHA, reviewed.HeadSHA, current.BaseSHA, current.HeadSHA)
	}
	return nil
}

func resolveEngineSetting(name, flagValue, setting string) string {
	if value := strings.TrimSpace(flagValue); value != "" {
		return value
	}
	engineKey := strings.ToUpper(name)
	for _, key := range []string{"ROAST_" + engineKey + "_" + setting, "ROAST_" + setting} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	if setting == "MODEL" {
		return engine.DefaultModel(name)
	}
	if setting == "BINARY" {
		return ""
	}
	return engine.DefaultThinking(name)
}

// readFakeVerdicts reads the canned verdicts for the fake engine: one JSON
// object for a single call, or a JSON array with one object per chunk.
func readFakeVerdicts(path string) ([][]byte, error) {
	if path == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("[ROAST-ENGINE-FAKE] cannot read canned verdict %q: %w; pass a readable JSON file", path, err)
	}
	var sequence []json.RawMessage
	if err := json.Unmarshal(content, &sequence); err != nil {
		return [][]byte{content}, nil
	}
	responses := make([][]byte, 0, len(sequence))
	for _, response := range sequence {
		responses = append(responses, []byte(response))
	}
	return responses, nil
}

// engineFrameBytes is what a real engine adds around the assembled prompt:
// the provenance contract and the space held back for one validator retry.
// The fake engine sends the prompt as assembled.
func engineFrameBytes(engineName string, provenance verdict.Provenance) int {
	if engineName == "fake" {
		return 0
	}
	return len(engine.PromptWithProvenance("", provenance)) + engine.RetryPromptReserveBytes()
}

// provenanceForChunk names the chunk in the provenance an engine call must
// echo, so a chunk verdict says which part of the change it reviewed. A
// change reviewed in one call keeps the provenance unchanged.
func provenanceForChunk(base verdict.Provenance, chunk prompt.DiffChunk) verdict.Provenance {
	if chunk.Count > 1 {
		base.Context += "; " + chunk.Label()
	}
	return base
}

// provenanceForChange records how a change was chunked in the merged verdict,
// so a verdict never hides that it came from several engine calls.
func provenanceForChange(base verdict.Provenance, chunks []prompt.DiffChunk) verdict.Provenance {
	if len(chunks) > 1 {
		base.Context += "; chunks=" + prompt.ChunkSummary(chunks)
	}
	return base
}

func chunkError(err error, chunk prompt.DiffChunk) error {
	if chunk.Count == 1 {
		return err
	}
	return fmt.Errorf("%w; this was chunk %d/%d, and the verdicts of the earlier chunks are discarded", err, chunk.Index, chunk.Count)
}

func bundleFingerprint(reviewBundle bundle.Bundle) string {
	hash := sha256.New()
	hash.Write([]byte("roast-bundle-v1\x00"))
	writePart := func(part []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		hash.Write(length[:])
		hash.Write(part)
	}
	writePart(reviewBundle.Diff)
	writePart(reviewBundle.Snapshot)
	writePart(reviewBundle.SecretScanDiff)
	writePart(reviewBundle.SecretScanSnapshot)
	excludedSensitivePaths := append([]string(nil), reviewBundle.ExcludedSensitivePaths...)
	sort.Strings(excludedSensitivePaths)
	for _, path := range excludedSensitivePaths {
		writePart([]byte(path))
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func contextDescription(documents []prompt.Document, maximum verdict.Priority, contextGlob, extraPrompt string) string {
	paths := make([]string, 0, len(documents))
	for _, document := range documents {
		paths = append(paths, document.Path)
	}
	return fmt.Sprintf("snapshot + %d project docs [%s]; priorities=%s; context-glob=%q; extra-prompt=%q", len(documents), strings.Join(paths, ", "), verdict.IncludedPriorities(maximum), contextGlob, extraPrompt)
}

func contextWithIsolation(documents []prompt.Document, maximum verdict.Priority, contextGlob, extraPrompt, engineName string) string {
	isolation := "cli-native"
	if engineName == "fake" {
		isolation = "fake"
	}
	return contextDescription(documents, maximum, contextGlob, extraPrompt) + "; isolation=" + isolation
}

func flagGiven(flags *flag.FlagSet, name string) bool {
	given := false
	flags.Visit(func(visited *flag.Flag) {
		if visited.Name == name {
			given = true
		}
	})
	return given
}

// resolveIntent returns the task's intent from the inline flag or from a file
// outside the repository. An empty result means the caller gave no intent.
func resolveIntent(inline, filePath, repoDir string) (string, error) {
	if filePath == "" {
		return strings.TrimSpace(inline), nil
	}
	checkedPath, err := checkedRepositoryArtifact(filePath, repoDir, "intent file")
	if err != nil {
		return "", err
	}
	content, err := os.ReadFile(checkedPath)
	if err != nil {
		return "", fmt.Errorf("[ROAST-INTENT-FILE] cannot read intent file %q: %w; write the task's intent to a readable file outside the repository and retry", filePath, err)
	}
	if !utf8.Valid(content) {
		return "", fmt.Errorf("[ROAST-INTENT-FILE] intent file %q is not valid UTF-8; write the task's intent as UTF-8 text and retry", filePath)
	}
	intent := strings.TrimSpace(string(content))
	if intent == "" {
		return "", fmt.Errorf("[ROAST-INTENT-EMPTY] intent file %q is empty; write the task's intent to it (the request in one sentence, the intended behavior, the owner boundary, the files touched) and retry", filePath)
	}
	return intent, nil
}

const inlineIntentProvenanceBytes = 120

// intentProvenance names the intent a verdict was judged against. A short
// one-line intent travels verbatim; a longer one travels as its SHA-256 digest
// plus its first line, so a verdict never hides what framed it.
func intentProvenance(intent string) string {
	if intent == "" {
		return ""
	}
	firstLine, _, multiline := strings.Cut(intent, "\n")
	if !multiline && len(intent) <= inlineIntentProvenanceBytes {
		return fmt.Sprintf("; intent=%q", intent)
	}
	digest := sha256.Sum256([]byte(intent))
	return fmt.Sprintf("; intent=sha256:%s; intent-first-line=%q", hex.EncodeToString(digest[:]), strings.TrimSpace(firstLine))
}

func displayProvenanceValue(value string) string {
	var displayed strings.Builder
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) || unicode.Is(unicode.Zl, character) || unicode.Is(unicode.Zp, character) {
			fmt.Fprintf(&displayed, "\\u%04x", character)
			continue
		}
		displayed.WriteRune(character)
	}
	return displayed.String()
}

func checkedRepositoryArtifact(pathValue, repoDir, description string) (string, error) {
	code := strings.ToUpper(strings.ReplaceAll(description, " ", "-"))
	resolvedPath, err := bundle.PathOutsideRepository(pathValue, repoDir)
	if err != nil {
		return "", fmt.Errorf("[ROAST-%s] cannot use %s path %q: %w; choose a path outside the repository", code, description, pathValue, err)
	}
	return resolvedPath, nil
}
