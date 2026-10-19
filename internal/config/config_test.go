package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadServerConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := `
server:
  listen: 127.0.0.1:9999
  state_dir: ` + dir + `/state
  poll_interval: 30s
approvals:
  push: ask
  comment: ask
  merge: ask
  fork_sync: auto
limits:
  job_timeout: 10m
repos:
  - name: omlx
    upstream: jundot/omlx
    fork: alytaphoenix/omlx
    default_branch: main
    fetch_remote: origin
    push_remote: fork
`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:9999" || cfg.Server.PollInterval.D() != 30*time.Second {
		t.Fatalf("unexpected server config: %+v", cfg.Server)
	}
	if cfg.Limits.MaxJobsPerHour != 6 || cfg.Limits.ApprovalTimeout.D() != 24*time.Hour {
		t.Fatalf("defaults not applied: %+v", cfg.Limits)
	}
	if cfg.Approvals.Push != "ask" || cfg.Approvals.ForkSync != "auto" {
		t.Fatalf("unexpected approvals: %+v", cfg.Approvals)
	}
	repo := cfg.Repos[0]
	if repo.Name != "omlx" || repo.PushRemote != "fork" || repo.ForkSyncInterval.D() != 24*time.Hour {
		t.Fatalf("unexpected repo: %+v", repo)
	}
}

func TestLoadRejectsBadDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  poll_interval: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected duration error")
	}
}

func TestRunnerConfigRequiresExistingPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runner.yaml")
	raw := `
server_url: http://127.0.0.1:8787
repos:
  - name: missing
    path: ` + dir + `/nope
`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunner(path); err == nil {
		t.Fatal("expected missing path error")
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Server: ServerConfig{Listen: "127.0.0.1:8787", StateDir: dir, PollInterval: Duration(2 * time.Minute)},
		Repos:  []Repo{{Name: "r", Upstream: "o/r"}},
	}
	cfg.ApplyDefaults()
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 || got.Repos[0].DefaultBranch != "main" {
		t.Fatalf("unexpected round trip: %+v", got.Repos)
	}
}

func TestExpandPath(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := ExpandPath("~/x"); got != filepath.Join(home, "x") {
		t.Fatalf("ExpandPath = %q", got)
	}
}

func TestUpstreamWithoutForkIsReadOnly(t *testing.T) {
	cfg := &Config{Repos: []Repo{{Name: "x", Upstream: "other/x"}}}
	cfg.ApplyDefaults()
	if !cfg.Repos[0].ReadOnly {
		t.Fatal("a non-owned repo without a fork must be read-only")
	}
}
