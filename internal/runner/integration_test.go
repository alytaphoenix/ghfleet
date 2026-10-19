package runner_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/runner"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
	return string(out)
}

func TestForkSyncEndToEnd(t *testing.T) {
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
	gitCmd(t, seed, "commit", "--allow-empty", "-m", "one")
	gitCmd(t, seed, "remote", "add", "upstream", upstream)
	gitCmd(t, seed, "remote", "add", "fork", fork)
	gitCmd(t, seed, "push", "upstream", "main")
	gitCmd(t, seed, "push", "fork", "main")
	gitCmd(t, seed, "commit", "--allow-empty", "-m", "two")
	gitCmd(t, seed, "push", "upstream", "main")
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
	job := jobs.NewJob(jobs.KindForkSync, repoCfg, github.PR{Number: 0, BaseRef: "main"}, "fork-key", jobs.Spec{}, nil)
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}

	rc := &config.RunnerConfig{
		ServerURL: ts.URL,
		ID:        "test-runner",
		Labels:    []string{"test"},
		WorkRoot:  filepath.Join(root, "worktrees"),
		Agent:     config.AgentConfig{Command: "true"},
		Repos: []config.RunnerRepo{{
			Name: "test", Path: work, FetchRemote: "upstream", PushRemote: "origin", DefaultBranch: "main",
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

	want := gitCmd(t, upstream, "rev-parse", "main")
	got := gitCmd(t, fork, "rev-parse", "main")
	if want != got {
		t.Fatalf("fork main = %s, upstream main = %s", got, want)
	}
	j, _ := st.GetJob(job.ID)
	if j.Status != "applied" {
		t.Fatalf("status = %s", j.Status)
	}
}
