// Package server is the ghfleet control plane: GitHub polling, job scheduling,
// approvals, and the HTTP API used by clients and runners.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/reconcile"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

// Version is the ghfleet version.
const Version = "0.2.0"

// CommentAPI is the GitHub write surface used after approval. It is an
// interface so tests can observe replies and thread resolution.
type CommentAPI interface {
	PostComment(ctx context.Context, slug string, number int, body string) error
	ReplyToThread(ctx context.Context, slug string, number int, rootCommentID, body string) error
	ResolveThread(ctx context.Context, threadID string) error
}

// Server is the control plane.
type Server struct {
	Cfg      *config.Config
	Store    *store.Store
	GH       *github.Client
	Comments CommentAPI
	Login    string
	Log      *slog.Logger
	Start    time.Time
	ArtDir   string

	pollMu    sync.Mutex
	summaryMu sync.Mutex
	summary   reconcile.Summary
}

// New creates a server.
func New(cfg *config.Config, st *store.Store, gh *github.Client, login string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	if login == "" && gh != nil {
		login = gh.Login
	}
	var comments CommentAPI
	if gh != nil {
		comments = gh
	}
	return &Server{
		Cfg:      cfg,
		Store:    st,
		GH:       gh,
		Comments: comments,
		Login:    login,
		Log:      log,
		Start:    time.Now().UTC(),
		ArtDir:   filepath.Join(cfg.Server.StateDir, "artifacts"),
	}
}

// Run starts polling and serves the API until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := os.MkdirAll(s.ArtDir, 0o755); err != nil {
		return err
	}
	if s.Cfg.Server.APIToken == "" && !isLoopback(s.Cfg.Server.Listen) {
		return fmt.Errorf("server.api_token is required when listening on %s", s.Cfg.Server.Listen)
	}
	go s.pollLoop(ctx)

	srv := &http.Server{
		Addr:              s.Cfg.Server.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.Log.Info("ghfleet server listening", "addr", s.Cfg.Server.Listen, "viewer", s.Login)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (s *Server) pollLoop(ctx context.Context) {
	s.reconcileOnce(ctx)
	interval := s.Cfg.Server.PollInterval.D()
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileOnce(ctx)
		}
	}
}

func (s *Server) reconcileOnce(ctx context.Context) {
	s.pollMu.Lock()
	defer s.pollMu.Unlock()

	r := &reconcile.Reconciler{Cfg: s.Cfg, Store: s.Store, GH: s.GH, Viewer: s.Login, Log: s.Log}
	summary := r.Run(ctx)

	s.summaryMu.Lock()
	s.summary = summary
	s.summaryMu.Unlock()

	if n, err := s.Store.RequeueExpired(); err == nil && n > 0 {
		s.Log.Info("requeued expired leases", "count", n)
	}
	if n, err := s.Store.MarkStaleRunners(time.Now().UTC().Add(-90 * time.Second)); err == nil && n > 0 {
		s.Log.Info("marked runners offline", "count", n)
	}
	if len(summary.Errors) > 0 {
		s.Log.Warn("reconcile completed with errors", "prs", summary.PRs, "new_jobs", summary.NewJobs, "errors", len(summary.Errors))
	} else {
		s.Log.Info("reconcile complete", "repos", summary.Repos, "prs", summary.PRs, "new_jobs", summary.NewJobs)
	}
}

// ReconcileOnce runs one synchronous polling pass and returns its summary.
func (s *Server) ReconcileOnce(ctx context.Context) reconcile.Summary {
	s.reconcileOnce(ctx)
	s.summaryMu.Lock()
	defer s.summaryMu.Unlock()
	return s.summary
}

