// Package runner executes jobs on a machine that has the repositories checked out.
package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/agent"
	"github.com/alytaphoenix/ghfleet/internal/client"
	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/git"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

// Runner executes jobs for one server.
type Runner struct {
	Cfg    *config.RunnerConfig
	Client *client.Client
	Log    *slog.Logger
	ID     string
	Agent  *agent.Runner
}

// New creates a runner.
func New(cfg *config.RunnerConfig, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{
		Cfg:    cfg,
		Client: client.New(cfg.ServerURL, cfg.Token),
		Log:    log,
		ID:     cfg.ID,
		Agent:  &agent.Runner{Cfg: cfg.Agent, Log: log},
	}
}

// Run registers and then starts the configured number of workers.
func (r *Runner) Run(ctx context.Context) error {
	for {
		err := r.register(ctx)
		if err == nil {
			break
		}
		r.Log.Warn("runner registration failed; retrying", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	go r.heartbeatLoop(ctx)
	concurrency := r.Cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}
	for i := 0; i < concurrency; i++ {
		go r.worker(ctx, i+1)
	}
	<-ctx.Done()
	return nil
}

func (r *Runner) register(ctx context.Context) error {
	host, _ := os.Hostname()
	info := server.RunnerInfo{
		ID: r.ID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Labels: r.Cfg.Labels, Repos: r.repoNames(), Version: server.Version,
	}
	if err := r.Client.Register(ctx, info); err != nil {
		return fmt.Errorf("register runner: %w", err)
	}
	r.Log.Info("runner registered", "id", r.ID, "server", r.Cfg.ServerURL, "labels", r.Cfg.Labels, "repos", info.Repos)
	return nil
}

func (r *Runner) repoNames() []string {
	out := make([]string, 0, len(r.Cfg.Repos))
	for _, repo := range r.Cfg.Repos {
		out = append(out, repo.Name)
	}
	return out
}

func (r *Runner) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.Client.Heartbeat(ctx, r.ID); err != nil {
				r.Log.Warn("heartbeat failed", "err", err)
				if err := r.register(ctx); err != nil {
					r.Log.Warn("re-register failed", "err", err)
				}
			}
		}
	}
}

