package target

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/janiorvalle/roast/internal/runner"
)

type Kind string

const (
	KindDirty       Kind = "dirty"
	KindBase        Kind = "base"
	KindCommit      Kind = "commit"
	KindPullRequest Kind = "pull_request"
)

type Options struct {
	Dirty       bool
	Base        string
	Commit      string
	PullRequest string
	RepoDir     string
	Runner      runner.Runner
}

type Target struct {
	Kind         Kind   `json:"kind"`
	RepoDir      string `json:"-"`
	BaseRef      string `json:"base_ref,omitempty"`
	HeadRef      string `json:"head_ref,omitempty"`
	BaseSHA      string `json:"base_sha,omitempty"`
	HeadSHA      string `json:"head_sha,omitempty"`
	SnapshotRef  string `json:"snapshot_ref"`
	Range        string `json:"range"`
	PRNumber     int    `json:"pr_number,omitempty"`
	PRURL        string `json:"pr_url,omitempty"`
	PRTitle      string `json:"pr_title,omitempty"`
	RootCommit   bool   `json:"root_commit,omitempty"`
	ThreeDotDiff bool   `json:"three_dot_diff,omitempty"`
}

func (t Target) Label() string {
	switch t.Kind {
	case KindDirty:
		return "uncommitted changes"
	case KindCommit:
		return "commit " + t.HeadRef
	case KindPullRequest:
		return fmt.Sprintf("PR #%d", t.PRNumber)
	default:
		return t.Range
	}
}

func (t Target) MarshalJSON() ([]byte, error) {
	type alias Target
	return json.Marshal(alias(t))
}

func Resolve(ctx context.Context, opts Options) (Target, error) {
	git := opts.Runner
	if git == nil {
		git = runner.ExecRunner{}
	}

	selectors := 0
	if opts.Dirty {
		selectors++
	}
	if opts.Base != "" {
		selectors++
	}
	if opts.Commit != "" {
		selectors++
	}
	if opts.PullRequest != "" {
		selectors++
	}
	if selectors > 1 {
		return Target{}, fmt.Errorf("[ROAST-TARGET-CONFLICT] choose exactly one of --dirty, --base <ref>, --commit <ref>, or <pr-number>; combine none of them for auto mode")
	}

	repoDir, err := repositoryRoot(ctx, opts.RepoDir, git)
	if err != nil {
		return Target{}, err
	}

	switch {
	case opts.Dirty:
		return resolveDirty(ctx, repoDir, git)
	case opts.Base != "":
		return resolveBase(ctx, repoDir, opts.Base, git)
	case opts.Commit != "":
		return resolveCommit(ctx, repoDir, opts.Commit, git)
	case opts.PullRequest != "":
		return resolvePullRequest(ctx, repoDir, opts.PullRequest, git, true)
	default:
		return resolveAuto(ctx, repoDir, git)
	}
}

func repositoryRoot(ctx context.Context, dir string, git runner.Runner) (string, error) {
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("[ROAST-TARGET-REPO] cannot resolve repository path %q: %w", dir, err)
	}
	result, err := git.Run(ctx, abs, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("[ROAST-TARGET-REPO] cannot inspect %q: %w; run roast from a Git repository or pass --repo <path>", abs, err)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("[ROAST-TARGET-REPO] %q is not a Git repository: %s; run roast from a Git repository or pass --repo <path>", abs, runner.Failure("git", []string{"rev-parse", "--show-toplevel"}, result))
	}
	root := strings.TrimSpace(string(result.Stdout))
	if root == "" {
		return "", fmt.Errorf("[ROAST-TARGET-REPO] Git returned an empty repository root for %q; pass --repo <path> to a worktree", abs)
	}
	return filepath.Clean(root), nil
}

func resolveDirty(ctx context.Context, repoDir string, git runner.Runner) (Target, error) {
	headSHA, err := resolveRef(ctx, repoDir, "HEAD", git)
	if err != nil {
		return Target{}, err
	}
	return Target{
		Kind:        KindDirty,
		RepoDir:     repoDir,
		BaseRef:     "HEAD",
		HeadRef:     "WORKTREE",
		BaseSHA:     headSHA,
		SnapshotRef: headSHA,
		Range:       "HEAD (uncommitted)",
	}, nil
}

