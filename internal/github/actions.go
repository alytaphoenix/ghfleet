package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
)

// WorkflowRun is one GitHub Actions workflow run.
type WorkflowRun struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	DisplayTitle string    `json:"display_title"`
	WorkflowName string    `json:"workflow_name"`
	Event        string    `json:"event"`
	Status       string    `json:"status"`
	Conclusion   string    `json:"conclusion"`
	HeadBranch   string    `json:"head_branch"`
	HeadSHA      string    `json:"head_sha"`
	HTMLURL      string    `json:"html_url"`
	RunNumber    int       `json:"run_number"`
	RunAttempt   int       `json:"run_attempt"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Repo         string    `json:"repo,omitempty"`
}

// WorkflowJob is one job within a workflow run.
type WorkflowJob struct {
	ID          int64     `json:"id"`
	RunID       int64     `json:"run_id"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Conclusion  string    `json:"conclusion"`
	HTMLURL     string    `json:"html_url"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Steps       []struct {
		Name        string    `json:"name"`
		Status      string    `json:"status"`
		Conclusion  string    `json:"conclusion"`
		StartedAt   time.Time `json:"started_at"`
		CompletedAt time.Time `json:"completed_at"`
	} `json:"steps"`
}

// Workflow is a workflow definition in a repository.
type Workflow struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	State string `json:"state"`
}

func actionPath(slug string, rest ...string) (string, error) {
	owner, repo, err := config.SplitSlug(slug)
	if err != nil {
		return "", err
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions"
	for _, part := range rest {
		path += "/" + url.PathEscape(part)
	}
	return path, nil
}

// WorkflowRuns lists recent Actions runs for a repository.
func (c *Client) WorkflowRuns(ctx context.Context, slug string, limit int) ([]WorkflowRun, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	path, err := actionPath(slug, "runs")
	if err != nil {
		return nil, err
	}
	path += "?per_page=" + strconv.Itoa(limit)
	var out struct {
		WorkflowRuns []WorkflowRun `json:"workflow_runs"`
	}
	if err := c.doREST(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	for i := range out.WorkflowRuns {
		out.WorkflowRuns[i].Repo = slug
	}
	return out.WorkflowRuns, nil
}

// WorkflowRunJobs lists the jobs in one Actions run.
func (c *Client) WorkflowRunJobs(ctx context.Context, slug string, runID int64) ([]WorkflowJob, error) {
	path, err := actionPath(slug, "runs", strconv.FormatInt(runID, 10), "jobs")
	if err != nil {
		return nil, err
	}
	path += "?per_page=100"
	var out struct {
		Jobs []WorkflowJob `json:"jobs"`
	}
	if err := c.doREST(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Jobs {
		out.Jobs[i].RunID = runID
	}
	return out.Jobs, nil
}

// Workflows lists workflow definitions for a repository.
func (c *Client) Workflows(ctx context.Context, slug string) ([]Workflow, error) {
	path, err := actionPath(slug, "workflows")
	if err != nil {
		return nil, err
	}
	path += "?per_page=100"
	var out struct {
		Workflows []Workflow `json:"workflows"`
	}
	if err := c.doREST(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Workflows, nil
}

// RerunWorkflowRun reruns all jobs, or only failed jobs, in a run.
func (c *Client) RerunWorkflowRun(ctx context.Context, slug string, runID int64, failedOnly bool) error {
	suffix := "rerun"
	if failedOnly {
		suffix = "rerun-failed-jobs"
	}
	path, err := actionPath(slug, "runs", strconv.FormatInt(runID, 10), suffix)
	if err != nil {
		return err
	}
	return c.doREST(ctx, http.MethodPost, path, []byte(`{}`), nil)
}

// CancelWorkflowRun cancels a queued or running Actions run.
func (c *Client) CancelWorkflowRun(ctx context.Context, slug string, runID int64) error {
	path, err := actionPath(slug, "runs", strconv.FormatInt(runID, 10), "cancel")
	if err != nil {
		return err
	}
	return c.doREST(ctx, http.MethodPost, path, []byte(`{}`), nil)
}

// DispatchWorkflow triggers a workflow_dispatch workflow.
func (c *Client) DispatchWorkflow(ctx context.Context, slug, workflow, ref string, inputs map[string]string) error {
	if ref == "" {
		return fmt.Errorf("ref is required")
	}
	path, err := actionPath(slug, "workflows", workflow, "dispatches")
	if err != nil {
		return err
	}
	body := map[string]any{"ref": ref}
	if len(inputs) > 0 {
		body["inputs"] = inputs
	}
	payload, _ := json.Marshal(body)
	return c.doREST(ctx, http.MethodPost, path, payload, nil)
}