func (r *Runner) worker(ctx context.Context, n int) {
	baseLog := r.Log.With("worker", n)
	// Give each worker its own identity so two workers on one machine cannot
	// claim or apply the same approved job.
	wr := *r
	wr.ID = fmt.Sprintf("%s#%d", r.ID, n)
	wr.Log = baseLog
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, err := wr.Client.Claim(ctx, wr.ID, wr.Cfg.Labels, wr.repoNames())
		if err != nil {
			baseLog.Warn("claim failed", "err", err)
			sleep(ctx, 5*time.Second)
			continue
		}
		if job == nil {
			sleep(ctx, 5*time.Second)
			continue
		}
		log := baseLog.With("job", job.Job.ID)
		log.Info("job claimed", "kind", job.Job.Kind, "repo", job.Job.Repo, "pr", job.Job.PRNumber, "status", job.Job.Status)
		if err := wr.execute(ctx, *job, log); err != nil {
			log.Error("job failed", "err", err)
			_ = wr.Client.Failed(ctx, job.Job.ID, wr.ID, err.Error())
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func (r *Runner) execute(ctx context.Context, detail server.JobDetail, log *slog.Logger) error {
	job := detail.Job
	var spec jobs.Spec
	if err := json.Unmarshal([]byte(job.SpecJSON), &spec); err != nil {
		return fmt.Errorf("decode job spec: %w", err)
	}
	repo, ok := r.Cfg.RunnerRepoByName(job.Repo)
	if !ok {
		return fmt.Errorf("runner %s does not have repo %q", r.ID, job.Repo)
	}

	// If the job is already approved (runner restarted while waiting), just apply.
	if job.Status == "approved" {
		return r.applyApproved(ctx, job, spec, repo, log)
	}

	if err := r.Client.Start(ctx, job.ID, r.ID); err != nil {
		return fmt.Errorf("start job: %w", err)
	}

	if job.Kind == jobs.KindForkSync {
		return r.runForkSync(ctx, job, spec, repo, log)
	}
	if job.Kind == jobs.KindSecurity {
		return r.runSecurity(ctx, job, spec, repo, log)
	}
	return r.runAgentJob(ctx, job, spec, repo, log)
}

func (r *Runner) runAgentJob(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, log *slog.Logger) error {
	worktree := filepath.Join(r.Cfg.WorkRoot, "jobs", job.ID)
	defer r.cleanup(ctx, repo, worktree, log)

	fetchRemote := firstNonEmpty(repo.FetchRemote, spec.Repo.FetchRemote, "origin")
	pushRemote := firstNonEmpty(repo.PushRemote, spec.Repo.PushRemote, "origin")

	headSHA, err := git.FetchPR(ctx, repo.Path, fetchRemote, spec.PR.Number, spec.PR.HeadRef)
	if err != nil {
		// Some checkouts keep the fork under a different remote name.
		headSHA, err = git.FetchPR(ctx, repo.Path, pushRemote, spec.PR.Number, spec.PR.HeadRef)
	}
	if err != nil {
		return fmt.Errorf("fetch PR: %w", err)
	}
	if spec.PR.HeadSHA != "" && headSHA != spec.PR.HeadSHA {
		return fmt.Errorf("PR head changed: job expects %s, fetched %s", git.ShortSHA(spec.PR.HeadSHA), git.ShortSHA(headSHA))
	}

	base := spec.PR.BaseSHA
	if base == "" {
		base = "refs/remotes/" + fetchRemote + "/" + spec.PR.BaseRef
	}
	if _, err := git.MustRun(ctx, repo.Path, "cat-file", "-e", base+"^{commit}"); err != nil {
		fetched, fetchErr := git.FetchRef(ctx, repo.Path, fetchRemote, spec.PR.BaseRef)
		if fetchErr != nil {
			return fmt.Errorf("fetch base %s: %w", spec.PR.BaseRef, fetchErr)
		}
		base = fetched
	}

	if spec.Kind == jobs.KindRebase || spec.Kind == jobs.KindUpstreamMerge {
		if err := git.AddWorktreeBranch(ctx, repo.Path, worktree, "ghfleet/"+job.ID, headSHA); err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}
	} else {
		if err := git.AddWorktree(ctx, repo.Path, worktree, headSHA); err != nil {
			return fmt.Errorf("create worktree: %w", err)
		}
	}

	diff, _ := git.DiffPR(ctx, worktree, base, headSHA)
	guidelines := readGuidelines(worktree)

	var result jobs.Result
	if spec.Kind == jobs.KindRebase {
		rebased := git.Rebase(ctx, worktree, base) == nil
		if rebased {
			result = jobs.Result{Decision: "fix", Summary: "Rebased onto the current base with no conflicts.", Risk: "low"}
		} else if !git.RebaseInProgress(ctx, worktree) {
			return fmt.Errorf("rebase failed without conflict state")
		}
	}
	if spec.Kind == jobs.KindUpstreamMerge && result.Decision == "" {
		if err := git.Merge(ctx, worktree, base); err == nil {
			if newHead, _ := git.HeadSHA(ctx, worktree); newHead == headSHA {
				result = jobs.Result{Decision: "noop", Summary: "PR branch already contains the current base; nothing to merge.", Risk: "low", Silent: true}
			} else {
				result = jobs.Result{Decision: "fix", Summary: "Merged the current base into the PR branch with no conflicts.", Risk: "low"}
			}
		} else if !git.MergeInProgress(ctx, worktree) {
			return fmt.Errorf("merge failed without an in-progress merge state")
		}
	}
	if result.Decision == "" {
		ares, err := r.Agent.Run(ctx, agent.Request{
			Kind: job.Kind, Workdir: worktree, Spec: spec, Diff: diff, Guidelines: guidelines,
			CheckCommands: repo.Checks,
		})
		if err != nil {
			return err
		}
		if err := json.Unmarshal(ares.JSON, &result); err != nil {
			return fmt.Errorf("decode agent result: %w", err)
		}
		if result.Decision == "" {
			result = jobs.Result{Decision: "escalate", Summary: "agent returned no decision", Risk: "high", NeedsHuman: true}
		}
		if result.Risk == "" {
			result.Risk = "medium"
		}
		result.CostUSD = ares.CostUSD
		log.Info("agent finished", "decision", result.Decision, "risk", result.Risk, "cost_usd", ares.CostUSD, "turns", ares.Turns)
	}

	_ = git.CleanAgentArtifacts(ctx, worktree)
	if spec.Kind == jobs.KindRebase && git.RebaseInProgress(ctx, worktree) {
		result.Decision = "escalate"
		result.NeedsHuman = true
		result.Risk = "high"
		result.Summary = strings.TrimSpace(result.Summary + " (rebase is still in progress)")
	}
	if spec.Kind == jobs.KindUpstreamMerge && git.MergeInProgress(ctx, worktree) {
		if unmerged, _ := git.UnmergedFiles(ctx, worktree); len(unmerged) > 0 {
			result.Decision = "escalate"
			result.NeedsHuman = true
			result.Risk = "high"
			result.Summary = strings.TrimSpace(result.Summary + " (merge still has unresolved files: " + strings.Join(unmerged, ", ") + ")")
		} else if err := git.CommitMerge(ctx, worktree); err != nil {
			return fmt.Errorf("complete merge commit: %w", err)
		}
	}

	checks := repo.Checks
	if job.Kind == jobs.KindIncomingReview {
		checks = nil
	}
	if len(checks) == 0 && job.Kind != jobs.KindIncomingReview {
		result.NeedsHuman = true
		result.Summary = strings.TrimSpace(result.Summary + " (no local checks configured)")
	}
	checksOut, checksOK := r.runChecks(ctx, worktree, checks, repo.VerifyTimeout.D())
	_ = git.CleanAgentArtifacts(ctx, worktree)
	result.ChecksOutput = checksOut
	result.Verification = append(result.Verification, checks...)
	if !checksOK {
		result.NeedsHuman = true
		result.Risk = "high"
		result.Summary = strings.TrimSpace(result.Summary + " (local verification failed)")
	}

	if spec.Kind != jobs.KindRebase && spec.Kind != jobs.KindUpstreamMerge {
		if newHead, err := git.HeadSHA(ctx, worktree); err == nil && newHead != headSHA {
			return fmt.Errorf("agent changed HEAD (commits are not allowed; leave changes in the working tree)")
		}
	}

	var patch string
	if spec.Kind == jobs.KindRebase {
		newHead, _ := git.HeadSHA(ctx, worktree)
		if oldBase, err := git.MergeBase(ctx, worktree, spec.PR.HeadSHA, base); err == nil {
			// Show only how the rebased patch series changed (conflict
			// resolution), not the entire upstream delta.
			patch, _ = git.RangeDiff(ctx, worktree, oldBase, spec.PR.HeadSHA, base, newHead)
		}
	} else if spec.Kind == jobs.KindUpstreamMerge && result.Decision != "noop" {
		// Show the conflict-resolution delta, not the whole upstream merge.
		patch, _ = git.RemergeDiff(ctx, worktree)
		if strings.TrimSpace(patch) == "" {
			patch, _ = git.DiffStatRefs(ctx, worktree, base, "HEAD")
		}
	} else {
		patch, _ = git.DiffWorking(ctx, worktree)
	}
	result.PatchSize = int64(len(patch))
	if patch != "" {
		sum := sha256.Sum256([]byte(patch))
		result.PatchSHA256 = hex.EncodeToString(sum[:])
		if err := r.Client.UploadArtifact(ctx, job.ID, "patch", []byte(patch)); err != nil {
			return fmt.Errorf("upload patch: %w", err)
		}
	}
	result.Worktree = worktree

	payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
	if err := r.Client.Result(ctx, job.ID, payload); err != nil {
		return fmt.Errorf("upload result: %w", err)
	}
	log.Info("job awaiting approval", "summary", result.Summary, "patch_bytes", len(patch))

	if err := r.waitAndApply(ctx, job, spec, repo, worktree, result, pushRemote, log); err != nil {
		return err
	}
	return nil
}

func (r *Runner) applyApproved(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, log *slog.Logger) error {
	result := jobs.ParseResult(job.ResultJSON)
	worktree := result.Worktree
	if worktree == "" {
		worktree = filepath.Join(r.Cfg.WorkRoot, "jobs", job.ID)
	}
	pushRemote := firstNonEmpty(repo.PushRemote, spec.Repo.PushRemote, "origin")
	if job.Kind == jobs.KindForkSync && strings.EqualFold(result.Decision, "noop") {
		return r.applyForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
	}
	if job.Kind != jobs.KindRebase && job.Kind != jobs.KindForkSync && job.Kind != jobs.KindUpstreamMerge && result.PatchSize == 0 {
		// Reply-only / no-op jobs do not need the worktree to apply.
		result.Applied = true
		payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
		return r.Client.Applied(ctx, job.ID, payload)
	}
	if _, err := os.Stat(worktree); err != nil {
		return fmt.Errorf("approved job %s has no worktree at %s; deny it and run a new job", job.ID, worktree)
	}
	defer r.cleanup(ctx, repo, worktree, log)
	if job.Kind == jobs.KindForkSync {
		return r.applyForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
	}
	return r.applyPush(ctx, job, spec, worktree, result, pushRemote, log)
}

func (r *Runner) waitAndApply(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, worktree string, result jobs.Result, pushRemote string, log *slog.Logger) error {
	timeout := r.Cfg.ApprovalTimeout.D()
	if timeout <= 0 {
		timeout = 24 * time.Hour
	}
	deadline := time.Now().Add(timeout)
	for {
		instr, err := r.Client.Instruction(ctx, job.ID, r.ID)
		if err == nil {
			switch instr.Action {
			case "apply":
				if job.Kind == jobs.KindForkSync {
					return r.applyForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
				}
				return r.applyPush(ctx, job, spec, worktree, result, pushRemote, log)
			case "deny":
				log.Info("job denied", "job", job.ID)
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("approval timed out after %s", timeout)
		}
		sleep(ctx, 5*time.Second)
	}
}

func (r *Runner) applyPush(ctx context.Context, job store.Job, spec jobs.Spec, worktree string, result jobs.Result, pushRemote string, log *slog.Logger) error {
	if job.Kind == jobs.KindIncomingReview || job.Kind == jobs.KindPreflight {
		result.Applied = true
		payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
		return r.Client.Applied(ctx, job.ID, payload)
	}
	if strings.EqualFold(result.Decision, "escalate") {
		result.Applied = true
		payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
		return r.Client.Applied(ctx, job.ID, payload)
	}
	if job.Kind == jobs.KindRebase && git.RebaseInProgress(ctx, worktree) {
		return fmt.Errorf("cannot apply: rebase is still in progress")
	}
	if job.Kind == jobs.KindUpstreamMerge && git.MergeInProgress(ctx, worktree) {
		return fmt.Errorf("cannot apply: merge is still in progress")
	}
	dirty, err := git.IsDirty(ctx, worktree)
	if err != nil {
		return err
	}
	shouldPush := dirty && (result.Decision == "fix" || result.Decision == "" || job.Kind == jobs.KindRebase)
	if dirty {
		msg := "ghfleet: " + firstLine(result.Summary)
		sha, err := git.CommitAll(ctx, worktree, msg)
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		result.CommitSHA = sha
	}
	if job.Kind == jobs.KindRebase || job.Kind == jobs.KindUpstreamMerge {
		if sha, err := git.HeadSHA(ctx, worktree); err == nil {
			result.CommitSHA = sha
		}
	}
	if spec.PR.HeadRef != "" && (shouldPush || job.Kind == jobs.KindRebase || (job.Kind == jobs.KindUpstreamMerge && result.Decision != "noop")) {
		ref := branchRef(spec.PR.HeadRef)
		if job.Kind == jobs.KindRebase {
			if err := git.PushLease(ctx, worktree, pushRemote, "HEAD:"+ref, ref, spec.PR.HeadSHA); err != nil {
				return fmt.Errorf("push: %w", err)
			}
		} else if err := git.Push(ctx, worktree, pushRemote, "HEAD:"+ref, false); err != nil {
			return fmt.Errorf("push: %w", err)
		}
	}
	result.Applied = true
	payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
	if err := r.Client.Applied(ctx, job.ID, payload); err != nil {
		return fmt.Errorf("report applied: %w", err)
	}
	log.Info("job applied", "commit", git.ShortSHA(result.CommitSHA))
	return nil
}

func (r *Runner) runSecurity(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, log *slog.Logger) error {
	worktree := filepath.Join(r.Cfg.WorkRoot, "jobs", job.ID)
	defer r.cleanup(ctx, repo, worktree, log)
	fetchRemote := firstNonEmpty(repo.FetchRemote, spec.Repo.FetchRemote, "origin")
	pushRemote := firstNonEmpty(repo.PushRemote, spec.Repo.PushRemote, "origin")
	branch := firstNonEmpty(repo.DefaultBranch, spec.Repo.DefaultBranch, "main")

	sha, err := git.FetchRef(ctx, repo.Path, pushRemote, branch)
	if err != nil {
		sha, err = git.FetchRef(ctx, repo.Path, fetchRemote, branch)
	}
	if err != nil {
		return fmt.Errorf("fetch %s: %w", branch, err)
	}
	if err := git.AddWorktree(ctx, repo.Path, worktree, sha); err != nil {
		return fmt.Errorf("create security worktree: %w", err)
	}

	scanOut, code, scanErr := r.runScanner(ctx, worktree, r.Cfg.Security.Command, r.Cfg.Security.Timeout.D())
	findings := countOSVVulns(scanOut)
	noSources := strings.Contains(scanOut, "No package sources found")
	if scanErr != nil && code != 1 && !noSources {
		return fmt.Errorf("security scan failed: %w: %s", scanErr, tail(scanOut, 3000))
	}
	if securityScanClean(findings, code, scanOut) {
		summary := "No known dependency vulnerabilities found."
		if noSources {
			summary = "No supported dependency manifest found; nothing to scan."
		}
		result := jobs.Result{
			Decision: "noop", Summary: summary,
			Risk: "low", Silent: true,
		}
		payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
		if err := r.Client.Result(ctx, job.ID, payload); err != nil {
			return err
		}
		return r.waitAndApply(ctx, job, spec, repo, worktree, result, pushRemote, log)
	}

	agentSpec := spec
	agentSpec.Instructions = strings.TrimSpace(spec.Instructions) + "\n\nScanner output:\n```\n" + truncate(scanOut, 120000) + "\n```"
	ares, err := r.Agent.Run(ctx, agent.Request{
		Kind: jobs.KindSecurity, Workdir: worktree, Spec: agentSpec,
		Guidelines: readGuidelines(worktree), CheckCommands: repo.Checks,
	})
	if err != nil {
		return err
	}
	var result jobs.Result
	if err := json.Unmarshal(ares.JSON, &result); err != nil {
		return fmt.Errorf("decode security agent result: %w", err)
	}
	if result.Decision == "" {
		result = jobs.Result{Decision: "escalate", Summary: "security agent returned no decision", Risk: "high", NeedsHuman: true}
	}
	if result.Risk == "" {
		result.Risk = "medium"
	}
	result.CostUSD = ares.CostUSD
	if result.Summary == "" {
		result.Summary = fmt.Sprintf("Scanned dependencies; %d vulnerability group(s) reported.", findings)
	}

	checksOut, checksOK := r.runChecks(ctx, worktree, repo.Checks, repo.VerifyTimeout.D())
	_ = git.CleanAgentArtifacts(ctx, worktree)
	result.ChecksOutput = checksOut
	result.Verification = append(result.Verification, repo.Checks...)
	if !checksOK {
		result.NeedsHuman = true
		result.Risk = "high"
		result.Summary = strings.TrimSpace(result.Summary + " (local verification failed)")
	}
	patch, _ := git.DiffWorking(ctx, worktree)
	result.PatchSize = int64(len(patch))
	if patch != "" {
		sum := sha256.Sum256([]byte(patch))
		result.PatchSHA256 = hex.EncodeToString(sum[:])
		if err := r.Client.UploadArtifact(ctx, job.ID, "patch", []byte(patch)); err != nil {
			return fmt.Errorf("upload patch: %w", err)
		}
	}
	// Re-scan after the dependency changes. A patch that still leaves
	// findings is not a safe auto-update; surface it for a human instead of
	// letting the approval prompt imply the CVEs are resolved.
	rescanOut, rescanCode, rescanErr := r.runScanner(ctx, worktree, r.Cfg.Security.Command, r.Cfg.Security.Timeout.D())
	rescanFindings := countOSVVulns(rescanOut)
	rescanNoSources := strings.Contains(rescanOut, "No package sources found")
	if rescanErr != nil && rescanCode != 1 && !rescanNoSources {
		result.NeedsHuman = true
		result.Risk = "high"
		result.Summary = strings.TrimSpace(result.Summary + " (post-fix security re-scan failed)")
	} else if rescanFindings > 0 && !rescanNoSources {
		result.NeedsHuman = true
		result.Risk = "high"
		result.Summary = strings.TrimSpace(result.Summary + fmt.Sprintf(" (post-fix re-scan still reports %d vulnerability group(s))", rescanFindings))
	}
	if strings.TrimSpace(rescanOut) != "" {
		_ = r.Client.UploadArtifact(ctx, job.ID, "security-rescan", []byte(rescanOut))
	}
	result.Worktree = worktree
	payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
	if err := r.Client.Result(ctx, job.ID, payload); err != nil {
		return err
	}
	log.Info("security job awaiting approval", "summary", result.Summary, "patch_bytes", len(patch))
	return r.waitAndApply(ctx, job, spec, repo, worktree, result, pushRemote, log)
}

func (r *Runner) runScanner(ctx context.Context, dir, command string, timeout time.Duration) (string, int, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = append(safeEnv(), "PWD="+dir, "CI=1", "GHFLEET=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			return string(out), -1, err
		}
	}
	if cctx.Err() == context.DeadlineExceeded {
		return string(out), code, fmt.Errorf("scanner timed out after %s", timeout)
	}
	return string(out), code, err
}

func securityScanClean(findings, code int, output string) bool {
	return findings == 0 && (code == 0 || strings.Contains(output, "No package sources found"))
}

func countOSVVulns(raw string) int {
	var report struct {
		Results []struct {
			Packages []struct {
				Vulnerabilities []json.RawMessage `json:"vulnerabilities"`
			} `json:"packages"`
		} `json:"results"`
	}
	if json.Unmarshal([]byte(raw), &report) != nil {
		return 0
	}
	n := 0
	for _, r := range report.Results {
		for _, p := range r.Packages {
			n += len(p.Vulnerabilities)
		}
	}
	return n
}

func (r *Runner) runForkSync(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, log *slog.Logger) error {
	worktree := filepath.Join(r.Cfg.WorkRoot, "jobs", job.ID)
	defer r.cleanup(ctx, repo, worktree, log)
	fetchRemote := firstNonEmpty(repo.FetchRemote, spec.Repo.FetchRemote, "origin")
	pushRemote := firstNonEmpty(repo.PushRemote, spec.Repo.PushRemote, "origin")
	branch := firstNonEmpty(repo.DefaultBranch, spec.Repo.DefaultBranch, "main")

	upstreamSHA, err := git.FetchRef(ctx, repo.Path, fetchRemote, branch)
	if err != nil {
		return fmt.Errorf("fetch upstream %s: %w", branch, err)
	}
	forkSHA, err := git.FetchRef(ctx, repo.Path, pushRemote, branch)
	if err != nil {
		return fmt.Errorf("fetch fork %s: %w", branch, err)
	}
	if upstreamSHA == forkSHA {
		result := jobs.Result{Decision: "noop", Summary: "Fork is already up to date.", Risk: "low"}
		return r.finishForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
	}
	if err := git.AddWorktree(ctx, repo.Path, worktree, forkSHA); err != nil {
		return fmt.Errorf("create worktree: %w", err)
	}
	if err := git.MergeFFOnly(ctx, worktree, upstreamSHA); err != nil {
		result := jobs.Result{
			Decision: "escalate", Risk: "high", NeedsHuman: true,
			Summary: "Fork has diverged from upstream; refusing to force-push. Resolve manually.",
		}
		return r.finishForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
	}
	result := jobs.Result{
		Decision: "fix", Risk: "low",
		Summary: "Fast-forwarded fork to upstream " + git.ShortSHA(upstreamSHA) + ".",
	}
	return r.finishForkSync(ctx, job, spec, repo, worktree, result, pushRemote, log)
}

func (r *Runner) finishForkSync(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, worktree string, result jobs.Result, pushRemote string, log *slog.Logger) error {
	payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
	if err := r.Client.Result(ctx, job.ID, payload); err != nil {
		return fmt.Errorf("upload result: %w", err)
	}
	if result.Decision == "escalate" || result.NeedsHuman {
		result.Applied = true
		payload.Result = result
		return r.Client.Applied(ctx, job.ID, payload)
	}
	return r.waitAndApply(ctx, job, spec, repo, worktree, result, pushRemote, log)
}

func (r *Runner) applyForkSync(ctx context.Context, job store.Job, spec jobs.Spec, repo config.RunnerRepo, worktree string, result jobs.Result, pushRemote string, log *slog.Logger) error {
	if result.Decision == "escalate" || result.NeedsHuman {
		result.Applied = true
		payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
		return r.Client.Applied(ctx, job.ID, payload)
	}
	branch := firstNonEmpty(repo.DefaultBranch, spec.Repo.DefaultBranch, "main")
	if result.Decision != "noop" {
		if sha, err := git.HeadSHA(ctx, worktree); err == nil {
			result.CommitSHA = sha
		}
		if err := git.Push(ctx, worktree, pushRemote, "HEAD:"+branchRef(branch), false); err != nil {
			return fmt.Errorf("push fork: %w", err)
		}
	}
	result.Applied = true
	payload := server.RunnerResult{RunnerID: r.ID, Result: result, Risk: result.Risk, Summary: result.Summary}
	if err := r.Client.Applied(ctx, job.ID, payload); err != nil {
		return err
	}
	log.Info("fork synced", "branch", branch)
	return nil
}

func (r *Runner) cleanup(ctx context.Context, repo config.RunnerRepo, worktree string, log *slog.Logger) {
	if worktree == "" {
		return
	}
	if _, err := os.Stat(worktree); err != nil {
		return
	}
	if r.Cfg.KeepWorktrees {
		log.Info("keeping worktree", "path", worktree)
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := git.RemoveWorktree(cleanupCtx, repo.Path, worktree, true); err != nil {
		log.Warn("worktree cleanup failed", "path", worktree, "err", err)
	}
	branch := "ghfleet/" + filepath.Base(worktree)
	_, _ = git.Run(cleanupCtx, repo.Path, "branch", "-D", branch)
}

func (r *Runner) runChecks(ctx context.Context, dir string, checks []string, timeout time.Duration) (string, bool) {
	if len(checks) == 0 {
		return "", true
	}
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	var b strings.Builder
	for _, command := range checks {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		cmd := exec.CommandContext(cctx, "/bin/sh", "-c", command)
		cmd.Dir = dir
		cmd.WaitDelay = 10 * time.Second
		cmd.Env = append(safeEnv(), "PWD="+dir, "CI=1", "GHFLEET=1", "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true")
		out, err := cmd.CombinedOutput()
		cancel()
		fmt.Fprintf(&b, "$ %s\n%s\n", command, tail(string(out), 20000))
		if err != nil {
			fmt.Fprintf(&b, "FAILED: %v\n", err)
			return b.String(), false
		}
	}
	return b.String(), true
}

func readGuidelines(dir string) string {
	files := []string{"CLAUDE.md", "AGENTS.md", ".github/copilot-instructions.md"}
	var b strings.Builder
	for _, name := range files {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "### %s\n\n%s", name, raw)
		if b.Len() > 40000 {
			break
		}
	}
	return b.String()
}

func branchRef(name string) string {
	if strings.HasPrefix(name, "refs/") {
		return name
	}
	return "refs/heads/" + name
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.TrimSpace(s)
}

func sensitiveEnvKey(key string) bool {
	switch key {
	case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AZURE_CLIENT_SECRET", "AZURE_TENANT_ID", "GOOGLE_APPLICATION_CREDENTIALS",
		"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "NPM_TOKEN",
		"CARGO_REGISTRY_TOKEN", "PYPI_TOKEN", "DATABASE_URL",
		"PGPASSWORD", "MYSQL_PWD", "DOCKER_AUTH_CONFIG",
		"CI_JOB_TOKEN", "GITLAB_TOKEN", "SENTRY_AUTH_TOKEN":
		return true
	}
	for _, prefix := range []string{
		"AWS_", "AZURE_", "GCP_", "GOOGLE_", "SLACK_", "STRIPE_", "SENTRY_",
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func safeEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch {
		case key == "GITHUB_TOKEN", key == "GH_TOKEN", key == "GHFLEET_GH_TOKEN":
			continue
		case strings.HasPrefix(key, "GH_"):
			continue
		case key == "SSH_AUTH_SOCK", key == "GIT_ASKPASS", key == "GIT_SSH_COMMAND":
			continue
		case sensitiveEnvKey(key):
			continue
		}
		out = append(out, kv)
	}
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n[truncated]"
}
