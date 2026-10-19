package reconcile

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

type fakeGitHub struct {
	prs      []github.PR
	incoming []github.PR
}

func (f fakeGitHub) AuthoredPRs(ctx context.Context, slugs []string, viewer string) ([]github.PR, error) {
	return f.prs, nil
}

func (f fakeGitHub) IncomingPRs(ctx context.Context, slugs []string, viewer string) ([]github.PR, error) {
	return f.incoming, nil
}

func testConfig(t *testing.T) *config.Config {
	cfg := &config.Config{
		Server: config.ServerConfig{StateDir: t.TempDir()},
		Limits: config.Limits{MaxFixAttemptsPerPR: 2},
		Repos: []config.Repo{
			{
				Name: "omlx", Upstream: "jundot/omlx", Fork: "alytaphoenix/omlx",
				DefaultBranch: "main", FetchRemote: "origin", PushRemote: "fork",
				AutoForkSync: true, ForkSyncInterval: config.Duration(24 * time.Hour),
			},
		},
	}
	cfg.ApplyDefaults()
	return cfg
}

func TestReconcileEnqueues(t *testing.T) {
	cfg := testConfig(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	pr := github.PR{
		Repo: "jundot/omlx", Slug: "jundot/omlx", Number: 1, Title: "t", Author: "alytaphoenix",
		HeadRef: "feature", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def",
		Mergeable: "CONFLICTING", CheckState: "FAILURE",
		Checks: []github.Check{{Name: "ci", Status: "COMPLETED", Conclusion: "FAILURE"}},
		Threads: []github.Thread{{
			ID: "t1", Path: "a.go", Line: 3,
			Comments: []github.Comment{{ID: "c1", Author: "reviewer", Body: "fix", CreatedAt: time.Now()}},
		}},
	}
	r := &Reconciler{Cfg: cfg, Store: st, GH: fakeGitHub{prs: []github.PR{pr}}, Viewer: "alytaphoenix",
		Now: func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }}
	summary := r.Run(context.Background())
	if summary.NewJobs != 4 {
		t.Fatalf("expected 4 jobs (review, ci, rebase, fork sync), got %d: %+v", summary.NewJobs, summary)
	}
	jobs, err := st.ListJobs([]string{"pending"}, 20)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, j := range jobs {
		kinds[j.Kind]++
	}
	for _, kind := range []string{"review-response", "ci-triage", "rebase", "fork-sync"} {
		if kinds[kind] != 1 {
			t.Fatalf("missing %s job: %+v", kind, kinds)
		}
	}

	// A second scan should not duplicate active work.
	summary = r.Run(context.Background())
	if summary.NewJobs != 0 {
		t.Fatalf("expected no new jobs, got %d", summary.NewJobs)
	}
}

func TestReconcileSkipsReadOnly(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repos[0].ReadOnly = true
	cfg.Repos[0].AutoForkSync = false
	st, err := store.Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pr := github.PR{
		Repo: "jundot/omlx", Slug: "jundot/omlx", Number: 1, Mergeable: "CONFLICTING", CheckState: "FAILURE",
		Threads: []github.Thread{{ID: "t", Comments: []github.Comment{{Author: "reviewer", ID: "c"}}}},
	}
	r := &Reconciler{Cfg: cfg, Store: st, GH: fakeGitHub{prs: []github.PR{pr}}, Viewer: "me"}
	summary := r.Run(context.Background())
	if summary.NewJobs != 0 {
		t.Fatalf("read-only repo should not enqueue, got %+v", summary)
	}
}

func TestReconcileIncomingReview(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repos[0] = config.Repo{
		Name: "own", Upstream: "me/own", Own: true, ReviewIncoming: true,
		DefaultBranch: "main", FetchRemote: "origin", PushRemote: "origin",
	}
	cfg.ApplyDefaults()
	st, err := store.Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pr := github.PR{
		Repo: "me/own", Slug: "me/own", Number: 5, Title: "contributor change", Author: "contributor",
		HeadRef: "feature", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def", State: "OPEN",
	}
	r := &Reconciler{Cfg: cfg, Store: st, GH: fakeGitHub{incoming: []github.PR{pr}}, Viewer: "me"}
	summary := r.Run(context.Background())
	if summary.NewJobs != 1 {
		t.Fatalf("expected one incoming review, got %+v", summary)
	}
	jobs, _ := st.ListJobs([]string{"pending"}, 10)
	if len(jobs) != 1 || jobs[0].Kind != "incoming-review" || jobs[0].PRNumber != 5 {
		t.Fatalf("unexpected jobs: %+v", jobs)
	}
	summary = r.Run(context.Background())
	if summary.NewJobs != 0 {
		t.Fatalf("expected no duplicate incoming review, got %+v", summary)
	}
}

func TestReconcileEnqueuesSecurityScan(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repos[0].AutoForkSync = false
	cfg.Repos[0].AutoSecurity = true
	cfg.Repos[0].SecurityInterval = config.Duration(24 * time.Hour)
	st, err := store.Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	r := &Reconciler{Cfg: cfg, Store: st, GH: fakeGitHub{}, Viewer: "alytaphoenix",
		Now: func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) }}
	summary := r.Run(context.Background())
	if summary.NewJobs != 1 {
		t.Fatalf("expected one security job, got %+v", summary)
	}
	jobsList, err := st.ListJobs([]string{"pending"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobsList) != 1 || jobsList[0].Kind != "security-fix" || jobsList[0].PRNumber != 0 {
		t.Fatalf("unexpected jobs: %+v", jobsList)
	}
	summary = r.Run(context.Background())
	if summary.NewJobs != 0 {
		t.Fatalf("expected no duplicate security job, got %+v", summary)
	}
}

func TestReconcileEnqueuesUpstreamMerge(t *testing.T) {
	cfg := testConfig(t)
	cfg.Repos[0].AutoForkSync = false
	cfg.Repos[0].MergeUpstream = true
	st, err := store.Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pr := github.PR{
		Repo: "jundot/omlx", Slug: "jundot/omlx", Number: 7, Title: "behind", Author: "alytaphoenix",
		HeadRef: "feature", HeadSHA: "head", BaseRef: "main", BaseSHA: "base",
		Mergeable: "MERGEABLE", MergeState: "BEHIND", State: "OPEN",
	}
	r := &Reconciler{Cfg: cfg, Store: st, GH: fakeGitHub{prs: []github.PR{pr}}, Viewer: "alytaphoenix"}
	summary := r.Run(context.Background())
	if summary.NewJobs != 1 {
		t.Fatalf("expected one upstream merge, got %+v", summary)
	}
	jobsList, _ := st.ListJobs([]string{"pending"}, 10)
	if len(jobsList) != 1 || jobsList[0].Kind != "upstream-merge" {
		t.Fatalf("unexpected jobs: %+v", jobsList)
	}
}
