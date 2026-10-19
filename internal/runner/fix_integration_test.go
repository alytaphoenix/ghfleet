package runner_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/runner"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

func TestReviewFixEndToEnd(t *testing.T) {
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	fork := filepath.Join(root, "fork.git")
	seed := filepath.Join(root, "seed")
	work := filepath.Join(root, "work")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "init", "--bare", "-b", "main", upstream)
	gitCmd(t, root, "init", "--bare", "-b", "main", fork)
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	gitCmd(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "upstream", upstream)
	gitCmd(t, seed, "remote", "add", "fork", fork)
	gitCmd(t, seed, "push", "upstream", "main")
	gitCmd(t, seed, "push", "fork", "main")
	baseSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "file.txt"), []byte("base\nfeature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "commit", "-am", "feature")
	gitCmd(t, seed, "push", "fork", "feature")
	headSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	gitCmd(t, root, "clone", fork, work)
	gitCmd(t, work, "config", "user.email", "test@example.com")
	gitCmd(t, work, "config", "user.name", "test")
	gitCmd(t, work, "remote", "add", "upstream", upstream)
	gitCmd(t, work, "fetch", "upstream")

	agentScript := filepath.Join(root, "fake-agent.sh")
	script := `#!/bin/sh
printf 'fixed\n' >> file.txt
cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"{\"decision\":\"fix\",\"summary\":\"fixed the review comment\",\"risk\":\"low\",\"needs_human\":false,\"reply\":\"fixed\"}"}
JSON
`
	if err := os.WriteFile(agentScript, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(root, "state")
	st, err := store.Open(filepath.Join(dir, "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{
		Server:    config.ServerConfig{Listen: "127.0.0.1:0", StateDir: dir},
		Approvals: config.Approvals{Push: "ask", Comment: "never", Merge: "ask", ForkSync: "auto"},
		Limits:    config.Limits{JobTimeout: config.Duration(5 * time.Minute), ApprovalTimeout: config.Duration(10 * time.Second)},
	}
	cfg.ApplyDefaults()
	s := server.New(cfg, st, nil, "me", slog.Default())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	repoCfg := config.Repo{
		Name: "test", Upstream: "up/test", Fork: "me/test", Own: false,
		DefaultBranch: "main", FetchRemote: "upstream", PushRemote: "origin",
	}
	pr := github.PR{
		Repo: "test", Slug: "up/test", Number: 1, Title: "feature", Author: "me",
		HeadRef: "feature", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA,
		Threads: []github.Thread{{
			ID: "t1", Path: "file.txt", Line: 2,
			Comments: []github.Comment{{ID: "c1", Author: "reviewer", Body: "please change this"}},
		}},
	}
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	job := jobs.NewJob(jobs.KindReviewResponse, repoCfg, pr, "fix-key",
		jobs.Spec{Threads: jobs.Threads(pr.Threads)}, nil)
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}

	rc := &config.RunnerConfig{
		ServerURL: ts.URL,
		ID:        "test-runner",
		Labels:    []string{"test"},
		WorkRoot:  filepath.Join(root, "worktrees"),
		Agent:     config.AgentConfig{Command: agentScript, Timeout: config.Duration(2 * time.Minute)},
		Repos: []config.RunnerRepo{{
			Name: "test", Path: work, Checks: []string{"true"},
			FetchRemote: "upstream", PushRemote: "origin", DefaultBranch: "main",
		}},
	}
	rc.ApplyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := runner.New(rc, slog.Default())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := st.GetJob(job.ID)
		if err == nil && j.Status == "awaiting_approval" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not reach approval: %+v %v", j, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := st.Approve(job.ID); err != nil {
		t.Fatal(err)
	}
	for {
		j, err := st.GetJob(job.ID)
		if err == nil && j.Status == "applied" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not apply: %+v %v", j, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	content := gitCmd(t, fork, "show", "feature:file.txt")
	if !strings.Contains(content, "fixed") {
		t.Fatalf("fork feature does not contain fix: %s", content)
	}
}
