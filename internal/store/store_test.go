package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/github"
)

func testPR() github.PR {
	now := time.Now().UTC()
	return github.PR{
		Repo: "omlx", Slug: "jundot/omlx", Number: 3141, Title: "test", Body: "body",
		URL: "https://github.com/jundot/omlx/pull/3141", Author: "alytaphoenix",
		HeadRef: "feature", HeadSHA: "abc123", BaseRef: "main", BaseSHA: "def456",
		Mergeable: "CONFLICTING", ReviewDecision: "CHANGES_REQUESTED", CheckState: "SUCCESS",
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Checks:  []github.Check{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}},
		Reviews: []github.Review{{ID: "r1", Author: "reviewer", State: "CHANGES_REQUESTED", Body: "please fix", SubmittedAt: now}},
		Threads: []github.Thread{
			{
				ID: "t1", Path: "a.go", Line: 4,
				Comments: []github.Comment{{ID: "c1", Author: "reviewer", Body: "please fix", CreatedAt: now}},
			},
			{
				ID: "t2", Path: "b.go", Line: 9,
				Comments: []github.Comment{{ID: "c2", Author: "alytaphoenix", Body: "mine", CreatedAt: now}},
			},
		},
	}
}

func TestStoreLifecycle(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	pr := testPR()
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetPR("omlx", 3141)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "test" || len(got.Threads) != 2 || len(got.Checks) != 1 {
		t.Fatalf("unexpected PR: %+v", got)
	}

	threads, err := st.UnresolvedThreads("omlx", 3141, "alytaphoenix")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || threads[0].ID != "t1" {
		t.Fatalf("expected only t1, got %+v", threads)
	}
	reviews, err := st.UnhandledReviews("omlx", 3141, "alytaphoenix")
	if err != nil || len(reviews) != 1 || reviews[0].ID != "r1" {
		t.Fatalf("expected one unhandled review, got %+v %v", reviews, err)
	}
	if err := st.MarkReviewsHandled([]string{"r1"}); err != nil {
		t.Fatal(err)
	}
	reviews, _ = st.UnhandledReviews("omlx", 3141, "alytaphoenix")
	if len(reviews) != 0 {
		t.Fatalf("expected no unhandled reviews, got %+v", reviews)
	}

	job := Job{
		ID: "job1", Repo: "omlx", PRNumber: 3141, Kind: "review-response", Status: "pending",
		SpecJSON: "{}", IdempotencyKey: "key1", RequiredLabels: []string{"darwin"},
	}
	ok, err := st.Enqueue(job)
	if err != nil || !ok {
		t.Fatalf("enqueue: %v %v", ok, err)
	}
	ok, err = st.Enqueue(job)
	if err != nil || ok {
		t.Fatalf("duplicate enqueue should be ignored: %v %v", ok, err)
	}
	if active, _ := st.HasActiveJob("omlx", 3141, "review-response"); !active {
		t.Fatal("expected active job")
	}

	ok, err = st.ClaimJob("job1", "runner-1", time.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := st.SaveResult("job1", `{"summary":"fixed","risk":"low"}`, "low", "fixed", 0.25, time.Time{}); err != nil {
		t.Fatal(err)
	}
	j, err := st.GetJob("job1")
	if err != nil || j.Status != "awaiting_approval" {
		t.Fatalf("job status: %+v %v", j, err)
	}
	if err := st.Approve("job1"); err != nil {
		t.Fatal(err)
	}
	instr, err := st.InstructionFor("job1", "runner-1")
	if err != nil || instr.Action != "apply" {
		t.Fatalf("instruction: %+v %v", instr, err)
	}
	if err := st.MarkApplied("job1", `{"applied":true}`); err != nil {
		t.Fatal(err)
	}
	if active, _ := st.HasActiveJob("omlx", 3141, "review-response"); active {
		t.Fatal("applied job should not be active")
	}

	// A handled thread should disappear from the unresolved set.
	if err := st.MarkThreadHandled("t1"); err != nil {
		t.Fatal(err)
	}
	threads, _ = st.UnresolvedThreads("omlx", 3141, "alytaphoenix")
	if len(threads) != 0 {
		t.Fatalf("expected no unresolved threads, got %+v", threads)
	}

	// Pruning removes PRs that are no longer open after a grace period.
	if _, err := st.DB.Exec(`UPDATE prs SET last_seen_at=? WHERE repo=? AND number=?`,
		time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339), "omlx", 3141); err != nil {
		t.Fatal(err)
	}
	if err := st.PrunePRs(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	prs, err := st.ListPRs()
	if err != nil || len(prs) != 0 {
		t.Fatalf("expected empty PR list, got %d %v", len(prs), err)
	}
}

func TestReleaseKeyAllowsRetry(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	job := Job{ID: "job1", Repo: "r", PRNumber: 1, Kind: "ci-triage", Status: "pending", SpecJSON: "{}", IdempotencyKey: "k"}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.SetJobStatus("job1", "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseKey("job1"); err != nil {
		t.Fatal(err)
	}
	job.ID = "job2"
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("retry should insert: %v %v", ok, err)
	}
}

