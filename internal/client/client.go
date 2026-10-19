// Package client is the HTTP client used by the CLI/TUI and by runners.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/reconcile"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

// Client talks to a ghfleet server.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New creates a client.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		if e.Error == "" {
			e.Error = resp.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// Status returns the server dashboard.
func (c *Client) Status(ctx context.Context) (server.StatusResponse, error) {
	var out server.StatusResponse
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &out)
	return out, err
}

// PRs returns tracked PRs.
func (c *Client) PRs(ctx context.Context) ([]store.PR, error) {
	var out struct {
		PRs []store.PR `json:"prs"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/prs", nil, &out)
	return out.PRs, err
}

// Scan triggers a reconcile pass.
func (c *Client) Scan(ctx context.Context) (reconcile.Summary, error) {
	var out reconcile.Summary
	err := c.do(ctx, http.MethodPost, "/api/v1/scan", map[string]any{}, &out)
	return out, err
}

// SyncRepo enqueues a manual fork sync.
func (c *Client) SyncRepo(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(name)+"/sync", map[string]any{}, nil)
}

// SecurityRepo enqueues a manual dependency scan for one repo.
func (c *Client) SecurityRepo(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(name)+"/security", map[string]any{}, nil)
}

// Actions lists recent GitHub Actions runs, optionally for one repo.
func (c *Client) Actions(ctx context.Context, repo string, limit int) ([]github.WorkflowRun, error) {
	path := "/api/v1/actions?limit=" + strconv.Itoa(limit)
	if repo != "" {
		path += "&repo=" + url.QueryEscape(repo)
	}
	var out struct {
		Runs []github.WorkflowRun `json:"runs"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Runs, err
}

// ActionJobs lists jobs for one Actions run.
func (c *Client) ActionJobs(ctx context.Context, repo string, runID int64) ([]github.WorkflowJob, error) {
	path := "/api/v1/actions/jobs?repo=" + url.QueryEscape(repo) + "&run_id=" + strconv.FormatInt(runID, 10)
	var out struct {
		Jobs []github.WorkflowJob `json:"jobs"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Jobs, err
}

// ActionWorkflows lists workflows in a repo.
func (c *Client) ActionWorkflows(ctx context.Context, repo string) ([]github.Workflow, error) {
	var out struct {
		Workflows []github.Workflow `json:"workflows"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/actions/workflows?repo="+url.QueryEscape(repo), nil, &out)
	return out.Workflows, err
}

// RerunAction reruns an Actions run (failed jobs unless failedOnly is false).
func (c *Client) RerunAction(ctx context.Context, repo string, runID int64, failedOnly bool) error {
	return c.do(ctx, http.MethodPost, "/api/v1/actions/rerun", map[string]any{
		"repo": repo, "run_id": runID, "failed_only": failedOnly,
	}, nil)
}

// CancelAction cancels an Actions run.
func (c *Client) CancelAction(ctx context.Context, repo string, runID int64) error {
	return c.do(ctx, http.MethodPost, "/api/v1/actions/cancel", map[string]any{"repo": repo, "run_id": runID}, nil)
}

// DispatchAction triggers a workflow_dispatch workflow.
func (c *Client) DispatchAction(ctx context.Context, repo, workflow, ref string, inputs map[string]string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/actions/dispatch", map[string]any{
		"repo": repo, "workflow": workflow, "ref": ref, "inputs": inputs,
	}, nil)
}

// ReviewPR enqueues a read-only agent review for one PR.
func (c *Client) ReviewPR(ctx context.Context, repo string, number int) (store.Job, error) {
	var out struct {
		Job store.Job `json:"job"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/repos/"+url.PathEscape(repo)+"/review", map[string]any{"number": number}, &out)
	return out.Job, err
}

// Jobs lists jobs.
func (c *Client) Jobs(ctx context.Context, status string) ([]store.Job, error) {
	path := "/api/v1/jobs"
	if status != "" {
		path += "?status=" + status
	}
	var out struct {
		Jobs []store.Job `json:"jobs"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out.Jobs, err
}

// Job returns one job, optionally with its full patch.
func (c *Client) Job(ctx context.Context, id string, includePatch bool) (server.JobDetail, error) {
	path := "/api/v1/jobs/" + id
	if includePatch {
		path += "?include=patch"
	}
	var out server.JobDetail
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// Patch returns a job's patch artifact.
func (c *Client) Patch(ctx context.Context, id string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/api/v1/jobs/"+id+"/patch", nil)
	if err != nil {
		return "", err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	return string(raw), nil
}

// Approve approves a job.
func (c *Client) Approve(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/approve", map[string]any{}, nil)
}

// Deny denies a job.
func (c *Client) Deny(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/deny", map[string]any{}, nil)
}

// Runners lists runners.
func (c *Client) Runners(ctx context.Context) ([]store.Runner, error) {
	var out struct {
		Runners []store.Runner `json:"runners"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/runners", nil, &out)
	return out.Runners, err
}

// Register registers a runner.
func (c *Client) Register(ctx context.Context, info server.RunnerInfo) error {
	return c.do(ctx, http.MethodPost, "/api/v1/runners/register", info, nil)
}

// Heartbeat updates a runner's last-seen time.
func (c *Client) Heartbeat(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/runners/"+url.PathEscape(id)+"/heartbeat", map[string]any{}, nil)
}

// Claim asks for a job.
func (c *Client) Claim(ctx context.Context, id string, labels, repos []string) (*server.JobDetail, error) {
	var out server.JobDetail
	err := c.do(ctx, http.MethodPost, "/api/v1/runners/"+url.PathEscape(id)+"/claim", map[string]any{"id": id, "labels": labels, "repos": repos}, &out)
	if err != nil {
		return nil, err
	}
	if out.Job.ID == "" {
		return nil, nil
	}
	return &out, nil
}

// Start marks a job running.
func (c *Client) Start(ctx context.Context, id, runnerID string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/start", map[string]any{"runner_id": runnerID}, nil)
}

// Result uploads an agent result.
func (c *Client) Result(ctx context.Context, id string, res server.RunnerResult) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/result", res, nil)
}

// UploadArtifact uploads a patch or log.
func (c *Client) UploadArtifact(ctx context.Context, id, kind string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/jobs/"+id+"/artifact?kind="+kind, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf("%s", strings.TrimSpace(string(raw)))
	}
	return nil
}

// Instruction polls the next action for a runner-owned job.
func (c *Client) Instruction(ctx context.Context, id, runnerID string) (store.Instruction, error) {
	var out store.Instruction
	err := c.do(ctx, http.MethodGet, "/api/v1/jobs/"+id+"/instruction?runner_id="+url.QueryEscape(runnerID), nil, &out)
	return out, err
}

// Applied reports a successful apply.
func (c *Client) Applied(ctx context.Context, id string, res server.RunnerResult) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/applied", res, nil)
}

// Failed reports a failed job.
func (c *Client) Failed(ctx context.Context, id, runnerID, errText string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/jobs/"+id+"/failed", map[string]any{"runner_id": runnerID, "error": errText}, nil)
}