func resolveBase(ctx context.Context, repoDir, base string, git runner.Runner) (Target, error) {
	baseSHA, err := resolveRef(ctx, repoDir, base, git)
	if err != nil {
		return Target{}, fmt.Errorf("%w; pass --base <ref> using a ref that exists locally", err)
	}
	headSHA, err := resolveRef(ctx, repoDir, "HEAD", git)
	if err != nil {
		return Target{}, err
	}
	return Target{
		Kind:         KindBase,
		RepoDir:      repoDir,
		BaseRef:      base,
		HeadRef:      "HEAD",
		BaseSHA:      baseSHA,
		HeadSHA:      headSHA,
		SnapshotRef:  headSHA,
		Range:        base + "...HEAD",
		ThreeDotDiff: true,
	}, nil
}

func resolveCommit(ctx context.Context, repoDir, commit string, git runner.Runner) (Target, error) {
	headSHA, err := resolveRef(ctx, repoDir, commit, git)
	if err != nil {
		return Target{}, fmt.Errorf("%w; pass --commit <ref> using a commit that exists locally", err)
	}
	result, err := git.Run(ctx, repoDir, "git", "cat-file", "-p", headSHA)
	if err != nil {
		return Target{}, fmt.Errorf("[ROAST-TARGET-COMMIT] cannot read parent of %s: %w", headSHA, err)
	}
	if result.ExitCode != 0 {
		return Target{}, fmt.Errorf("[ROAST-TARGET-COMMIT] cannot read parent of %s: %s; fetch enough history to include the commit parent", headSHA, runner.Failure("git", []string{"cat-file", "-p", headSHA}, result))
	}
	parents := commitParents(result.Stdout)
	if len(parents) == 0 {
		return Target{
			Kind:         KindCommit,
			RepoDir:      repoDir,
			HeadRef:      commit,
			HeadSHA:      headSHA,
			SnapshotRef:  headSHA,
			Range:        commit + " (root commit)",
			RootCommit:   true,
			ThreeDotDiff: false,
		}, nil
	}
	parent := parents[0]
	if _, parentErr := tryResolveRef(ctx, repoDir, parent, git); parentErr != nil {
		return Target{}, fmt.Errorf("[ROAST-TARGET-SHALLOW] commit %s has parent %s, but that parent is not available locally; run `git fetch --unshallow` or `git fetch --deepen=1` and retry", headSHA, parent)
	}
	return Target{
		Kind:        KindCommit,
		RepoDir:     repoDir,
		BaseRef:     parent,
		HeadRef:     commit,
		BaseSHA:     parent,
		HeadSHA:     headSHA,
		SnapshotRef: headSHA,
		Range:       commit + "^.." + commit,
	}, nil
}

func commitParents(raw []byte) []string {
	var parents []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line == "" {
			break
		}
		parent, found := strings.CutPrefix(line, "parent ")
		if found && strings.TrimSpace(parent) != "" {
			parents = append(parents, strings.TrimSpace(parent))
		}
	}
	return parents
}

func resolveAuto(ctx context.Context, repoDir string, git runner.Runner) (Target, error) {
	result, err := git.Run(ctx, repoDir, "git", "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return Target{}, fmt.Errorf("[ROAST-TARGET-AUTO] cannot check for uncommitted changes: %w", err)
	}
	if result.ExitCode != 0 {
		return Target{}, fmt.Errorf("[ROAST-TARGET-AUTO] cannot check for uncommitted changes: %s; pass an explicit target after fixing the repository", runner.Failure("git", []string{"status", "--porcelain=v1", "--untracked-files=all"}, result))
	}
	if len(result.Stdout) > 0 {
		return resolveDirty(ctx, repoDir, git)
	}

	if pullRequest, prErr := resolvePullRequest(ctx, repoDir, "", git, false); prErr == nil {
		return pullRequest, nil
	}

	for _, candidate := range []string{"origin/main", "origin/master", "main", "master"} {
		baseSHA, refErr := tryResolveRef(ctx, repoDir, candidate, git)
		if refErr != nil {
			continue
		}
		headSHA, headErr := resolveRef(ctx, repoDir, "HEAD", git)
		if headErr != nil {
			return Target{}, headErr
		}
		return Target{
			Kind:         KindBase,
			RepoDir:      repoDir,
			BaseRef:      candidate,
			HeadRef:      "HEAD",
			BaseSHA:      baseSHA,
			HeadSHA:      headSHA,
			SnapshotRef:  headSHA,
			Range:        candidate + "...HEAD",
			ThreeDotDiff: true,
		}, nil
	}

	return Target{}, fmt.Errorf("[ROAST-TARGET-AUTO] no current PR or local base branch found; fetch origin/main or use --base <ref>, --commit <ref>, or --dirty")
}

