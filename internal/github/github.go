// Package github is a small GitHub GraphQL/REST client used by the control plane.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
)

const apiEndpoint = "https://api.github.com"

// Client talks to GitHub using a token from GH_TOKEN or `gh auth token`.
type Client struct {
	HTTP  *http.Client
	Token string
	Login string
}

// New creates a client and resolves the authenticated login.
func New(ctx context.Context) (*Client, error) {
	token, err := resolveToken(ctx)
	if err != nil {
		return nil, err
	}
	c := &Client{
		HTTP:  &http.Client{Timeout: 45 * time.Second},
		Token: token,
	}
	var out struct {
		Viewer struct {
			Login string `json:"login"`
		} `json:"viewer"`
	}
	if err := c.doGraphQL(ctx, `query { viewer { login } }`, nil, &out); err != nil {
		return nil, fmt.Errorf("resolve GitHub login: %w", err)
	}
	c.Login = out.Viewer.Login
	return c, nil
}

func resolveToken(ctx context.Context) (string, error) {
	for _, key := range []string{"GHFLEET_GH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v, nil
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("no GitHub token: set GH_TOKEN or run `gh auth login` (%s)", strings.TrimSpace(stderr.String()))
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("`gh auth token` returned an empty token")
	}
	return token, nil
}

// PR is the subset of pull request state ghfleet needs.
type PR struct {
	Repo           string    `json:"repo"`
	Slug           string    `json:"slug"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	URL            string    `json:"url"`
	Author         string    `json:"author"`
	State          string    `json:"state"`
	IsDraft        bool      `json:"is_draft"`
	HeadRef        string    `json:"head_ref"`
	HeadSHA        string    `json:"head_sha"`
	BaseRef        string    `json:"base_ref"`
	BaseSHA        string    `json:"base_sha"`
	Mergeable      string    `json:"mergeable"`
	MergeState     string    `json:"merge_state"`
	ReviewDecision string    `json:"review_decision"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changed_files"`
	CommentsCount  int       `json:"comments_count"`
	CheckState     string    `json:"check_state"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Checks         []Check   `json:"checks"`
	Threads        []Thread  `json:"threads"`
	Reviews        []Review  `json:"reviews"`
}

// Check is one CI check or status context.
type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
}

// Thread is a pull request review thread.
type Thread struct {
	ID            string    `json:"id"`
	RootCommentID string    `json:"root_comment_id,omitempty"`
	Path          string    `json:"path"`
	Line          int       `json:"line"`
	IsResolved    bool      `json:"is_resolved"`
	IsOutdated    bool      `json:"is_outdated"`
	Comments      []Comment `json:"comments"`
}

// Review is a pull request review (the body, not an inline thread).
type Review struct {
	ID          string    `json:"id"`
	Author      string    `json:"author"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// Comment is one review comment.
type Comment struct {
	ID         string    `json:"id"`
	DatabaseID int64     `json:"database_id,omitempty"`
	Author     string    `json:"author"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"created_at"`
}

// LastComment returns the newest comment in the thread.
func (t Thread) LastComment() (Comment, bool) {
	if len(t.Comments) == 0 {
		return Comment{}, false
	}
	return t.Comments[len(t.Comments)-1], true
}

// NeedsAttention reports whether the thread has an unresolved human comment.
func (t Thread) NeedsAttention(viewer string) bool {
	if t.IsResolved || t.IsOutdated {
		return false
	}
	last, ok := t.LastComment()
	if !ok {
		return false
	}
	if last.Author == viewer || strings.HasSuffix(last.Author, "[bot]") || last.Author == "github-actions" {
		return false
	}
	return true
}

type gqlResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"errors"`
}

func (c *Client) doGraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiEndpoint+"/graphql", bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("github graphql: %s: %s", resp.Status, truncate(string(body), 300))
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("github graphql: %s: %s", resp.Status, truncate(string(body), 500))
		}
		var envelope gqlResponse
		if err := json.Unmarshal(body, &envelope); err != nil {
			return fmt.Errorf("decode graphql response: %w", err)
		}
		if len(envelope.Errors) > 0 {
			msgs := make([]string, 0, len(envelope.Errors))
			for _, e := range envelope.Errors {
				msgs = append(msgs, e.Message)
			}
			return fmt.Errorf("github graphql: %s", strings.Join(msgs, "; "))
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode graphql data: %w", err)
		}
		return nil
	}
	return fmt.Errorf("github graphql failed after retries: %w", lastErr)
}

const prsQuery = `
query($owner: String!, $name: String!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequests(first: 50, states: OPEN, after: $after, orderBy: {field: UPDATED_AT, direction: DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number title body url isDraft state
        headRefName headRefOid baseRefName baseRefOid
        mergeable mergeStateStatus reviewDecision
        createdAt updatedAt additions deletions changedFiles totalCommentsCount
        author { login }
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                state
                contexts(first: 50) {
                  nodes {
                    __typename
                    ... on CheckRun { name status conclusion detailsUrl }
                    ... on StatusContext { context state targetUrl }
                  }
                }
              }
            }
          }
        }
        reviewThreads(first: 50) {
          nodes {
            id isResolved isOutdated path line
            firstComment: comments(first: 1) { nodes { id databaseId } }
            comments(last: 20) {
              nodes { id databaseId body createdAt url author { login } }
            }
          }
        }
      }
    }
  }
}`

type gqlPR struct {
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	URL            string    `json:"url"`
	IsDraft        bool      `json:"isDraft"`
	HeadRefName    string    `json:"headRefName"`
	HeadRefOid     string    `json:"headRefOid"`
	BaseRefName    string    `json:"baseRefName"`
	BaseRefOid     string    `json:"baseRefOid"`
	Mergeable      string    `json:"mergeable"`
	MergeState     string    `json:"mergeStateStatus"`
	ReviewDecision string    `json:"reviewDecision"`
	State          string    `json:"state"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changedFiles"`
	CommentsCount  int       `json:"totalCommentsCount"`
	Author         struct {
		Login string `json:"login"`
	} `json:"author"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				Oid               string     `json:"oid"`
				StatusCheckRollup *gqlRollup `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	ReviewThreads struct {
		Nodes []gqlThread `json:"nodes"`
	} `json:"reviewThreads"`
	Reviews struct {
		Nodes []gqlReview `json:"nodes"`
	} `json:"reviews"`
}