func TestDenyApprovedJob(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	j := Job{ID: "j", Repo: "r", PRNumber: 1, Kind: "review-response", Status: "pending", SpecJSON: "{}", IdempotencyKey: "k"}
	if ok, err := st.Enqueue(j); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.ClaimJob("j", "r1", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.SaveResult("j", "{}", "low", "done", 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := st.Approve("j"); err != nil {
		t.Fatal(err)
	}
	if err := st.Deny("j"); err != nil {
		t.Fatalf("denying an approved job should work: %v", err)
	}
	got, _ := st.GetJob("j")
	if got.Status != "denied" {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestMarkThreadHandledAtDoesNotSwallowNewComment(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	pr := testPR()
	pr.Threads = pr.Threads[:1]
	pr.Threads[0].Comments = []github.Comment{{ID: "c1", Author: "reviewer", Body: "one"}}
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkThreadHandledAt("t1", "c1"); err != nil {
		t.Fatal(err)
	}
	threads, _ := st.UnresolvedThreads("omlx", 3141, "alytaphoenix")
	if len(threads) != 0 {
		t.Fatalf("expected handled thread, got %+v", threads)
	}
	pr.Threads[0].Comments = append(pr.Threads[0].Comments, github.Comment{ID: "c2", Author: "reviewer", Body: "two"})
	if err := st.UpsertPR(pr); err != nil {
		t.Fatal(err)
	}
	threads, _ = st.UnresolvedThreads("omlx", 3141, "alytaphoenix")
	last, _ := threads[0].LastComment()
	if len(threads) != 1 || last.ID != "c2" {
		t.Fatalf("new comment should be unhandled, got %+v", threads)
	}
}

func TestAwaitingApprovalLeasePreventsConcurrentApply(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	j := Job{ID: "j", Repo: "r", PRNumber: 1, Kind: "review-response", Status: "pending", SpecJSON: "{}", IdempotencyKey: "k"}
	if ok, err := st.Enqueue(j); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.ClaimJob("j", "r1", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.SaveResult("j", "{}", "low", "done", 0, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := st.Approve("j"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ReserveApproved("j", "r2", time.Now().Add(time.Hour)); err != nil || ok {
		t.Fatalf("another worker must not reserve a leased approved job: ok=%v err=%v", ok, err)
	}
	// Once the lease expires, it can be taken over.
	if _, err := st.DB.Exec(`UPDATE jobs SET lease_until=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "j"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ReserveApproved("j", "r2", time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("expired lease should be reservable: ok=%v err=%v", ok, err)
	}
}

func TestRetryWithSameIDAfterRelease(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ghfleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	job := Job{ID: "same", Repo: "r", PRNumber: 1, Kind: "ci-triage", Status: "pending", SpecJSON: "{}", IdempotencyKey: "k"}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatal(err)
	}
	if err := st.SetJobStatus("same", "failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseKey("same"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.Enqueue(job); err != nil || !ok {
		t.Fatalf("same-ID retry should insert a new row: ok=%v err=%v", ok, err)
	}
	jobs, _ := st.ListJobs([]string{"pending"}, 10)
	if len(jobs) != 1 || jobs[0].ID == "same" {
		t.Fatalf("expected a retry job with a new ID, got %+v", jobs)
	}
}