// Handler builds the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version})
	})

	mux.Handle("GET /api/v1/status", s.auth(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /api/v1/prs", s.auth(http.HandlerFunc(s.handlePRs)))
	mux.Handle("POST /api/v1/scan", s.auth(http.HandlerFunc(s.handleScan)))
	mux.Handle("POST /api/v1/repos/{name}/sync", s.auth(http.HandlerFunc(s.handleSyncRepo)))
	mux.Handle("POST /api/v1/repos/{name}/security", s.auth(http.HandlerFunc(s.handleSecurityRepo)))
	mux.Handle("POST /api/v1/repos/{name}/review", s.auth(http.HandlerFunc(s.handleReviewPR)))
	mux.Handle("GET /api/v1/actions", s.auth(http.HandlerFunc(s.handleActions)))
	mux.Handle("GET /api/v1/actions/jobs", s.auth(http.HandlerFunc(s.handleActionJobs)))
	mux.Handle("GET /api/v1/actions/workflows", s.auth(http.HandlerFunc(s.handleActionWorkflows)))
	mux.Handle("POST /api/v1/actions/rerun", s.auth(http.HandlerFunc(s.handleActionRerun)))
	mux.Handle("POST /api/v1/actions/cancel", s.auth(http.HandlerFunc(s.handleActionCancel)))
	mux.Handle("POST /api/v1/actions/dispatch", s.auth(http.HandlerFunc(s.handleActionDispatch)))
	mux.Handle("GET /api/v1/jobs", s.auth(http.HandlerFunc(s.handleJobs)))
	mux.Handle("GET /api/v1/jobs/{id}", s.auth(http.HandlerFunc(s.handleJob)))
	mux.Handle("GET /api/v1/jobs/{id}/patch", s.auth(http.HandlerFunc(s.handlePatch)))
	mux.Handle("POST /api/v1/jobs/{id}/approve", s.auth(http.HandlerFunc(s.handleApprove)))
	mux.Handle("POST /api/v1/jobs/{id}/deny", s.auth(http.HandlerFunc(s.handleDeny)))
	mux.Handle("POST /api/v1/jobs/{id}/start", s.auth(http.HandlerFunc(s.handleStart)))
	mux.Handle("POST /api/v1/jobs/{id}/result", s.auth(http.HandlerFunc(s.handleResult)))
	mux.Handle("POST /api/v1/jobs/{id}/artifact", s.auth(http.HandlerFunc(s.handleArtifact)))
	mux.Handle("GET /api/v1/jobs/{id}/instruction", s.auth(http.HandlerFunc(s.handleInstruction)))
	mux.Handle("POST /api/v1/jobs/{id}/applied", s.auth(http.HandlerFunc(s.handleApplied)))
	mux.Handle("POST /api/v1/jobs/{id}/failed", s.auth(http.HandlerFunc(s.handleFailed)))
	mux.Handle("GET /api/v1/runners", s.auth(http.HandlerFunc(s.handleRunners)))
	mux.Handle("POST /api/v1/runners/register", s.auth(http.HandlerFunc(s.handleRegister)))
	mux.Handle("POST /api/v1/runners/{id}/heartbeat", s.auth(http.HandlerFunc(s.handleHeartbeat)))
	mux.Handle("POST /api/v1/runners/{id}/claim", s.auth(http.HandlerFunc(s.handleClaim)))
	return mux
}

func (s *Server) auth(next http.Handler) http.Handler {
	token := s.Cfg.Server.APIToken
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" {
			got = r.Header.Get("X-Ghfleet-Token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid API token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// StatusResponse is the dashboard payload.
type StatusResponse struct {
	Stats       store.Stats       `json:"stats"`
	Runners     []store.Runner    `json:"runners"`
	LastSummary reconcile.Summary `json:"last_summary"`
	Viewer      string            `json:"viewer"`
	Version     string            `json:"version"`
	Started     time.Time         `json:"started"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	runners, _ := s.Store.ListRunners()
	s.summaryMu.Lock()
	summary := s.summary
	s.summaryMu.Unlock()
	writeJSON(w, http.StatusOK, StatusResponse{
		Stats: st, Runners: runners, LastSummary: summary, Viewer: s.Login, Version: Version, Started: s.Start,
	})
}

func (s *Server) handlePRs(w http.ResponseWriter, r *http.Request) {
	prs, err := s.Store.ListPRs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	repoFilter := r.URL.Query().Get("repo")
	stateFilter := strings.ToLower(r.URL.Query().Get("state"))
	out := prs[:0]
	for _, pr := range prs {
		if repoFilter != "" && pr.Repo != repoFilter && pr.Slug != repoFilter {
			continue
		}
		if stateFilter != "" {
			state := PRState(pr)
			if stateFilter != "all" && state != stateFilter {
				continue
			}
		}
		out = append(out, pr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"prs": out, "count": len(out)})
}

// PRState is the short "what needs attention" label used by clients.
func PRState(pr store.PR) string {
	switch {
	case strings.EqualFold(pr.Mergeable, "CONFLICTING"):
		return "conflict"
	case strings.EqualFold(pr.CheckState, "FAILURE"), strings.EqualFold(pr.CheckState, "ERROR"):
		return "ci"
	case strings.EqualFold(pr.ReviewDecision, "CHANGES_REQUESTED"):
		return "changes"
	case pr.IsDraft:
		return "draft"
	case strings.EqualFold(pr.ReviewDecision, "APPROVED"):
		return "approved"
	default:
		return "open"
	}
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	s.reconcileOnce(r.Context())
	s.summaryMu.Lock()
	summary := s.summary
	s.summaryMu.Unlock()
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleSyncRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	repo, ok := s.Cfg.RepoByName(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+name)
		return
	}
	if repo.ReadOnly || repo.Fork == "" || repo.Fork == repo.Upstream {
		writeErr(w, http.StatusBadRequest, "repo is not a writable fork")
		return
	}
	if owner(repo.Fork) != s.Login {
		writeErr(w, http.StatusBadRequest, "fork is not owned by the authenticated user")
		return
	}
	key := jobs.IdempotencyKey(jobs.KindForkSync, repo.Name, 0, "", "manual-"+time.Now().UTC().Format("20060102T150405"))
	pr := github.PR{Repo: repo.Upstream, Number: 0, BaseRef: repo.DefaultBranch}
	job := jobs.NewJob(jobs.KindForkSync, repo, pr, key, jobs.Spec{
		Instructions: "Fast-forward the fork's default branch from upstream. Never force-push. If the fork has diverged, escalate.",
	}, repo.RunnerLabels)
	inserted, err := s.Store.Enqueue(job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": inserted, "job": job})
}

func (s *Server) handleSecurityRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	repo, ok := s.Cfg.RepoByName(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+name)
		return
	}
	if repo.ReadOnly {
		writeErr(w, http.StatusBadRequest, "repo is read-only")
		return
	}
	key := jobs.IdempotencyKey(jobs.KindSecurity, repo.Name, 0, "", "manual-"+time.Now().UTC().Format("20060102T150405"))
	pr := github.PR{Repo: repo.Upstream, Number: 0, BaseRef: repo.DefaultBranch, HeadRef: repo.DefaultBranch}
	job := jobs.NewJob(jobs.KindSecurity, repo, pr, key, jobs.Spec{
		Instructions: "Scan the default branch for known dependency vulnerabilities. If findings exist, update only the affected dependencies to the smallest safe versions, regenerate lockfiles, and run the repository checks. Escalate major-version or code-migration fixes.",
	}, repo.RunnerLabels)
	inserted, err := s.Store.Enqueue(job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": inserted, "job": job})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	statusParam := r.URL.Query().Get("status")
	var statuses []string
	if statusParam == "" || statusParam == "inbox" {
		statuses = []string{"pending", "claimed", "running", "awaiting_approval", "approved"}
	} else {
		for _, st := range strings.Split(statusParam, ",") {
			if st = strings.TrimSpace(st); st != "" {
				statuses = append(statuses, st)
			}
		}
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	list, err := s.Store.ListJobs(statuses, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": list, "count": len(list)})
}

// JobDetail is the client-facing job payload.
type JobDetail struct {
	Job     store.Job   `json:"job"`
	Spec    jobs.Spec   `json:"spec"`
	Result  jobs.Result `json:"result"`
	Patch   string      `json:"patch,omitempty"`
	PRURL   string      `json:"pr_url,omitempty"`
	PRTitle string      `json:"pr_title,omitempty"`
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	detail := jobDetail(job)
	if r.URL.Query().Get("include") == "patch" {
		if patch, err := s.readPatch(job.ID); err == nil {
			detail.Patch = patch
		}
	}
	writeJSON(w, http.StatusOK, detail)
}

func jobDetail(job store.Job) JobDetail {
	var spec jobs.Spec
	_ = json.Unmarshal([]byte(job.SpecJSON), &spec)
	result := jobs.ParseResult(job.ResultJSON)
	return JobDetail{
		Job: job, Spec: spec, Result: result,
		PRURL: spec.PR.URL, PRTitle: spec.PR.Title,
	}
}

func (s *Server) readPatch(jobID string) (string, error) {
	a, err := s.Store.GetArtifact(jobID, "patch")
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) handlePatch(w http.ResponseWriter, r *http.Request) {
	patch, err := s.readPatch(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no patch for this job")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, patch)
}

func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.jobStale(job); err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err := s.Store.Approve(job.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	job, _ = s.Store.GetJob(job.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

func (s *Server) jobStale(job store.Job) error {
	if job.Kind == jobs.KindForkSync || job.PRNumber <= 0 {
		return nil
	}
	pr, err := s.Store.GetPR(job.Repo, job.PRNumber)
	if err != nil {
		return fmt.Errorf("job is stale: the PR is no longer tracked")
	}
	if pr.HeadSHA != "" && pr.HeadSHA != job.HeadSHA {
		return fmt.Errorf("job is stale: the PR head has changed; wait for the next scan")
	}
	if (job.Kind == jobs.KindRebase || job.Kind == jobs.KindUpstreamMerge) && pr.BaseSHA != "" && pr.BaseSHA != job.BaseSHA {
		return fmt.Errorf("job is stale: the base branch has moved; wait for the next scan")
	}
	return nil
}

func (s *Server) handleDeny(w http.ResponseWriter, r *http.Request) {
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err := s.Store.Deny(job.ID); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.markThreadsHandled(job)
	if job.Kind != jobs.KindForkSync && job.Kind != jobs.KindSecurity {
		_ = s.Store.ReleaseKey(job.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RunnerInfo is sent by a runner on registration.
type RunnerInfo struct {
	ID       string   `json:"id"`
	Hostname string   `json:"hostname"`
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	Labels   []string `json:"labels"`
	Repos    []string `json:"repos"`
	Version  string   `json:"version"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var info RunnerInfo
	if err := decode(w, r, &info); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if info.ID == "" {
		writeErr(w, http.StatusBadRequest, "runner id is required")
		return
	}
	err := s.Store.UpsertRunner(store.Runner{
		ID: info.ID, Hostname: info.Hostname, OS: info.OS, Arch: info.Arch,
		Labels: info.Labels, Repos: info.Repos, Version: info.Version,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Heartbeat(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ClaimRequest is a runner asking for work.
type ClaimRequest struct {
	ID     string   `json:"id"`
	Labels []string `json:"labels"`
	Repos  []string `json:"repos"`
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	var req ClaimRequest
	if err := decode(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ID == "" {
		req.ID = r.PathValue("id")
	}
	// A runner that restarted while a job was approved should resume it first.
	if approved, err := s.Store.JobsForRunner(req.ID, []string{"approved"}, 20); err == nil {
		lease := time.Now().UTC().Add(s.Cfg.Limits.ApprovalTimeout.D() + time.Hour)
		for _, job := range approved {
			if !contains(req.Repos, job.Repo) || !containsAll(req.Labels, job.RequiredLabels) {
				continue
			}
			reserved, err := s.Store.ReserveApproved(job.ID, req.ID, lease)
			if err != nil || !reserved {
				continue
			}
			if staleErr := s.jobStale(job); staleErr != nil {
				_ = s.Store.Deny(job.ID)
				s.markThreadsHandled(job)
				s.Log.Warn("denied stale approved job", "job", job.ID, "err", staleErr)
				continue
			}
			writeJSON(w, http.StatusOK, jobDetail(job))
			return
		}
	}

	if s.Cfg.Limits.MaxJobsPerHour > 0 {
		if n, err := s.Store.CountStartedSince(time.Now().Add(-time.Hour)); err == nil && n >= s.Cfg.Limits.MaxJobsPerHour {
			w.Header().Set("X-Ghfleet-Rate-Limited", "jobs")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if s.Cfg.Limits.MaxBudgetUSDPerDay > 0 {
		if cost, err := s.Store.CostSince(time.Now().Add(-24 * time.Hour)); err == nil && cost >= s.Cfg.Limits.MaxBudgetUSDPerDay {
			w.Header().Set("X-Ghfleet-Rate-Limited", "budget")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	pending, err := s.Store.PendingJobs(100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lease := time.Now().UTC().Add(s.Cfg.Limits.JobTimeout.D() + 60*time.Minute)
	for _, job := range pending {
		if !contains(req.Repos, job.Repo) {
			continue
		}
		if !containsAll(req.Labels, job.RequiredLabels) {
			continue
		}
		ok, err := s.Store.ClaimJob(job.ID, req.ID, lease)
		if err != nil || !ok {
			continue
		}
		claimed, _ := s.Store.GetJob(job.ID)
		writeJSON(w, http.StatusOK, jobDetail(claimed))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunnerID string `json:"runner_id"`
	}
	_ = decode(w, r, &body)
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if job.RunnerID != "" && body.RunnerID != "" && job.RunnerID != body.RunnerID {
		writeErr(w, http.StatusForbidden, "job belongs to another runner")
		return
	}
	if job.Status == "denied" || job.Status == "cancelled" || job.Status == "applied" || job.Status == "failed" {
		writeErr(w, http.StatusConflict, "job is no longer claimable")
		return
	}
	if err := s.Store.SetJobStatus(job.ID, "running", ""); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// RunnerResult is uploaded after the agent and verification finish.
type RunnerResult struct {
	RunnerID string      `json:"runner_id"`
	Result   jobs.Result `json:"result"`
	Risk     string      `json:"risk"`
	Summary  string      `json:"summary"`
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	var body RunnerResult
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if body.RunnerID != "" && job.RunnerID != "" && body.RunnerID != job.RunnerID {
		writeErr(w, http.StatusForbidden, "job belongs to another runner")
		return
	}
	if job.Status == "denied" || job.Status == "cancelled" || job.Status == "applied" || job.Status == "failed" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	risk := body.Risk
	if risk == "" {
		risk = body.Result.Risk
	}
	summary := body.Summary
	if summary == "" {
		summary = body.Result.Summary
	}
	raw := body.Result
	raw.Risk, raw.Summary = risk, summary
	leaseUntil := time.Now().UTC().Add(s.Cfg.Limits.ApprovalTimeout.D() + time.Hour)
	if err := s.Store.SaveResult(job.ID, raw.Marshal(), risk, summary, raw.CostUSD, leaseUntil); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if raw.Decision == "noop" && raw.PatchSize == 0 && raw.Silent {
		// A clean security scan has no write to approve.
		_ = s.Store.Approve(job.ID)
	} else if !raw.NeedsHuman && !strings.EqualFold(raw.Decision, "escalate") && !strings.EqualFold(raw.Risk, "high") {
		if err := s.jobStale(job); err != nil {
			_ = s.Store.Deny(job.ID)
			s.markThreadsHandled(job)
			s.Log.Warn("denied stale job", "job", job.ID, "err", err)
		} else if job.Kind == jobs.KindForkSync && s.Cfg.Approvals.ForkSync == "auto" {
			_ = s.Store.Approve(job.ID)
		} else if job.Kind != jobs.KindForkSync && s.Cfg.Approvals.Push == "auto" {
			_ = s.Store.Approve(job.ID)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("id")
	if jobID == "" || filepath.Base(jobID) != jobID || jobID == "." || jobID == ".." {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	if _, err := s.Store.GetJob(jobID); err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "patch"
	}
	if strings.ContainsAny(kind, "/\\") {
		writeErr(w, http.StatusBadRequest, "invalid artifact kind")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	dir := filepath.Join(s.ArtDir, jobID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := filepath.Join(dir, kind)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if err := s.Store.SetArtifact(jobID, kind, path, int64(len(body)), sha); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "size": len(body), "sha256": sha})
}

func (s *Server) handleInstruction(w http.ResponseWriter, r *http.Request) {
	runnerID := r.URL.Query().Get("runner_id")
	if runnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id is required")
		return
	}
	id := r.PathValue("id")
	if job, err := s.Store.GetJob(id); err == nil && job.Status == "approved" {
		if staleErr := s.jobStale(job); staleErr != nil {
			_ = s.Store.Deny(id)
			s.markThreadsHandled(job)
			s.Log.Warn("denied stale approved job", "job", id, "err", staleErr)
			writeJSON(w, http.StatusOK, store.Instruction{Action: "deny", Job: job})
			return
		}
	}
	instr, err := s.Store.InstructionFor(id, runnerID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, instr)
}

func (s *Server) handleApplied(w http.ResponseWriter, r *http.Request) {
	var body RunnerResult
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if body.RunnerID != "" && job.RunnerID != "" && body.RunnerID != job.RunnerID {
		writeErr(w, http.StatusForbidden, "job belongs to another runner")
		return
	}
	if job.Status == "applied" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "already_applied": true})
		return
	}
	if job.Status == "denied" || job.Status == "cancelled" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
		return
	}
	if err := s.Store.MarkApplied(job.ID, body.Result.Marshal()); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.markThreadsHandled(job)
	s.postAppliedComment(r.Context(), job, body.Result)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleFailed(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RunnerID string `json:"runner_id"`
		Error    string `json:"error"`
	}
	_ = decode(w, r, &body)
	job, err := s.Store.GetJob(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if body.RunnerID != "" && job.RunnerID != "" && body.RunnerID != job.RunnerID {
		writeErr(w, http.StatusForbidden, "job belongs to another runner")
		return
	}
	if err := s.Store.SetJobStatus(job.ID, "failed", body.Error); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if job.Kind != jobs.KindForkSync && job.Kind != jobs.KindSecurity {
		_ = s.Store.ReleaseKey(job.ID)
	}
	s.Log.Warn("job failed", "job", job.ID, "err", body.Error)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) markThreadsHandled(job store.Job) {
	var spec jobs.Spec
	if err := json.Unmarshal([]byte(job.SpecJSON), &spec); err != nil {
		return
	}
	for _, t := range spec.Threads {
		commentID := t.LastCommentID
		if commentID == "" && len(t.Comments) > 0 {
			commentID = t.Comments[len(t.Comments)-1].ID
		}
		if commentID != "" {
			_ = s.Store.MarkThreadHandledAt(t.ID, commentID)
		}
	}
	reviewIDs := make([]string, 0, len(spec.Reviews))
	for _, rv := range spec.Reviews {
		reviewIDs = append(reviewIDs, rv.ID)
	}
	if len(reviewIDs) > 0 {
		_ = s.Store.MarkReviewsHandled(reviewIDs)
	}
}

func (s *Server) postAppliedComment(ctx context.Context, job store.Job, result jobs.Result) {
	if s.Cfg.Approvals.Comment == "never" || job.PRNumber == 0 || s.Comments == nil || result.Silent {
		return
	}
	var spec jobs.Spec
	if err := json.Unmarshal([]byte(job.SpecJSON), &spec); err != nil {
		return
	}
	replies := map[string]jobs.ThreadReply{}
	for _, tr := range result.ThreadReplies {
		if strings.TrimSpace(tr.Body) != "" {
			replies[tr.ThreadID] = tr
		}
	}
	posted := 0
	for _, t := range spec.Threads {
		tr, ok := replies[t.ID]
		if !ok {
			continue
		}
		root := t.RootCommentID
		if root == "" && len(t.Comments) > 0 && t.Comments[0].DatabaseID > 0 {
			root = strconv.FormatInt(t.Comments[0].DatabaseID, 10)
		}
		if root == "" {
			continue
		}
		if err := s.Comments.ReplyToThread(ctx, spec.Repo.Upstream, job.PRNumber, root, clamp(tr.Body, 10000)); err != nil {
			s.Log.Warn("thread reply failed", "job", job.ID, "thread", t.ID, "err", err)
			continue
		}
		posted++
		_ = s.Store.Action(job.ID, "thread-reply", t.ID, "")
		if tr.Resolve && s.canResolveThread(t) {
			if err := s.Comments.ResolveThread(ctx, t.ID); err != nil {
				s.Log.Warn("resolve thread failed", "job", job.ID, "thread", t.ID, "err", err)
			} else {
				_ = s.Store.Action(job.ID, "thread-resolve", t.ID, "")
			}
		}
	}

	// Post one summary comment when some threads were not replied to, when the
	// job addressed review-body comments, or when it had no threads at all.
	if posted < len(spec.Threads) || len(spec.Reviews) > 0 || (len(spec.Threads) == 0 && len(spec.Reviews) == 0) {
		body := strings.TrimSpace(result.Reply)
		if body == "" {
			body = "Applied by ghfleet: " + result.Summary
		}
		if job.Kind == jobs.KindIncomingReview {
			body = formatFindings(result)
		}
		if result.CommitSHA != "" {
			body += "\n\nCommit: `" + result.CommitSHA + "`"
		}
		body += "\n\n<!-- ghfleet:" + job.ID + " -->"
		if err := s.Comments.PostComment(ctx, spec.Repo.Upstream, job.PRNumber, clamp(body, 60000)); err != nil {
			s.Log.Warn("post comment failed", "job", job.ID, "err", err)
			return
		}
		_ = s.Store.Action(job.ID, "comment", spec.Repo.Upstream, "")
	}
}

func formatFindings(result jobs.Result) string {
	var b strings.Builder
	b.WriteString("ghfleet review")
	if result.Summary != "" {
		b.WriteString(": " + result.Summary)
	}
	if len(result.Findings) == 0 {
		b.WriteString("\n\nNo blocking findings.")
		return b.String()
	}
	b.WriteString("\n\n")
	for _, f := range result.Findings {
		loc := f.File
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "- **%s** %s: %s\n", strings.ToUpper(emptyDefault(f.Severity, "info")), loc, f.Body)
	}
	return strings.TrimSpace(b.String())
}

func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "\n\n[truncated]"
}

func emptyDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *Server) canResolveThread(t jobs.ThreadSpec) bool {
	switch s.Cfg.Approvals.ResolveThreads {
	case "all":
		return true
	case "bot":
		if len(t.Comments) == 0 {
			return false
		}
		author := t.Comments[len(t.Comments)-1].Author
		return strings.HasSuffix(author, "[bot]") || author == "github-actions"
	default:
		return false
	}
}

func (s *Server) handleRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.Store.ListRunners()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": runners})
}

func owner(slug string) string {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsAll(have, need []string) bool {
	for _, n := range need {
		if !contains(have, n) {
			return false
		}
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, out any) error {
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}
