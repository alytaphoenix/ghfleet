package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
)

func TestNewIDStableAndReadable(t *testing.T) {
	a := NewID(KindReviewResponse, "jundot/omlx", 3141, "key")
	b := NewID(KindReviewResponse, "jundot/omlx", 3141, "key")
	if a != b {
		t.Fatalf("ids differ: %s %s", a, b)
	}
	if !strings.HasPrefix(a, "review-response-jundot-omlx-3141-") {
		t.Fatalf("unexpected id: %s", a)
	}
	c := NewID(KindReviewResponse, "jundot/omlx", 3141, "other")
	if a == c {
		t.Fatal("different keys should produce different ids")
	}
}

func TestThreadFingerprint(t *testing.T) {
	threads := []github.Thread{
		{ID: "b", Comments: []github.Comment{{ID: "2"}}},
		{ID: "a", Comments: []github.Comment{{ID: "1"}}},
	}
	fp1 := ThreadFingerprint(threads)
	threads[0].Comments = append(threads[0].Comments, github.Comment{ID: "3"})
	fp2 := ThreadFingerprint(threads)
	if fp1 == fp2 {
		t.Fatal("fingerprint should change when a thread gets a new comment")
	}
	// Order should not matter.
	swapped := []github.Thread{threads[1], threads[0]}
	if ThreadFingerprint(swapped) != fp2 {
		t.Fatal("fingerprint should be order-independent")
	}
}

func TestNewJobSpec(t *testing.T) {
	repo := config.Repo{
		Name: "omlx", Upstream: "jundot/omlx", Fork: "alytaphoenix/omlx",
		DefaultBranch: "main", FetchRemote: "origin", PushRemote: "fork",
	}
	pr := github.PR{Number: 3141, Title: "t", HeadRef: "h", HeadSHA: "abc", BaseRef: "main", BaseSHA: "def"}
	spec := Spec{Instructions: "go"}
	job := NewJob(KindRebase, repo, pr, "key", spec, []string{"darwin"})
	if job.Repo != "omlx" || job.PRNumber != 3141 || job.Kind != KindRebase {
		t.Fatalf("unexpected job: %+v", job)
	}
	if !strings.Contains(job.SpecJSON, `"upstream":"jundot/omlx"`) || !strings.Contains(job.SpecJSON, `"head_sha":"abc"`) {
		t.Fatalf("spec missing fields: %s", job.SpecJSON)
	}
	if job.CreatedAt.IsZero() || job.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("unexpected created time: %v", job.CreatedAt)
	}
}

func TestForkSyncKeyUsesInterval(t *testing.T) {
	t0 := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	if ForkSyncKey(t0, 24*time.Hour) != ForkSyncKey(t0.Add(time.Hour), 24*time.Hour) {
		t.Fatal("same 24h bucket should share a key")
	}
	if ForkSyncKey(t0, 24*time.Hour) == ForkSyncKey(t0.Add(25*time.Hour), 24*time.Hour) {
		t.Fatal("different 24h buckets should differ")
	}
	if ForkSyncKey(t0, time.Hour) == ForkSyncKey(t0.Add(2*time.Hour), time.Hour) {
		t.Fatal("hourly buckets should differ")
	}
}
