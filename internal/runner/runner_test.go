package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
)

func TestBranchRef(t *testing.T) {
	if got := branchRef("feature"); got != "refs/heads/feature" {
		t.Fatalf("branchRef = %s", got)
	}
	if got := branchRef("refs/heads/main"); got != "refs/heads/main" {
		t.Fatalf("branchRef = %s", got)
	}
}

func TestSafeEnv(t *testing.T) {
	t.Setenv("GH_TOKEN", "secret")
	t.Setenv("GITHUB_TOKEN", "secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cloud-secret")
	t.Setenv("NPM_TOKEN", "npm-secret")
	t.Setenv("KEEP_ME", "yes")
	env := strings.Join(safeEnv(), "\n")
	if strings.Contains(env, "secret") || strings.Contains(env, "cloud-secret") || strings.Contains(env, "npm-secret") || strings.Contains(env, "SSH_AUTH_SOCK") {
		t.Fatalf("sensitive env leaked: %s", env)
	}
	if !strings.Contains(env, "KEEP_ME=yes") {
		t.Fatalf("safe env missing: %s", env)
	}
}

func TestRunChecks(t *testing.T) {
	r := &Runner{Cfg: &config.RunnerConfig{}}
	out, ok := r.runChecks(context.Background(), t.TempDir(), []string{"true"}, time.Minute)
	if !ok {
		t.Fatalf("expected success: %s", out)
	}
	out, ok = r.runChecks(context.Background(), t.TempDir(), []string{"sh -c 'echo boom; exit 3'"}, time.Minute)
	if ok || !strings.Contains(out, "boom") {
		t.Fatalf("expected failure with output, got ok=%v out=%s", ok, out)
	}
}

func TestReadGuidelines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("be nice"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readGuidelines(dir)
	if !strings.Contains(got, "be nice") {
		t.Fatalf("guidelines missing: %s", got)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("one\ntwo"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}
	if got := firstLine(strings.Repeat("x", 200)); len(got) != 120 {
		t.Fatalf("firstLine length = %d", len(got))
	}
}

func TestCountOSVVulns(t *testing.T) {
	raw := `{"results":[{"packages":[{"vulnerabilities":[{"id":"A"},{"id":"B"}]},{"vulnerabilities":[{"id":"C"}]}]},{"packages":[{"vulnerabilities":[]}]}]}`
	if got := countOSVVulns(raw); got != 3 {
		t.Fatalf("countOSVVulns = %d, want 3", got)
	}
	if got := countOSVVulns("not json"); got != 0 {
		t.Fatalf("countOSVVulns(invalid) = %d", got)
	}
}

func TestRunScannerExitCodes(t *testing.T) {
	r := &Runner{Cfg: &config.RunnerConfig{}}
	out, code, err := r.runScanner(context.Background(), t.TempDir(), "true", time.Minute)
	if err != nil || code != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("true: code=%d err=%v out=%q", code, err, out)
	}
	out, code, err = r.runScanner(context.Background(), t.TempDir(), "echo '{}'; exit 1", time.Minute)
	if code != 1 || err == nil || !strings.Contains(out, "{}") {
		t.Fatalf("findings: code=%d err=%v out=%q", code, err, out)
	}
}

func TestSecurityScanClean(t *testing.T) {
	if !securityScanClean(0, 0, "") {
		t.Fatal("zero findings and exit 0 should be clean")
	}
	if !securityScanClean(0, 128, "No package sources found") {
		t.Fatal("no manifests should be clean")
	}
	if securityScanClean(2, 0, "") || securityScanClean(0, 128, "permission denied") {
		t.Fatal("findings or real errors must not be clean")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc\n\n[truncated]" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Fatalf("truncate short = %q", got)
	}
}
