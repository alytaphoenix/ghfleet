// Package jobs defines the payloads exchanged between the server and runners.
package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

// Kind names.
const (
	KindReviewResponse = "review-response"
	KindCITriage       = "ci-triage"
	KindRebase         = "rebase"
	KindForkSync       = "fork-sync"
	KindIncomingReview = "incoming-review"
	KindSecurity       = "security-fix"
	KindUpstreamMerge  = "upstream-merge"
	KindPreflight      = "preflight"
)

// RepoSpec is the GitHub side of a repo, sent to runners.
type RepoSpec struct {
	Name          string `json:"name"`
	Upstream      string `json:"upstream"`
	Fork          string `json:"fork,omitempty"`
	Own           bool   `json:"own"`
	ReadOnly      bool   `json:"read_only,omitempty"`
	DefaultBranch string `json:"default_branch"`
	FetchRemote   string `json:"fetch_remote,omitempty"`
	PushRemote    string `json:"push_remote,omitempty"`
}

// PRSpec is the PR context a runner needs.
type PRSpec struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	URL            string `json:"url"`
	Author         string `json:"author"`
	HeadRef        string `json:"head_ref"`
	HeadSHA        string `json:"head_sha"`
	BaseRef        string `json:"base_ref"`
	BaseSHA        string `json:"base_sha"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"review_decision"`
}

// CommentSpec is one review comment.
type CommentSpec struct {
	ID         string    `json:"id"`
	DatabaseID int64     `json:"database_id,omitempty"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// ThreadSpec is a review thread.
type ThreadSpec struct {
	ID            string        `json:"id"`
	RootCommentID string        `json:"root_comment_id,omitempty"`
	LastCommentID string        `json:"last_comment_id,omitempty"`
	Path          string        `json:"path"`
	Line          int           `json:"line"`
	IsResolved    bool          `json:"is_resolved"`
	IsOutdated    bool          `json:"is_outdated"`
	Comments      []CommentSpec `json:"comments"`
}

// ReviewSpec is a review body.
type ReviewSpec struct {
	ID          string    `json:"id"`
	Author      string    `json:"author"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// CheckSpec is a failing check.
type CheckSpec struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
}

// Spec is the full job payload.
type Spec struct {
	JobID        string       `json:"job_id"`
	Kind         string       `json:"kind"`
	Repo         RepoSpec     `json:"repo"`
	PR           PRSpec       `json:"pr"`
	Threads      []ThreadSpec `json:"threads,omitempty"`
	Reviews      []ReviewSpec `json:"reviews,omitempty"`
	Checks       []CheckSpec  `json:"checks,omitempty"`
	HeadSHA      string       `json:"head_sha"`
	BaseSHA      string       `json:"base_sha"`
	Instructions string       `json:"instructions,omitempty"`
}

// Finding is one agent finding.
type Finding struct {
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity,omitempty"`
	Body     string `json:"body"`
}

// ThreadReply is a per-thread reply produced by the agent.
type ThreadReply struct {
	ThreadID string `json:"thread_id"`
	Body     string `json:"body"`
	Resolve  bool   `json:"resolve,omitempty"`
}

// Result is the agent's structured output plus runner metadata.
type Result struct {
	Decision       string        `json:"decision"`
	Summary        string        `json:"summary"`
	Risk           string        `json:"risk"`
	NeedsHuman     bool          `json:"needs_human"`
	Findings       []Finding     `json:"findings,omitempty"`
	Reply          string        `json:"reply,omitempty"`
	ThreadReplies  []ThreadReply `json:"thread_replies,omitempty"`
	Verification   []string      `json:"verification,omitempty"`
	CommitSHA      string        `json:"commit_sha,omitempty"`
	Applied        bool          `json:"applied,omitempty"`
	HandledThreads []string      `json:"handled_threads,omitempty"`
	ChecksOutput   string        `json:"checks_output,omitempty"`
	Error          string        `json:"error,omitempty"`
	PatchSHA256    string        `json:"patch_sha256,omitempty"`
	PatchSize      int64         `json:"patch_size,omitempty"`
	CostUSD        float64       `json:"cost_usd,omitempty"`
	Silent         bool          `json:"silent,omitempty"`
	Worktree       string        `json:"worktree,omitempty"`
}

// Marshal renders the result as JSON.
func (r Result) Marshal() string {
	b, _ := json.Marshal(r)
	return string(b)
}

// ParseResult decodes a result.
func ParseResult(raw string) Result {
	var r Result
	_ = json.Unmarshal([]byte(raw), &r)
	return r
}

// NewID builds a deterministic, readable job ID.
func NewID(kind, repo string, pr int, key string) string {
	sum := sha256.Sum256([]byte(key))
	h := hex.EncodeToString(sum[:])[:12]
	repo = strings.NewReplacer("/", "-", "_", "-").Replace(repo)
	return fmt.Sprintf("%s-%s-%d-%s", kind, repo, pr, h)
}

