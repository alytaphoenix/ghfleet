package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/client"
	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

func testServer(t *testing.T, token string) (*server.Server, *httptest.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1:0", StateDir: dir, APIToken: token},
		Approvals: config.Approvals{
			Push: "ask", Comment: "never", Merge: "ask", ForkSync: "auto",
		},
		Limits: config.Limits{JobTimeout: config.Duration(10 * time.Minute)},
	}
	cfg.ApplyDefaults()
	s := server.New(cfg, st, nil, "me", slog.Default())
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return s, ts, st
}

func TestHealthAndAuth(t *testing.T) {
	_, ts, _ := testServer(t, "secret")
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/api/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth = %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth = %d: %s", resp.StatusCode, body)
	}
}

func TestRunnerJobLifecycle(t *testing.T) {
	_, ts, st := testServer(t, "")
	ctx := context.Background()
	c := client.New(ts.URL, "")

	if err := c.Register(ctx, server.RunnerInfo{ID: "r1", Labels: []string{"darwin"}, Repos: []string{"omlx"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertPR(github.PR{Repo: "omlx", Number: 1, HeadSHA: "abc", BaseSHA: "def"}); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: "job1", Repo: "omlx", PRNumber: 1, HeadSHA: "abc", Kind: "review-response", Status: "pending",
		SpecJSON:       `{"job_id":"job1","kind":"review-response","repo":{"name":"omlx","upstream":"jundot/omlx"},"pr":{"number":1,"head_ref":"feature","head_sha":"abc"},"threads":[]}`,
		IdempotencyKey: "key1", RequiredLabels: []string{"darwin"}, CreatedAt: time.Now(),
	}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}

	claimed, err := c.Claim(ctx, "r1", []string{"darwin"}, []string{"omlx"})
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.Job.ID != "job1" {
		t.Fatalf("unexpected claim: %+v", claimed)
	}
	if err := c.Start(ctx, "job1", "r1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Result(ctx, "job1", server.RunnerResult{
		RunnerID: "r1",
		Result:   jobsResultFix(),
		Risk:     "low",
		Summary:  "fixed it",
	}); err != nil {
		t.Fatal(err)
	}
	j, err := c.Job(ctx, "job1", false)
	if err != nil {
		t.Fatal(err)
	}
	if j.Job.Status != "awaiting_approval" {
		t.Fatalf("status = %s", j.Job.Status)
	}
	if err := c.Approve(ctx, "job1"); err != nil {
		t.Fatal(err)
	}
	instr, err := c.Instruction(ctx, "job1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if instr.Action != "apply" {
		t.Fatalf("instruction = %s", instr.Action)
	}
	if err := c.Applied(ctx, "job1", server.RunnerResult{RunnerID: "r1", Result: jobsResultFix()}); err != nil {
		t.Fatal(err)
	}
	j, _ = c.Job(ctx, "job1", false)
	if j.Job.Status != "applied" {
		t.Fatalf("status = %s", j.Job.Status)
	}
}

func jobsResultFix() jobs.Result {
	return jobs.Result{Decision: "fix", Summary: "fixed it", Risk: "low", CommitSHA: "abc"}
}

func TestNilGitHubDoesNotCreateTypedNilComments(t *testing.T) {
	s, _, _ := testServer(t, "")
	if s.Comments != nil {
		t.Fatal("nil GitHub client must not become a non-nil comment interface")
	}
}

func TestSecurityNoopAutoApproved(t *testing.T) {
	_, ts, st := testServer(t, "")
	ctx := context.Background()
	c := client.New(ts.URL, "")
	if err := c.Register(ctx, server.RunnerInfo{ID: "r1", Labels: []string{"darwin"}, Repos: []string{"omlx"}}); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: "sec1", Repo: "omlx", PRNumber: 0, Kind: jobs.KindSecurity, Status: "pending",
		SpecJSON:       `{"kind":"security-fix","repo":{"name":"omlx","upstream":"jundot/omlx"},"pr":{"number":0,"head_ref":"main"}}`,
		IdempotencyKey: "sec-key", CreatedAt: time.Now(),
	}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}
	claimed, err := c.Claim(ctx, "r1", nil, []string{"omlx"})
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	if err := c.Start(ctx, "sec1", "r1"); err != nil {
		t.Fatal(err)
	}
	result := jobs.Result{Decision: "noop", Summary: "No known dependency vulnerabilities found.", Risk: "low", Silent: true}
	if err := c.Result(ctx, "sec1", server.RunnerResult{RunnerID: "r1", Result: result}); err != nil {
		t.Fatal(err)
	}
	j, err := c.Job(ctx, "sec1", false)
	if err != nil {
		t.Fatal(err)
	}
	if j.Job.Status != "approved" {
		t.Fatalf("status = %s, want approved", j.Job.Status)
	}
	instr, err := c.Instruction(ctx, "sec1", "r1")
	if err != nil {
		t.Fatal(err)
	}
	if instr.Action != "apply" {
		t.Fatalf("instruction = %s", instr.Action)
	}
}

func TestReviewPRManualEnqueue(t *testing.T) {
	s, ts, st := testServer(t, "")
	s.Cfg.Repos = []config.Repo{{Name: "test", Upstream: "me/test", FetchRemote: "origin", PushRemote: "origin", DefaultBranch: "main"}}
	c := client.New(ts.URL, "")
	if _, err := c.ReviewPR(context.Background(), "test", 1); err == nil {
		t.Fatal("expected untracked PR to be rejected")
	}
	if err := st.UpsertPR(github.PR{Repo: "test", Number: 1, HeadSHA: "abc", BaseSHA: "def", HeadRef: "feature", BaseRef: "main", State: "OPEN"}); err != nil {
		t.Fatal(err)
	}
	job, err := c.ReviewPR(context.Background(), "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if job.Kind != jobs.KindIncomingReview || job.PRNumber != 1 {
		t.Fatalf("unexpected review job: %+v", job)
	}
}

func TestActionsValidation(t *testing.T) {
	s, ts, _ := testServer(t, "")
	if _, err := client.New(ts.URL, "").Actions(context.Background(), "missing", 5); err == nil {
		t.Fatal("expected unknown repo error")
	}
	s.Cfg.Repos = []config.Repo{{Name: "test", Upstream: "me/test", ReadOnly: true}}
	if err := client.New(ts.URL, "").RerunAction(context.Background(), "test", 1, true); err == nil {
		t.Fatal("expected read-only repo rejection")
	}
	if err := client.New(ts.URL, "").CancelAction(context.Background(), "test", 1); err == nil {
		t.Fatal("expected read-only repo rejection")
	}
}
