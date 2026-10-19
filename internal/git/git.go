// Package git wraps the git commands ghfleet needs on a runner.
package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Repo is a local git checkout.
type Repo struct {
	Dir string
}

// Result is a command result.
type Result struct {
	Stdout string
	Stderr string
	Code   int
}

func (r Result) Err() error {
	if r.Code == 0 {
		return nil
	}
	return fmt.Errorf("git exited %d: %s", r.Code, strings.TrimSpace(r.Stderr))
}

// Run executes git in dir.
func Run(ctx context.Context, dir string, args ...string) (Result, error) {
	all := args
	if _, err := exec.LookPath("gh"); err == nil {
		all = append([]string{"-c", "credential.helper=!gh auth git-credential"}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", all...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.Code = ee.ExitCode()
			return res, res.Err()
		}
		return res, err
	}
	return res, nil
}

// MustRun executes git and returns trimmed stdout.
func MustRun(ctx context.Context, dir string, args ...string) (string, error) {
	res, err := Run(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// Remotes returns remote name -> URL.
func Remotes(ctx context.Context, dir string) (map[string]string, error) {
	out, err := MustRun(ctx, dir, "remote", "-v")
	if err != nil {
		return nil, err
	}
	remotes := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			if _, ok := remotes[fields[0]]; !ok {
				remotes[fields[0]] = fields[1]
			}
		}
	}
	return remotes, nil
}

// RemoteURL returns the URL for a remote, or "".
func RemoteURL(ctx context.Context, dir, name string) string {
	out, err := MustRun(ctx, dir, "remote", "get-url", name)
	if err != nil {
		return ""
	}
	return out
}

// Status describes the working tree.
type Status struct {
	Branch string
	Dirty  int
	Ahead  int
	Behind int
}

// GetStatus reports branch and dirtiness.
func GetStatus(ctx context.Context, dir string) (Status, error) {
	var st Status
	branch, err := MustRun(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return st, err
	}
	st.Branch = branch
	out, err := MustRun(ctx, dir, "status", "--porcelain")
	if err != nil {
		return st, err
	}
	if out != "" {
		st.Dirty = len(strings.Split(out, "\n"))
	}
	return st, nil
}

// HeadSHA returns HEAD.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	return MustRun(ctx, dir, "rev-parse", "HEAD")
}

// Worktree is one entry from `git worktree list`.
type Worktree struct {
	Path   string
	Head   string
	Branch string
	Bare   bool
}

// Worktrees lists worktrees.
func Worktrees(ctx context.Context, dir string) ([]Worktree, error) {
	out, err := MustRun(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	var cur Worktree
	flush := func() {
		if cur.Path != "" {
			list = append(list, cur)
		}
		cur = Worktree{}
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(line, "refs/heads/")
		case line == "bare":
			cur.Bare = true
		case line == "":
			flush()
		}
	}
	flush()
	return list, nil
}

// Fetch fetches refspecs from a remote.
func Fetch(ctx context.Context, dir, remote string, refspecs ...string) error {
	args := []string{"fetch", "--no-tags", remote}
	args = append(args, refspecs...)
	_, err := Run(ctx, dir, args...)
	return err
}

// FetchPR fetches a pull request head and returns the commit SHA.
func FetchPR(ctx context.Context, dir, remote string, number int, headRef string) (string, error) {
	ref := fmt.Sprintf("refs/ghfleet/pr/%d", number)
	prRef := fmt.Sprintf("refs/pull/%d/head", number)
	if _, err := Run(ctx, dir, "fetch", "--no-tags", remote, prRef+":"+ref); err == nil {
		return MustRun(ctx, dir, "rev-parse", ref)
	}
	if headRef != "" {
		local := fmt.Sprintf("refs/ghfleet/head/%d", number)
		if err := Fetch(ctx, dir, remote, headRef+":"+local); err == nil {
			return MustRun(ctx, dir, "rev-parse", local)
		}
	}
	return "", fmt.Errorf("could not fetch PR #%d from %s", number, remote)
}

// FetchRef fetches a ref into a unique local ref and returns its SHA.
func FetchRef(ctx context.Context, dir, remote, ref string) (string, error) {
	sum := sha256.Sum256([]byte(remote + ":" + ref))
	local := "refs/ghfleet/base/" + hex.EncodeToString(sum[:])[:12]
	if err := Fetch(ctx, dir, remote, ref+":"+local); err != nil {
		return "", err
	}
	return MustRun(ctx, dir, "rev-parse", local)
}

