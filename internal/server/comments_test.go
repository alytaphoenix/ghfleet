package server_test

import (
	"context"
	"log/slog"
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

type fakeComments struct {
	posts    []string
	replies  []string
	resolved []string
}

func (f *fakeComments) PostComment(ctx context.Context, slug string, number int, body string) error {
	f.posts = append(f.posts, body)
	return nil
}

func (f *fakeComments) ReplyToThread(ctx context.Context, slug string, number int, rootCommentID, body string) error {
	f.replies = append(f.replies, rootCommentID+":"+body)
	return nil
}

func (f *fakeComments) ResolveThread(ctx context.Context, threadID string) error {
	f.resolved = append(f.resolved, threadID)
	return nil
}

func TestAppliedPostsThreadRepliesAndResolvesBotThreads(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{
		Server: config.ServerConfig{Listen: "127.0.0.1:0", StateDir: dir},
		Approvals: config.Approvals{
			Push: "ask", Comment: "ask", Merge: "ask", ForkSync: "auto", ResolveThreads: "bot",
		},
		Limits: config.Limits{JobTimeout: config.Duration(5 * time.Minute), ApprovalTimeout: config.Duration(10 * time.Minute)},
	}
	cfg.ApplyDefaults()
	s := server.New(cfg, st, nil, "me", slog.Default())
	fake := &fakeComments{}
	s.Comments = fake
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	spec := `{"repo":{"name":"test","upstream":"up/test"},"pr":{"number":1},"threads":[
      {"id":"t1","root_comment_id":"101","path":"a.go","comments":[{"id":"c1","author":"reviewer","body":"fix"}]},
      {"id":"t2","root_comment_id":"102","path":"b.go","comments":[{"id":"c2","author":"dependabot[bot]","body":"bump"}]}
    ]}`
	if err := st.UpsertPR(github.PR{Repo: "test", Number: 1, HeadSHA: "abc", BaseSHA: "def"}); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: "job1", Repo: "test", PRNumber: 1, HeadSHA: "abc", Kind: jobs.KindReviewResponse, Status: "pending",
		SpecJSON: spec, IdempotencyKey: "k1", CreatedAt: time.Now(),
	}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.ClaimJob("job1", "r1", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatal(err)
	}
	result := jobs.Result{
		Decision: "fix", Summary: "fixed", Risk: "low",
		ThreadReplies: []jobs.ThreadReply{
			{ThreadID: "t1", Body: "fixed in abc", Resolve: true},
			{ThreadID: "t2", Body: "bumped", Resolve: true},
		},
	}
	if err := st.SaveResult("job1", result.Marshal(), "low", "fixed", 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Approve("job1"); err != nil {
		t.Fatal(err)
	}
	c := client.New(ts.URL, "")
	if err := c.Applied(context.Background(), "job1", server.RunnerResult{RunnerID: "r1", Result: result}); err != nil {
		t.Fatal(err)
	}
	if len(fake.replies) != 2 {
		t.Fatalf("expected 2 thread replies, got %v", fake.replies)
	}
	if len(fake.resolved) != 1 || fake.resolved[0] != "t2" {
		t.Fatalf("expected only bot thread t2 resolved, got %v", fake.resolved)
	}
	if len(fake.posts) != 0 {
		t.Fatalf("expected no summary when every thread was replied to, got %v", fake.posts)
	}
}

func TestAppliedPostsSummaryForReviewBodies(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	cfg := &config.Config{
		Server:    config.ServerConfig{Listen: "127.0.0.1:0", StateDir: dir},
		Approvals: config.Approvals{Push: "ask", Comment: "ask", Merge: "ask", ForkSync: "auto", ResolveThreads: "never"},
		Limits:    config.Limits{JobTimeout: config.Duration(5 * time.Minute), ApprovalTimeout: config.Duration(10 * time.Minute)},
	}
	cfg.ApplyDefaults()
	s := server.New(cfg, st, nil, "me", slog.Default())
	fake := &fakeComments{}
	s.Comments = fake
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	spec := `{"repo":{"name":"test","upstream":"up/test"},"pr":{"number":1},"reviews":[{"id":"r1","author":"reviewer","state":"CHANGES_REQUESTED","body":"please fix"}]}`
	if err := st.UpsertPR(github.PR{Repo: "test", Number: 1, HeadSHA: "abc", BaseSHA: "def"}); err != nil {
		t.Fatal(err)
	}
	job := store.Job{
		ID: "job2", Repo: "test", PRNumber: 1, HeadSHA: "abc", Kind: jobs.KindReviewResponse, Status: "pending",
		SpecJSON: spec, IdempotencyKey: "k2", CreatedAt: time.Now(),
	}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.ClaimJob("job2", "r1", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatal(err)
	}
	result := jobs.Result{Decision: "fix", Summary: "fixed review body", Risk: "low", Reply: "addressed the review"}
	if err := st.SaveResult("job2", result.Marshal(), "low", result.Summary, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Approve("job2"); err != nil {
		t.Fatal(err)
	}
	c := client.New(ts.URL, "")
	if err := c.Applied(context.Background(), "job2", server.RunnerResult{RunnerID: "r1", Result: result}); err != nil {
		t.Fatal(err)
	}
	if len(fake.posts) != 1 || !contains(fake.posts[0], "addressed the review") {
		t.Fatalf("expected one summary comment, got %v", fake.posts)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
