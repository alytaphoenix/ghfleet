// Package reconcile observes GitHub and enqueues the jobs that policy says are due.
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

// GitHub is the subset of the client the reconciler needs.
type GitHub interface {
	AuthoredPRs(ctx context.Context, slugs []string, viewer string) ([]github.PR, error)
	IncomingPRs(ctx context.Context, slugs []string, viewer string) ([]github.PR, error)
}

// Reconciler polls GitHub and enqueues work.
type Reconciler struct {
	Cfg    *config.Config
	Store  *store.Store
	GH     GitHub
	Viewer string
	Log    *slog.Logger
	Now    func() time.Time
}

// Summary reports one reconcile pass.
type Summary struct {
	Repos   int       `json:"repos"`
	PRs     int       `json:"prs"`
	NewJobs int       `json:"new_jobs"`
	Errors  []string  `json:"errors,omitempty"`
	At      time.Time `json:"at"`
}

// Run performs one pass over all configured repos.
func (r *Reconciler) Run(ctx context.Context) Summary {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	out := Summary{At: now}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	byRepo := map[string][]github.PR{}
	slugs := make([]string, 0, len(r.Cfg.Repos))
	for _, repo := range r.Cfg.Repos {
		if !repo.ReadOnly {
			slugs = append(slugs, repo.Upstream)
		}
	}
	all, err := r.GH.AuthoredPRs(ctx, slugs, r.Viewer)
	if err != nil {
		out.Errors = append(out.Errors, fmt.Sprintf("search: %v", err))
		r.Log.Warn("authored PR search failed", "err", err)
	}
	for _, pr := range all {
		byRepo[pr.Slug] = append(byRepo[pr.Slug], pr)
	}
	incomingByRepo := map[string][]github.PR{}
	incomingSlugs := make([]string, 0)
	for _, repo := range r.Cfg.Repos {
		if repo.ReviewIncoming && !repo.ReadOnly {
			incomingSlugs = append(incomingSlugs, repo.Upstream)
		}
	}
	if len(incomingSlugs) > 0 {
		incoming, inErr := r.GH.IncomingPRs(ctx, incomingSlugs, r.Viewer)
		if inErr != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("incoming: %v", inErr))
			r.Log.Warn("incoming PR query failed", "err", inErr)
		}
		for _, pr := range incoming {
			incomingByRepo[pr.Slug] = append(incomingByRepo[pr.Slug], pr)
		}
	}
	seen := map[string]bool{}
	for _, repo := range r.Cfg.Repos {
		out.Repos++
		if r.Viewer != "" && repo.Fork != "" && !repo.Own && owner(repo.Fork) != r.Viewer {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: fork %s is not owned by %s", repo.Name, repo.Fork, r.Viewer))
			continue
		}
		prs := byRepo[repo.Upstream]
		for _, pr := range prs {
			out.PRs++
			pr.Repo = repo.Name
			seen[fmt.Sprintf("%s#%d", repo.Name, pr.Number)] = true
			if err := r.Store.UpsertPR(pr); err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s#%d: %v", repo.Name, pr.Number, err))
				continue
			}
			n, err := r.reconcilePR(ctx, repo, pr, now)
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s#%d: %v", repo.Name, pr.Number, err))
				continue
			}
			out.NewJobs += n
		}
		if repo.ReviewIncoming && !repo.ReadOnly {
			for _, pr := range incomingByRepo[repo.Upstream] {
				out.PRs++
				pr.Repo = repo.Name
				seen[fmt.Sprintf("%s#%d", repo.Name, pr.Number)] = true
				if err := r.Store.UpsertPR(pr); err != nil {
					out.Errors = append(out.Errors, fmt.Sprintf("%s#%d: %v", repo.Name, pr.Number, err))
					continue
				}
				n, err := r.reconcileIncomingPR(repo, pr)
				if err != nil {
					out.Errors = append(out.Errors, fmt.Sprintf("%s#%d: %v", repo.Name, pr.Number, err))
					continue
				}
				out.NewJobs += n
			}
		}
		if repo.AutoForkSync && !repo.ReadOnly && repo.Fork != "" && repo.Fork != repo.Upstream {
			added, err := r.reconcileForkSync(repo, now)
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s fork-sync: %v", repo.Name, err))
			} else {
				out.NewJobs += added
			}
		}
		if repo.AutoSecurity && !repo.ReadOnly {
			added, err := r.reconcileSecurity(repo, now)
			if err != nil {
				out.Errors = append(out.Errors, fmt.Sprintf("%s security: %v", repo.Name, err))
			} else {
				out.NewJobs += added
			}
		}
	}
	if err == nil {
		_ = r.Store.PrunePRs(seen)
	}
	_ = r.Store.SetMeta("last_scan", now.Format(time.RFC3339))
	return out
}