// AddWorktree creates a detached worktree at path.
func AddWorktree(ctx context.Context, repoDir, path, ref string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		_ = RemoveWorktree(ctx, repoDir, path, true)
	}
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", path, ref)
	return err
}

// AddWorktreeBranch creates a worktree on a new branch.
func AddWorktreeBranch(ctx context.Context, repoDir, path, branch, start string) error {
	if _, err := os.Stat(path); err == nil {
		_ = RemoveWorktree(ctx, repoDir, path, true)
	}
	_, err := Run(ctx, repoDir, "worktree", "add", "-B", branch, path, start)
	return err
}

// RemoveWorktree removes a worktree and cleans up.
func RemoveWorktree(ctx context.Context, repoDir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, err := Run(ctx, repoDir, args...)
	if err != nil {
		_ = os.RemoveAll(path)
		_, _ = Run(ctx, repoDir, "worktree", "prune")
	}
	return err
}

// PruneWorktrees prunes stale worktree metadata.
func PruneWorktrees(ctx context.Context, repoDir string) error {
	_, err := Run(ctx, repoDir, "worktree", "prune")
	return err
}

// DiffPR returns the PR diff between base and head.
func DiffPR(ctx context.Context, dir, base, head string) (string, error) {
	return MustRun(ctx, dir, "diff", base+"..."+head)
}

// agentArtifacts are directories/files created by agent tooling or build steps.
// They must never be committed to a pull request.
var agentArtifacts = []string{
	".serena/", ".claude/", ".opencode/", ".cursor/", ".continue/", ".specstory/",
	".codex/", ".gemini/", ".windsurf/", ".zed/", ".idea/", ".vscode/",
	".venv/", "__pycache__/", ".pytest_cache/", ".mypy_cache/", ".ruff_cache/",
	"node_modules/", "target/", "dist/", "build/", ".aider",
}

// IsAgentArtifact reports whether path is generated by agent tooling or a build.
func IsAgentArtifact(path string) bool {
	return artifactRoot(path) != ""
}

func artifactRoot(path string) string {
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	for _, prefix := range agentArtifacts {
		trimmed := strings.TrimSuffix(prefix, "/")
		if path == trimmed || strings.HasPrefix(path, trimmed+"/") {
			return trimmed
		}
		if !strings.HasSuffix(prefix, "/") && strings.HasPrefix(path, prefix) {
			return path
		}
	}
	return ""
}

// CleanAgentArtifacts removes untracked agent/tool/build artifacts from a worktree.
func CleanAgentArtifacts(ctx context.Context, dir string) error {
	out, err := MustRun(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, path := range strings.Split(out, "\n") {
		path = strings.TrimSpace(path)
		root := artifactRoot(path)
		if path == "" || root == "" || seen[root] {
			continue
		}
		seen[root] = true
		if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(root))); err != nil {
			return err
		}
	}
	return nil
}

// DiffWorking returns changes against HEAD, including untracked files.
func DiffWorking(ctx context.Context, dir string) (string, error) {
	res, err := Run(ctx, dir, "diff", "HEAD")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(res.Stdout)
	untracked, err := MustRun(ctx, dir, "ls-files", "--others", "--exclude-standard")
	if err != nil || untracked == "" {
		return b.String(), nil
	}
	for _, file := range strings.Split(untracked, "\n") {
		file = strings.TrimSpace(file)
		if file == "" || IsAgentArtifact(file) {
			continue
		}
		// git diff --no-index exits 1 when the files differ; that is expected.
		out, _ := Run(ctx, dir, "diff", "--no-index", "--", "/dev/null", file)
		b.WriteString(out.Stdout)
	}
	return b.String(), nil
}

// DiffStat returns a diffstat against HEAD.
func DiffStat(ctx context.Context, dir string) (string, error) {
	return MustRun(ctx, dir, "diff", "--stat", "HEAD")
}

// IsDirty reports whether the worktree differs from HEAD.
func IsDirty(ctx context.Context, dir string) (bool, error) {
	out, err := MustRun(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// CommitAll stages and commits everything. It returns the new SHA.
func CommitAll(ctx context.Context, dir, message string) (string, error) {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return "", err
	}
	if res, _ := Run(ctx, dir, "diff", "--cached", "--quiet"); res.Code == 0 {
		return HeadSHA(ctx, dir)
	}
	if _, err := Run(ctx, dir, "commit", "-m", message); err != nil {
		return "", err
	}
	return HeadSHA(ctx, dir)
}

// Push pushes a refspec.
func Push(ctx context.Context, dir, remote, refspec string, forceWithLease bool) error {
	args := []string{"push", "--porcelain"}
	if forceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remote, refspec)
	_, err := Run(ctx, dir, args...)
	return err
}