type pullRequestMetadata struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	BaseRefName string `json:"baseRefName"`
	HeadRefName string `json:"headRefName"`
	BaseRefOID  string `json:"baseRefOid"`
	HeadRefOID  string `json:"headRefOid"`
	URL         string `json:"url"`
}

func resolvePullRequest(ctx context.Context, repoDir, number string, commands runner.Runner, required bool) (Target, error) {
	args := []string{"pr", "view"}
	if number != "" {
		parsed, err := strconv.Atoi(number)
		if err != nil || parsed <= 0 {
			return Target{}, fmt.Errorf("[ROAST-TARGET-PR] %q is not a valid pull request number; use roast <number>, for example roast 42", number)
		}
		args = append(args, number)
	}
	args = append(args, "--json", "number,title,baseRefName,headRefName,baseRefOid,headRefOid,url")
	result, err := commands.Run(ctx, repoDir, "gh", args...)
	if err != nil {
		if required {
			return Target{}, fmt.Errorf("[ROAST-TARGET-GH] cannot run GitHub CLI: %w; install gh and run `gh auth login`, or use --base <ref> instead", err)
		}
		return Target{}, err
	}
	if result.ExitCode != 0 {
		if required {
			return Target{}, fmt.Errorf("[ROAST-TARGET-GH] GitHub CLI could not load %sPR metadata: %s; install gh and run `gh auth login`, or use --base <ref> instead", prNumberPrefix(number), runner.Failure("gh", args, result))
		}
		return Target{}, runner.Failure("gh", args, result)
	}
	var metadata pullRequestMetadata
	if err := json.Unmarshal(result.Stdout, &metadata); err != nil {
		return Target{}, fmt.Errorf("[ROAST-TARGET-GH] GitHub CLI returned invalid PR JSON: %w; retry `gh pr view %s --json number,title,baseRefName,headRefName,baseRefOid,headRefOid,url`", err, number)
	}
	if metadata.Number <= 0 && number != "" {
		metadata.Number, _ = strconv.Atoi(number)
	}
	if metadata.Number <= 0 || metadata.BaseRefOID == "" || metadata.HeadRefOID == "" {
		return Target{}, fmt.Errorf("[ROAST-TARGET-GH] PR metadata is missing number, baseRefOid, or headRefOid; retry `gh pr view %s --json number,baseRefOid,headRefOid`", number)
	}
	if metadata.BaseRefName == "" {
		metadata.BaseRefName = "base"
	}
	if metadata.HeadRefName == "" {
		metadata.HeadRefName = "head"
	}
	return Target{
		Kind:         KindPullRequest,
		RepoDir:      repoDir,
		BaseRef:      metadata.BaseRefName,
		HeadRef:      metadata.HeadRefName,
		BaseSHA:      metadata.BaseRefOID,
		HeadSHA:      metadata.HeadRefOID,
		SnapshotRef:  metadata.HeadRefOID,
		Range:        fmt.Sprintf("%s...%s", metadata.BaseRefName, metadata.HeadRefName),
		PRNumber:     metadata.Number,
		PRURL:        metadata.URL,
		PRTitle:      metadata.Title,
		ThreeDotDiff: true,
	}, nil
}

func prNumberPrefix(number string) string {
	if number == "" {
		return "current "
	}
	return "PR #" + number + " "
}

func resolveRef(ctx context.Context, repoDir, ref string, git runner.Runner) (string, error) {
	resolved, err := tryResolveRef(ctx, repoDir, ref, git)
	if err != nil {
		return "", fmt.Errorf("[ROAST-TARGET-REF] cannot resolve Git ref %q: %w; fetch it or pass a different ref", ref, err)
	}
	return resolved, nil
}

func tryResolveRef(ctx context.Context, repoDir, ref string, git runner.Runner) (string, error) {
	if strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("ref must not start with '-'")
	}
	args := []string{"rev-parse", "--verify", "--quiet", "--end-of-options", ref + "^{commit}"}
	result, err := git.Run(ctx, repoDir, "git", args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", runner.Failure("git", args, result)
	}
	resolved := strings.TrimSpace(string(result.Stdout))
	if resolved == "" {
		return "", fmt.Errorf("Git returned no commit ID")
	}
	return resolved, nil
}
