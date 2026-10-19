package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestIsAgentArtifact(t *testing.T) {
	for _, p := range []string{".serena/project.yml", ".claude/settings.json", "target/debug/foo", "node_modules/x", ".aider.chat.history.md", "build/out"} {
		if !IsAgentArtifact(p) {
			t.Fatalf("%s should be an artifact", p)
		}
	}
	for _, p := range []string{"src/main.rs", "tests/new_test.rs", "README.md", ".github/workflows/ci.yml"} {
		if IsAgentArtifact(p) {
			t.Fatalf("%s should not be an artifact", p)
		}
	}
}

func TestCleanAgentArtifacts(t *testing.T) {
	dir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-qm", "init")
	if err := os.MkdirAll(filepath.Join(dir, ".serena"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".serena", "project.yml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanAgentArtifacts(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".serena")); !os.IsNotExist(err) {
		t.Fatal(".serena should be removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "new_test.go")); err != nil {
		t.Fatal("legitimate new file should remain")
	}
}
