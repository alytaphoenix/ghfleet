package discover

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T, dir string, remotes map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	for name, url := range remotes {
		run("remote", "add", name, url)
	}
}

func TestParseRemote(t *testing.T) {
	cases := map[string]string{
		"https://github.com/alytaphoenix/omlx.git": "alytaphoenix/omlx",
		"git@github.com:jundot/omlx.git":           "jundot/omlx",
		"ssh://git@github.com/foo/bar":             "foo/bar",
	}
	for raw, want := range cases {
		got, ok := ParseRemote(raw)
		if !ok || got != want {
			t.Fatalf("ParseRemote(%q) = %q, %v", raw, got, ok)
		}
	}
	if _, ok := ParseRemote("file:///tmp/x"); ok {
		t.Fatal("local remote should not parse")
	}
}

func TestRepos(t *testing.T) {
	root := t.TempDir()
	initRepo(t, filepath.Join(root, "fork"), map[string]string{
		"origin":   "https://github.com/alytaphoenix/upstream.git",
		"upstream": "https://github.com/other/upstream.git",
	})
	initRepo(t, filepath.Join(root, "own"), map[string]string{
		"origin": "https://github.com/alytaphoenix/own.git",
	})
	initRepo(t, filepath.Join(root, "readonly"), map[string]string{
		"origin": "https://github.com/other/readonly.git",
	})
	initRepo(t, filepath.Join(root, "foreign"), map[string]string{
		"origin": "https://github.com/other/upstream.git",
		"fork":   "https://github.com/notme/upstream.git",
	})
	found, err := Repos(context.Background(), root, "alytaphoenix")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 4 {
		t.Fatalf("expected 4 repos, got %d: %+v", len(found), found)
	}
	byName := map[string]Found{}
	for _, f := range found {
		byName[f.Name] = f
	}
	fork := byName["fork"]
	if fork.Upstream != "other/upstream" || fork.Fork != "alytaphoenix/upstream" || fork.FetchRemote != "upstream" || fork.PushRemote != "origin" {
		t.Fatalf("unexpected fork: %+v", fork)
	}
	own := byName["own"]
	if !own.Own || own.ReadOnly || own.Fork != "alytaphoenix/own" {
		t.Fatalf("unexpected own: %+v", own)
	}
	ro := byName["readonly"]
	if !ro.ReadOnly || ro.PushRemote != "" {
		t.Fatalf("unexpected readonly: %+v", ro)
	}
	foreign := byName["foreign"]
	if !foreign.ReadOnly || foreign.Fork != "" || foreign.PushRemote != "" {
		t.Fatalf("foreign fork must be read-only: %+v", foreign)
	}
}