type gqlThread struct {
	ID           string `json:"id"`
	FirstComment struct {
		Nodes []struct {
			ID         string `json:"id"`
			DatabaseID int64  `json:"databaseId"`
		} `json:"nodes"`
	} `json:"firstComment"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       *int   `json:"line"`
	Comments   struct {
		Nodes []gqlComment `json:"nodes"`
	} `json:"comments"`
}

type gqlReview struct {
	ID          string    `json:"id"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submittedAt"`
	Author      struct {
		Login string `json:"login"`
	} `json:"author"`
}

type gqlComment struct {
	ID         string    `json:"id"`
	DatabaseID int64     `json:"databaseId"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	URL        string    `json:"url"`
	Author     struct {
		Login string `json:"login"`
	} `json:"author"`
}

type gqlRollup struct {
	State    string `json:"state"`
	Contexts struct {
		Nodes []struct {
			Typename   string `json:"__typename"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			DetailsURL string `json:"detailsUrl"`
			Context    string `json:"context"`
			State      string `json:"state"`
			TargetURL  string `json:"targetUrl"`
		} `json:"nodes"`
	} `json:"contexts"`
}

// PRs returns open PRs authored by viewer (or all PRs when viewer is empty).
// It follows pagination for up to 10 pages.
func (c *Client) PRs(ctx context.Context, slug, viewer string) ([]PR, error) {
	owner, name, err := config.SplitSlug(slug)
	if err != nil {
		return nil, err
	}
	var all []PR
	var after *string
	for page := 0; page < 10; page++ {
		vars := map[string]any{"owner": owner, "name": name}
		if after != nil {
			vars["after"] = *after
		}
		var resp struct {
			Repository struct {
				PullRequests struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []gqlPR `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		if err := c.doGraphQL(ctx, prsQuery, vars, &resp); err != nil {
			return nil, fmt.Errorf("%s: %w", slug, err)
		}
		for _, n := range resp.Repository.PullRequests.Nodes {
			if viewer != "" && n.Author.Login != viewer {
				continue
			}
			all = append(all, convertPR(slug, n))
		}
		if !resp.Repository.PullRequests.PageInfo.HasNextPage {
			break
		}
		cursor := resp.Repository.PullRequests.PageInfo.EndCursor
		after = &cursor
	}
	return all, nil
}

func convertPR(slug string, n gqlPR) PR {
	if n.Repository.NameWithOwner != "" {
		slug = n.Repository.NameWithOwner
	}
	pr := PR{
		Repo:           slug,
		Slug:           slug,
		Number:         n.Number,
		Title:          n.Title,
		Body:           n.Body,
		URL:            n.URL,
		Author:         n.Author.Login,
		IsDraft:        n.IsDraft,
		HeadRef:        n.HeadRefName,
		HeadSHA:        n.HeadRefOid,
		BaseRef:        n.BaseRefName,
		BaseSHA:        n.BaseRefOid,
		Mergeable:      n.Mergeable,
		MergeState:     n.MergeState,
		ReviewDecision: n.ReviewDecision,
		State:          n.State,
		Additions:      n.Additions,
		Deletions:      n.Deletions,
		ChangedFiles:   n.ChangedFiles,
		CommentsCount:  n.CommentsCount,
		CreatedAt:      n.CreatedAt,
		UpdatedAt:      n.UpdatedAt,
	}
	if len(n.Commits.Nodes) > 0 {
		rollup := n.Commits.Nodes[len(n.Commits.Nodes)-1].Commit.StatusCheckRollup
		pr.CheckState, pr.Checks = convertRollup(rollup)
	}
	for _, t := range n.ReviewThreads.Nodes {
		pr.Threads = append(pr.Threads, convertThread(t))
	}
	for _, r := range n.Reviews.Nodes {
		pr.Reviews = append(pr.Reviews, Review{
			ID: r.ID, Author: r.Author.Login, Body: r.Body, State: r.State, SubmittedAt: r.SubmittedAt,
		})
	}
	return pr
}

func convertThread(t gqlThread) Thread {
	th := Thread{
		ID:         t.ID,
		Path:       t.Path,
		IsResolved: t.IsResolved,
		IsOutdated: t.IsOutdated,
	}
	if len(t.FirstComment.Nodes) > 0 && t.FirstComment.Nodes[0].DatabaseID > 0 {
		th.RootCommentID = fmt.Sprintf("%d", t.FirstComment.Nodes[0].DatabaseID)
	}
	if t.Line != nil {
		th.Line = *t.Line
	}
	for _, cm := range t.Comments.Nodes {
		th.Comments = append(th.Comments, Comment{
			ID:         cm.ID,
			DatabaseID: cm.DatabaseID,
			Author:     cm.Author.Login,
			Body:       cm.Body,
			URL:        cm.URL,
			CreatedAt:  cm.CreatedAt,
		})
	}
	return th
}

func convertRollup(r *gqlRollup) (string, []Check) {
	if r == nil {
		return "", nil
	}
	var checks []Check
	failure, pending := false, false
	for _, n := range r.Contexts.Nodes {
		ch := Check{Name: n.Name, Status: n.Status, Conclusion: n.Conclusion, DetailsURL: n.DetailsURL}
		if n.Typename == "StatusContext" {
			ch.Name = n.Context
			ch.Status = n.State
			ch.Conclusion = n.State
			ch.DetailsURL = n.TargetURL
		}
		checks = append(checks, ch)
		switch strings.ToUpper(ch.Conclusion) {
		case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
			failure = true
		case "PENDING", "QUEUED", "IN_PROGRESS", "WAITING", "REQUESTED", "":
			if !strings.EqualFold(ch.Status, "COMPLETED") || ch.Conclusion == "" {
				pending = true
			}
		}
	}
	switch {
	case failure:
		return "FAILURE", checks
	case pending:
		return "PENDING", checks
	case strings.EqualFold(r.State, "SUCCESS"):
		return "SUCCESS", checks
	case r.State != "":
		return strings.ToUpper(r.State), checks
	default:
		return "", checks
	}
}

// FailingChecks returns checks with a failure conclusion.
func FailingChecks(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		switch strings.ToUpper(c.Conclusion) {
		case "FAILURE", "ERROR", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE":
			out = append(out, c)
		}
	}
	return out
}

// PostComment posts an issue/PR comment.
func (c *Client) PostComment(ctx context.Context, slug string, number int, body string) error {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", slug, number)
	payload, _ := json.Marshal(map[string]string{"body": body})
	return c.doREST(ctx, http.MethodPost, path, payload, nil)
}

// ReplyToThread replies to a review thread by its root comment ID.
func (c *Client) ReplyToThread(ctx context.Context, slug string, number int, rootCommentID, body string) error {
	if _, err := strconv.ParseInt(rootCommentID, 10, 64); err != nil {
		return fmt.Errorf("review thread root comment must be a numeric database ID")
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/comments/%s/replies", slug, number, rootCommentID)
	payload, _ := json.Marshal(map[string]string{"body": body})
	return c.doREST(ctx, http.MethodPost, path, payload, nil)
}

// ResolveThread resolves a review thread.
func (c *Client) ResolveThread(ctx context.Context, threadID string) error {
	var out map[string]any
	return c.doGraphQL(ctx, `
mutation($id: ID!) {
  resolveReviewThread(input: {threadId: $id}) { thread { id isResolved } }
}`, map[string]any{"id": threadID}, &out)
}

func (c *Client) doREST(ctx context.Context, method, path string, payload []byte, out any) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiEndpoint+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github api %s %s: %s: %s", method, path, resp.Status, truncate(string(respBody), 500))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
