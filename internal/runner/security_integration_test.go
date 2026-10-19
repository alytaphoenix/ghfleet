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

func TestSecurityFixEndToEnd(t *testing.T) {
	root := t.TempDir()
	fork := filepath.Join(root, "fork.git")
	seed := filepath.Join(root, "seed")
	work := filepath.Join(root, "work")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "init", "--bare", "-b", "main", fork)
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	gitCmd(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "go.mod"), []byte("module example.com/test\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, seed, "add", ".")
	gitCmd(t, seed, "commit", "-m", "base")
	gitCmd(t, seed, "remote", "add", "origin", fork)
	gitCmd(t, seed, "push", "origin", "main")

	gitCmd(t, root, "clone", fork, work)
	gitCmd(t, work, "config", "user.email", "test@example.com")
	gitCmd(t, work, "config", "user.name", "test")

	scanner := filepath.Join(root, "fake-scanner.sh")
	scannerScript := `#!/bin/sh
cat <<'JSON'
{"results":[{"packages":[{"vulnerabilities":[{"id":"TEST-1"}]}]}]}
JSON
exit 1
`
	if err := os.WriteFile(scanner, []byte(scannerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	agentScript := filepath.Join(root, "fake-security-agent.sh")
	script := `#!/bin/sh
printf 'fixed\n' > fix.txt
cat <<'JSON'
{"type":"result","subtype":"success","is_error":false,"result":"{\"decision\":\"fix\",\"summary\":\"updated vulnerable dependency\",\"risk\":\"low\",\"needs_human\":false}"}
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
		Name: "test", Upstream: "me/test", Own: true,
		DefaultBranch: "main", FetchRemote: "origin", PushRemote: "origin",
	}
	pr := github.PR{Repo: "test", Slug: "me/test", Number: 0, BaseRef: "main", HeadRef: "main"}
	job := jobs.NewJob(jobs.KindSecurity, repoCfg, pr, "security-key", jobs.Spec{}, nil)
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}

	rc := &config.RunnerConfig{
		ServerURL: ts.URL,
		ID:        "test-runner",
		Labels:    []string{"test"},
		WorkRoot:  filepath.Join(root, "worktrees"),
		Security:  config.SecurityConfig{Command: scanner, Timeout: config.Duration(time.Minute)},
		Agent:     config.AgentConfig{Command: agentScript, Timeout: config.Duration(2 * time.Minute)},
		Repos: []config.RunnerRepo{{
			Name: "test", Path: work, Checks: []string{"true"},
			FetchRemote: "origin", PushRemote: "origin", DefaultBranch: "main",
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
	j, _ := st.GetJob(job.ID)
	result := jobs.ParseResult(j.ResultJSON)
	if result.PatchSize == 0 || result.PatchSHA256 == "" {
		t.Fatalf("expected uploaded patch: %+v", result)
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

	content := gitCmd(t, fork, "show", "main:fix.txt")
	if !strings.Contains(content, "fixed") {
		t.Fatalf("main does not contain the fix: %s", content)
	}
}

func TestSecurityCleanScanIsSilent(t *testing.T) {
	root := t.TempDir()
	fork := filepath.Join(root, "fork.git")
	seed := filepath.Join(root, "seed")
	work := filepath.Join(root, "work")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, root, "init", "--bare", "-b", "main", fork)
	gitCmd(t, seed, "init", "-b", "main")
	gitCmd(t, seed, "config", "user.email", "test@example.com")
	gitCmd(t, seed, "config", "user.name", "test")
	gitCmd(t, seed, "commit", "--allow-empty", "-m", "base")
	gitCmd(t, seed, "remote", "add", "origin", fork)
	gitCmd(t, seed, "push", "origin", "main")
	gitCmd(t, root, "clone", fork, work)
	gitCmd(t, work, "config", "user.email", "test@example.com")
	gitCmd(t, work, "config", "user.name", "test")

	scanner := filepath.Join(root, "clean-scanner.sh")
	if err := os.WriteFile(scanner, []byte("#!/bin/sh\necho '{\"results\":[]}'\n"), 0o755); err != nil {
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
		Approvals: config.Approvals{Push: "ask", Comment: "ask", Merge: "ask", ForkSync: "auto"},
		Limits:    config.Limits{JobTimeout: config.Duration(5 * time.Minute), ApprovalTimeout: config.Duration(10 * time.Second)},
	}
	cfg.ApplyDefaults()
	s := server.New(cfg, st, nil, "me", slog.Default())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	repoCfg := config.Repo{Name: "test", Upstream: "me/test", Own: true, DefaultBranch: "main"}
	pr := github.PR{Repo: "test", Slug: "me/test", Number: 0, BaseRef: "main", HeadRef: "main"}
	job := jobs.NewJob(jobs.KindSecurity, repoCfg, pr, "clean-key", jobs.Spec{}, nil)
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}

	rc := &config.RunnerConfig{
		ServerURL: ts.URL,
		ID:        "test-runner",
		Labels:    []string{"test"},
		WorkRoot:  filepath.Join(root, "worktrees"),
		Security:  config.SecurityConfig{Command: scanner, Timeout: config.Duration(time.Minute)},
		Agent:     config.AgentConfig{Command: "false"},
		Repos: []config.RunnerRepo{{
			Name: "test", Path: work, Checks: []string{"true"},
			FetchRemote: "origin", PushRemote: "origin", DefaultBranch: "main",
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
			t.Fatalf("clean scan did not apply: %+v %v", j, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(job.ID)
	result := jobs.ParseResult(j.ResultJSON)
	if !result.Silent || result.PatchSize != 0 || result.Decision != "noop" {
		t.Fatalf("unexpected clean result: %+v", result)
	}
}
