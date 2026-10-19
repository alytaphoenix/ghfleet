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

func TestUpstreamMergeCleanEndToEnd(t *testing.T) {
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
	gitCmd(t, seed, "commit", "--allow-empty", "-m", "base")
	gitCmd(t, seed, "remote", "add", "upstream", upstream)
	gitCmd(t, seed, "remote", "add", "fork", fork)
	gitCmd(t, seed, "push", "upstream", "main")
	gitCmd(t, seed, "push", "fork", "main")

	// The PR branch adds a file.
	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "feature")
	gitCmd(t, seed, "push", "fork", "feature")
	headSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	// Upstream advances main with an unrelated file: merge is clean.
	gitCmd(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "upstream.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "upstream")
	gitCmd(t, seed, "push", "upstream", "main")
	baseSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	gitCmd(t, root, "clone", fork, work)
	gitCmd(t, work, "config", "user.email", "test@example.com")
	gitCmd(t, work, "config", "user.name", "test")
	gitCmd(t, work, "remote", "add", "upstream", upstream)

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
		Repo: "test", Slug: "up/test", Number: 2, Title: "merge", Author: "me",
		HeadRef: "feature", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA,
	}
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	job := jobs.NewJob(jobs.KindUpstreamMerge, repoCfg, pr, "merge-key", jobs.Spec{}, nil)
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}

	rc := &config.RunnerConfig{
		ServerURL: ts.URL,
		ID:        "test-runner",
		Labels:    []string{"test"},
		WorkRoot:  filepath.Join(root, "worktrees"),
		Agent:     config.AgentConfig{Command: "false", Timeout: config.Duration(2 * time.Minute)},
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

	// The fork branch must be a merge commit containing both sides.
	parents := strings.Fields(gitCmd(t, fork, "rev-list", "--parents", "-n", "1", "feature"))
	if len(parents) != 3 {
		t.Fatalf("expected a two-parent merge commit, got %v", parents)
	}
	if !strings.Contains(gitCmd(t, fork, "show", "feature:feature.txt"), "feature") {
		t.Fatal("merge lost the PR change")
	}
	if !strings.Contains(gitCmd(t, fork, "show", "feature:upstream.txt"), "upstream") {
		t.Fatal("merge lost the upstream change")
	}
	gitCmd(t, fork, "merge-base", "--is-ancestor", baseSHA, "feature")
}

func TestUpstreamMergeConflictEndToEnd(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "upstream", upstream)
	gitCmd(t, seed, "remote", "add", "fork", fork)
	gitCmd(t, seed, "push", "upstream", "main")
	gitCmd(t, seed, "push", "fork", "main")

	gitCmd(t, seed, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "commit", "-am", "feature")
	gitCmd(t, seed, "push", "fork", "feature")
	headSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	gitCmd(t, seed, "checkout", "main")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "commit", "-am", "upstream")
	gitCmd(t, seed, "push", "upstream", "main")
	baseSHA := strings.TrimSpace(gitCmd(t, seed, "rev-parse", "HEAD"))

	gitCmd(t, root, "clone", fork, work)
	gitCmd(t, work, "config", "user.email", "test@example.com")
	gitCmd(t, work, "config", "user.name", "test")
	gitCmd(t, work, "remote", "add", "upstream", upstream)

	agentScript := filepath.Join(root, "fake-merge-agent.sh")
	script := `#!/bin/sh
printf 'resolved\n' > a.txt
git add a.txt
cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"{\"decision\":\"fix\",\"summary\":\"resolved merge conflict\",\"risk\":\"low\",\"needs_human\":false}"}
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
		Repo: "test", Slug: "up/test", Number: 3, Title: "merge conflict", Author: "me",
		HeadRef: "feature", HeadSHA: headSHA, BaseRef: "main", BaseSHA: baseSHA,
	}
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	job := jobs.NewJob(jobs.KindUpstreamMerge, repoCfg, pr, "merge-conflict-key", jobs.Spec{}, nil)
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

	parents := strings.Fields(gitCmd(t, fork, "rev-list", "--parents", "-n", "1", "feature"))
	if len(parents) != 3 {
		t.Fatalf("expected a two-parent merge commit, got %v", parents)
	}
	content := gitCmd(t, fork, "show", "feature:a.txt")
	if !strings.Contains(content, "resolved") {
		t.Fatalf("fork feature does not contain resolution: %s", content)
	}
	gitCmd(t, fork, "merge-base", "--is-ancestor", baseSHA, "feature")
}
