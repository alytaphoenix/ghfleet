package server

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
)

type actionRepoRequest struct {
	Repo       string            `json:"repo"`
	RunID      int64             `json:"run_id"`
	FailedOnly bool              `json:"failed_only"`
	Workflow   string            `json:"workflow"`
	Ref        string            `json:"ref"`
	Inputs     map[string]string `json:"inputs,omitempty"`
	Number     int               `json:"number"`
}

func (s *Server) handleActions(w http.ResponseWriter, r *http.Request) {
	if s.GH == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub client is not configured")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	name := r.URL.Query().Get("repo")
	var runs []github.WorkflowRun
	if name != "" {
		repo, ok := s.Cfg.RepoByName(name)
		if !ok {
			writeErr(w, http.StatusNotFound, "unknown repo "+name)
			return
		}
		got, err := s.GH.WorkflowRuns(r.Context(), repo.Upstream, limit)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		runs = got
	} else {
		for _, repo := range s.Cfg.Repos {
			got, err := s.GH.WorkflowRuns(r.Context(), repo.Upstream, 10)
			if err != nil {
				s.Log.Warn("actions list failed", "repo", repo.Upstream, "err", err)
				continue
			}
			runs = append(runs, got...)
		}
		sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
		if len(runs) > limit {
			runs = runs[:limit]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleActionJobs(w http.ResponseWriter, r *http.Request) {
	if s.GH == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub client is not configured")
		return
	}
	name := r.URL.Query().Get("repo")
	repo, ok := s.Cfg.RepoByName(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+name)
		return
	}
	runID, err := strconv.ParseInt(r.URL.Query().Get("run_id"), 10, 64)
	if err != nil || runID <= 0 {
		writeErr(w, http.StatusBadRequest, "run_id is required")
		return
	}
	got, err := s.GH.WorkflowRunJobs(r.Context(), repo.Upstream, runID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": got})
}

func (s *Server) handleActionRerun(w http.ResponseWriter, r *http.Request) {
	var body actionRepoRequest
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, ok := s.Cfg.RepoByName(body.Repo)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+body.Repo)
		return
	}
	if repo.ReadOnly {
		writeErr(w, http.StatusForbidden, "repo is read-only")
		return
	}
	if body.RunID <= 0 {
		writeErr(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if s.GH == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub client is not configured")
		return
	}
	if err := s.GH.RerunWorkflowRun(r.Context(), repo.Upstream, body.RunID, body.FailedOnly); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("actions rerun requested", "repo", repo.Upstream, "run_id", body.RunID, "failed_only", body.FailedOnly)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleActionCancel(w http.ResponseWriter, r *http.Request) {
	var body actionRepoRequest
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, ok := s.Cfg.RepoByName(body.Repo)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+body.Repo)
		return
	}
	if repo.ReadOnly {
		writeErr(w, http.StatusForbidden, "repo is read-only")
		return
	}
	if body.RunID <= 0 || s.GH == nil {
		writeErr(w, http.StatusBadRequest, "run_id is required")
		return
	}
	if err := s.GH.CancelWorkflowRun(r.Context(), repo.Upstream, body.RunID); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("actions cancel requested", "repo", repo.Upstream, "run_id", body.RunID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleActionDispatch(w http.ResponseWriter, r *http.Request) {
	var body actionRepoRequest
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	repo, ok := s.Cfg.RepoByName(body.Repo)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+body.Repo)
		return
	}
	if repo.ReadOnly {
		writeErr(w, http.StatusForbidden, "repo is read-only")
		return
	}
	if body.Workflow == "" || body.Ref == "" || s.GH == nil {
		writeErr(w, http.StatusBadRequest, "workflow and ref are required")
		return
	}
	if err := s.GH.DispatchWorkflow(r.Context(), repo.Upstream, body.Workflow, body.Ref, body.Inputs); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.Log.Info("workflow dispatched", "repo", repo.Upstream, "workflow", body.Workflow, "ref", body.Ref)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReviewPR(w http.ResponseWriter, r *http.Request) {
	var body actionRepoRequest
	if err := decode(w, r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := r.PathValue("name")
	repo, ok := s.Cfg.RepoByName(name)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo "+name)
		return
	}
	if repo.ReadOnly {
		writeErr(w, http.StatusForbidden, "repo is read-only")
		return
	}
	if body.Number <= 0 {
		writeErr(w, http.StatusBadRequest, "number is required")
		return
	}
	stored, err := s.Store.GetPR(repo.Name, body.Number)
	if err != nil {
		writeErr(w, http.StatusNotFound, "PR is not tracked yet; run `ghfleet scan` first")
		return
	}
	pr := stored.PR
	active, err := s.Store.HasActiveJob(repo.Name, pr.Number, jobs.KindIncomingReview)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if active {
		writeErr(w, http.StatusConflict, "a review is already queued or running for this PR")
		return
	}
	key := jobs.IdempotencyKey(jobs.KindIncomingReview, repo.Name, pr.Number, pr.HeadSHA, "manual-"+time.Now().UTC().Format("20060102T150405.000000000"))
	spec := jobs.Spec{Instructions: "Review this pull request. Do not edit files. Return findings for the author."}
	job := jobs.NewJob(jobs.KindIncomingReview, repo, pr, key, spec, repo.RunnerLabels)
	inserted, err := s.Store.Enqueue(job)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "inserted": inserted, "job": job})
}

func (s *Server) handleActionWorkflows(w http.ResponseWriter, r *http.Request) {
	if s.GH == nil {
		writeErr(w, http.StatusServiceUnavailable, "GitHub client is not configured")
		return
	}
	repo, ok := s.Cfg.RepoByName(r.URL.Query().Get("repo"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown repo")
		return
	}
	got, err := s.GH.Workflows(r.Context(), repo.Upstream)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workflows": got})
}