// IdempotencyKey builds a key for a job kind and state.
func IdempotencyKey(kind, repo string, pr int, headSHA string, extra string) string {
	return fmt.Sprintf("%s:%s:%d:%s:%s", kind, repo, pr, headSHA, extra)
}

// Threads converts GitHub threads to job specs.
func Threads(threads []github.Thread) []ThreadSpec {
	out := make([]ThreadSpec, 0, len(threads))
	for _, t := range threads {
		ts := ThreadSpec{ID: t.ID, RootCommentID: t.RootCommentID, Path: t.Path, Line: t.Line, IsResolved: t.IsResolved, IsOutdated: t.IsOutdated}
		if last, ok := t.LastComment(); ok {
			ts.LastCommentID = last.ID
		}
		for _, c := range t.Comments {
			ts.Comments = append(ts.Comments, CommentSpec{ID: c.ID, DatabaseID: c.DatabaseID, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt})
		}
		out = append(out, ts)
	}
	return out
}

// Reviews converts GitHub reviews to job specs.
func Reviews(reviews []github.Review) []ReviewSpec {
	out := make([]ReviewSpec, 0, len(reviews))
	for _, r := range reviews {
		out = append(out, ReviewSpec{ID: r.ID, Author: r.Author, Body: r.Body, State: r.State, SubmittedAt: r.SubmittedAt})
	}
	return out
}

// Checks converts GitHub checks to job specs.
func Checks(checks []github.Check) []CheckSpec {
	out := make([]CheckSpec, 0, len(checks))
	for _, c := range checks {
		out = append(out, CheckSpec{Name: c.Name, Status: c.Status, Conclusion: c.Conclusion, DetailsURL: c.DetailsURL})
	}
	return out
}

// Repo converts config to a spec.
func Repo(r config.Repo) RepoSpec {
	return RepoSpec{
		Name:          r.Name,
		Upstream:      r.Upstream,
		Fork:          r.Fork,
		Own:           r.Own,
		ReadOnly:      r.ReadOnly,
		DefaultBranch: r.DefaultBranch,
		FetchRemote:   r.FetchRemote,
		PushRemote:    r.PushRemote,
	}
}

// PR converts GitHub PR state to a spec.
func PR(pr github.PR) PRSpec {
	return PRSpec{
		Number:         pr.Number,
		Title:          pr.Title,
		Body:           pr.Body,
		URL:            pr.URL,
		Author:         pr.Author,
		HeadRef:        pr.HeadRef,
		HeadSHA:        pr.HeadSHA,
		BaseRef:        pr.BaseRef,
		BaseSHA:        pr.BaseSHA,
		Mergeable:      pr.Mergeable,
		ReviewDecision: pr.ReviewDecision,
	}
}

// NewJob assembles a store job.
func NewJob(kind string, repo config.Repo, pr github.PR, key string, spec Spec, labels []string) store.Job {
	spec.JobID = NewID(kind, repo.Name, pr.Number, key)
	spec.Kind = kind
	spec.Repo = Repo(repo)
	spec.PR = PR(pr)
	spec.HeadSHA = pr.HeadSHA
	spec.BaseSHA = pr.BaseSHA
	raw, _ := json.Marshal(spec)
	return store.Job{
		ID:             spec.JobID,
		Repo:           repo.Name,
		PRNumber:       pr.Number,
		Kind:           kind,
		HeadSHA:        pr.HeadSHA,
		BaseSHA:        pr.BaseSHA,
		Status:         "pending",
		SpecJSON:       string(raw),
		RequiredLabels: labels,
		IdempotencyKey: key,
		CreatedAt:      time.Now().UTC(),
	}
}

// ThreadFingerprint returns a stable fingerprint for a set of threads.
func ThreadFingerprint(threads []github.Thread) string {
	parts := make([]string, 0, len(threads))
	for _, t := range threads {
		last := ""
		if c, ok := t.LastComment(); ok {
			last = c.ID
		}
		parts = append(parts, t.ID+":"+last)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

// ReviewFingerprint returns a stable fingerprint for a set of reviews.
func ReviewFingerprint(reviews []github.Review) string {
	parts := make([]string, 0, len(reviews))
	for _, r := range reviews {
		parts = append(parts, r.ID+":"+r.State)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

func CheckFingerprint(checks []github.Check) string {
	parts := make([]string, 0, len(checks))
	for _, c := range checks {
		parts = append(parts, c.Name+":"+c.Conclusion)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(sum[:])[:12]
}

// RebaseFingerprint returns a stable key for a rebase target.
func RebaseFingerprint(baseSHA string) string {
	sum := sha256.Sum256([]byte(baseSHA))
	return hex.EncodeToString(sum[:])[:12]
}

// ForkSyncKey returns a time-bucketed key for fork sync.
func ForkSyncKey(now time.Time, interval time.Duration) string {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	seconds := int64(interval.Seconds())
	if seconds <= 0 {
		seconds = int64((24 * time.Hour).Seconds())
	}
	return strconv.FormatInt(now.UTC().Unix()/seconds, 10)
}

// FormatPR renders "owner/repo#123".
func FormatPR(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}