func (r *Reconciler) reconcilePR(ctx context.Context, repo config.Repo, pr github.PR, now time.Time) (int, error) {
	added := 0
	if repo.ReadOnly {
		return added, nil
	}
	threads, err := r.Store.UnresolvedThreads(repo.Name, pr.Number, r.Viewer)
	if err != nil {
		return added, err
	}
	reviews, err := r.Store.UnhandledReviews(repo.Name, pr.Number, r.Viewer)
	if err != nil {
		return added, err
	}
	if len(threads) > 0 || len(reviews) > 0 {
		active, err := r.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindReviewResponse)
		if err != nil {
			return added, err
		}
		if !active && !r.attemptsExhausted(repo, pr.Number, jobs.KindReviewResponse) {
			extra := jobs.ThreadFingerprint(threads) + ":" + jobs.ReviewFingerprint(reviews)
			key := jobs.IdempotencyKey(jobs.KindReviewResponse, repo.Name, pr.Number, pr.HeadSHA, extra)
			spec := jobs.Spec{
				Threads: jobs.Threads(threads), Reviews: jobs.Reviews(reviews),
				Instructions: "Address the listed review threads and review bodies. Reply with what changed, or explain why no change is needed.",
			}
			job := jobs.NewJob(jobs.KindReviewResponse, repo, pr, key, spec, repo.RunnerLabels)
			ok, err := r.Store.Enqueue(job)
			if err != nil {
				return added, err
			}
			if ok {
				added++
				r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "pr", pr.Number)
			}
		}
	}

	if strings.EqualFold(pr.CheckState, "FAILURE") || strings.EqualFold(pr.CheckState, "ERROR") {
		failing := github.FailingChecks(pr.Checks)
		if len(failing) > 0 {
			active, err := r.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindCITriage)
			if err != nil {
				return added, err
			}
			if !active && !r.attemptsExhausted(repo, pr.Number, jobs.KindCITriage) {
				key := jobs.IdempotencyKey(jobs.KindCITriage, repo.Name, pr.Number, pr.HeadSHA, jobs.CheckFingerprint(failing))
				spec := jobs.Spec{Checks: jobs.Checks(failing), Instructions: "Fix the failing checks. Do not weaken tests. If a failure is flaky or infrastructure-related, escalate."}
				job := jobs.NewJob(jobs.KindCITriage, repo, pr, key, spec, repo.RunnerLabels)
				ok, err := r.Store.Enqueue(job)
				if err != nil {
					return added, err
				}
				if ok {
					added++
					r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "pr", pr.Number)
				}
			}
		}
	}

	if repo.MergeUpstream && strings.EqualFold(pr.MergeState, "BEHIND") {
		active, err := r.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindUpstreamMerge)
		if err != nil {
			return added, err
		}
		if !active && !r.attemptsExhausted(repo, pr.Number, jobs.KindUpstreamMerge) {
			key := jobs.IdempotencyKey(jobs.KindUpstreamMerge, repo.Name, pr.Number, pr.HeadSHA, pr.BaseSHA)
			spec := jobs.Spec{Instructions: "Merge the current base branch into the PR branch without rewriting history. Resolve only mechanical conflicts; preserve the intent of both sides."}
			job := jobs.NewJob(jobs.KindUpstreamMerge, repo, pr, key, spec, repo.RunnerLabels)
			ok, err := r.Store.Enqueue(job)
			if err != nil {
				return added, err
			}
			if ok {
				added++
				r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "pr", pr.Number, "kind", jobs.KindUpstreamMerge)
			}
		}
	}

	if strings.EqualFold(pr.Mergeable, "CONFLICTING") {
		active, err := r.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindRebase)
		if err != nil {
			return added, err
		}
		if !active && !r.attemptsExhausted(repo, pr.Number, jobs.KindRebase) {
			key := jobs.IdempotencyKey(jobs.KindRebase, repo.Name, pr.Number, pr.HeadSHA, jobs.RebaseFingerprint(pr.BaseSHA))
			spec := jobs.Spec{Instructions: "Rebase the PR branch onto the current base. Resolve only mechanical conflicts."}
			job := jobs.NewJob(jobs.KindRebase, repo, pr, key, spec, repo.RunnerLabels)
			ok, err := r.Store.Enqueue(job)
			if err != nil {
				return added, err
			}
			if ok {
				added++
				r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "pr", pr.Number)
			}
		}
	}
	return added, nil
}