// PushLease pushes with an explicit force-with-lease expected value.
func PushLease(ctx context.Context, dir, remote, refspec, leaseRef, expectedSHA string) error {
	args := []string{"push", "--porcelain", "--force-with-lease=" + leaseRef + ":" + expectedSHA, remote, refspec}
	_, err := Run(ctx, dir, args...)
	return err
}

// Rebase rebases the current branch onto onto.
func Rebase(ctx context.Context, dir, onto string) error {
	_, err := Run(ctx, dir, "rebase", onto)
	return err
}

// RebaseInProgress reports whether a rebase is in progress.
func RebaseInProgress(ctx context.Context, dir string) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(dir, ".git", p)); err == nil {
			return true
		}
	}
	// Worktrees keep .git as a file pointing at the real git dir.
	out, err := MustRun(ctx, dir, "rev-parse", "--git-path", "rebase-merge")
	if err == nil {
		if !filepath.IsAbs(out) {
			out = filepath.Join(dir, out)
		}
		if _, statErr := os.Stat(out); statErr == nil {
			return true
		}
	}
	return false
}

// AbortRebase aborts a rebase.
func AbortRebase(ctx context.Context, dir string) error {
	_, err := Run(ctx, dir, "rebase", "--abort")
	return err
}

// ContinueRebase continues a rebase.
func ContinueRebase(ctx context.Context, dir string) error {
	_, err := Run(ctx, dir, "rebase", "--continue")
	return err
}

// MergeFFOnly fast-forwards the current branch.
// Merge creates a merge commit (never fast-forward) for ref.
func Merge(ctx context.Context, dir, ref string) error {
	_, err := MustRun(ctx, dir, "merge", "--no-ff", "--no-edit", ref)
	return err
}

// MergeInProgress reports whether the worktree has an unfinished merge.
func MergeInProgress(ctx context.Context, dir string) bool {
	res, _ := Run(ctx, dir, "rev-parse", "--verify", "-q", "MERGE_HEAD")
	return res.Err() == nil
}

// CommitMerge completes an in-progress merge after conflicts were resolved.
func CommitMerge(ctx context.Context, dir string) error {
	if _, err := MustRun(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err := MustRun(ctx, dir, "commit", "--no-edit")
	return err
}

// UnmergedFiles lists paths with unresolved conflicts.
func UnmergedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := MustRun(ctx, dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

// DiffStat returns the diffstat between two revisions.
func DiffStatRefs(ctx context.Context, dir, oldRef, newRef string) (string, error) {
	return MustRun(ctx, dir, "diff", "--stat", oldRef, newRef)
}

// RemergeDiff shows the conflict-resolution delta of a merge commit.
func RemergeDiff(ctx context.Context, dir string) (string, error) {
	return MustRun(ctx, dir, "show", "--remerge-diff", "--format=", "HEAD")
}

func MergeFFOnly(ctx context.Context, dir, ref string) error {
	_, err := Run(ctx, dir, "merge", "--ff-only", ref)
	return err
}

// MergeBase returns the merge base of two commits.
func MergeBase(ctx context.Context, dir, a, b string) (string, error) {
	return MustRun(ctx, dir, "merge-base", a, b)
}

// RangeDiff summarizes the difference between two commit ranges.
func RangeDiff(ctx context.Context, dir, oldBase, oldHead, newBase, newHead string) (string, error) {
	return MustRun(ctx, dir, "range-diff", oldBase+".."+oldHead, newBase+".."+newHead)
}

// ShortSHA shortens a SHA for display.
func ShortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// ParseAheadBehind is a helper for `git rev-list --left-right --count`.
func ParseAheadBehind(s string) (ahead, behind int) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0
	}
	ahead, _ = strconv.Atoi(fields[0])
	behind, _ = strconv.Atoi(fields[1])
	return ahead, behind
}

// CommandTimeout returns a context with the default git timeout.
func CommandTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		d = 5 * time.Minute
	}
	return context.WithTimeout(parent, d)
}