func (r *Reconciler) reconcileIncomingPR(repo config.Repo, pr github.PR) (int, error) {
	if pr.IsDraft {
		return 0, nil
	}
	active, err := r.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindIncomingReview)
	if err != nil {
		return 0, err
	}
	if active || r.attemptsExhausted(repo, pr.Number, jobs.KindIncomingReview) {
		return 0, nil
	}
	key := jobs.IdempotencyKey(jobs.KindIncomingReview, repo.Name, pr.Number, pr.HeadSHA, "")
	spec := jobs.Spec{Instructions: "Review this pull request. Do not edit files. Return findings for the author."}
	job := jobs.NewJob(jobs.KindIncomingReview, repo, pr, key, spec, repo.RunnerLabels)
	ok, err := r.Store.Enqueue(job)
	if err != nil {
		return 0, err
	}
	if ok {
		r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "pr", pr.Number)
		return 1, nil
	}
	return 0, nil
}

func owner(slug string) string {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func (r *Reconciler) attemptsExhausted(repo config.Repo, prNumber int, kind string) bool {
	if r.Cfg.Limits.MaxFixAttemptsPerPR <= 0 {
		return false
	}
	n, err := r.Store.CountClosedJobs(repo.Name, prNumber, kind)
	return err == nil && n >= r.Cfg.Limits.MaxFixAttemptsPerPR
}

func (r *Reconciler) reconcileSecurity(repo config.Repo, now time.Time) (int, error) {
	active, err := r.Store.HasActiveJob(repo.Name, 0, jobs.KindSecurity)
	if err != nil {
		return 0, err
	}
	if active {
		return 0, nil
	}
	key := jobs.IdempotencyKey(jobs.KindSecurity, repo.Name, 0, "", jobs.ForkSyncKey(now, repo.SecurityInterval.D()))
	pr := github.PR{Repo: repo.Upstream, Number: 0, BaseRef: repo.DefaultBranch, HeadRef: repo.DefaultBranch}
	spec := jobs.Spec{
		Instructions: "Scan the default branch for known dependency vulnerabilities. If findings exist, update only the affected dependencies to the smallest safe versions, regenerate lockfiles, and run the repository checks. Escalate major-version or code-migration fixes.",
	}
	job := jobs.NewJob(jobs.KindSecurity, repo, pr, key, spec, repo.RunnerLabels)
	ok, err := r.Store.Enqueue(job)
	if err != nil {
		return 0, err
	}
	if ok {
		r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "kind", jobs.KindSecurity)
		return 1, nil
	}
	return 0, nil
}

func (r *Reconciler) reconcileForkSync(repo config.Repo, now time.Time) (int, error) {
	active, err := r.Store.HasActiveJob(repo.Name, 0, jobs.KindForkSync)
	if err != nil {
		return 0, err
	}
	if active {
		return 0, nil
	}
	key := jobs.IdempotencyKey(jobs.KindForkSync, repo.Name, 0, "", jobs.ForkSyncKey(now, repo.ForkSyncInterval.D()))
	pr := github.PR{Repo: repo.Upstream, Number: 0, BaseRef: repo.DefaultBranch}
	spec := jobs.Spec{
		Instructions: "Fast-forward the fork's default branch from upstream. Never force-push. If the fork has diverged, escalate.",
	}
	job := jobs.NewJob(jobs.KindForkSync, repo, pr, key, spec, repo.RunnerLabels)
	ok, err := r.Store.Enqueue(job)
	if err != nil {
		return 0, err
	}
	if ok {
		r.Log.Info("enqueued", "job", job.ID, "repo", repo.Upstream, "kind", jobs.KindForkSync)
		return 1, nil
	}
	return 0, nil
}
