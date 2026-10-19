package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/alytaphoenix/ghfleet/internal/config"
)

const searchBasicQuery = `
query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on PullRequest {
        repository { nameWithOwner }
        number title url isDraft state
        headRefName headRefOid baseRefName baseRefOid
        mergeable mergeStateStatus reviewDecision
        createdAt updatedAt additions deletions changedFiles totalCommentsCount
        author { login }
      }
    }
  }
}`

// AuthoredPRs finds open PRs authored by viewer in the configured repos.
// It combines the search index (good for older PRs) with a small per-repo
// "recently updated" query (good for PRs the search index has not caught up
// with), then fetches bodies, checks, and review threads in one batched query.
func (c *Client) AuthoredPRs(ctx context.Context, slugs []string, viewer string) ([]PR, error) {
	if viewer == "" {
		return nil, fmt.Errorf("viewer login is required")
	}
	if len(slugs) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, slug := range slugs {
		allowed[slug] = true
	}

	merged := map[string]PR{}
	var errs []string
	if prs, err := c.searchAuthored(ctx, viewer); err == nil {
		for _, pr := range prs {
			if allowed[pr.Slug] {
				merged[fmt.Sprintf("%s#%d", pr.Slug, pr.Number)] = pr
			}
		}
	} else {
		errs = append(errs, "search: "+err.Error())
	}
	if prs, err := c.recentAuthored(ctx, slugs, viewer); err == nil {
		for _, pr := range prs {
			merged[fmt.Sprintf("%s#%d", pr.Slug, pr.Number)] = pr
		}
	} else {
		errs = append(errs, "recent: "+err.Error())
	}
	prs := make([]PR, 0, len(merged))
	for _, pr := range merged {
		if pr.State != "" && !strings.EqualFold(pr.State, "OPEN") {
			continue
		}
		prs = append(prs, pr)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].UpdatedAt.After(prs[j].UpdatedAt) })
	usedFallback := false
	if len(prs) == 0 && len(errs) > 0 {
		// Last resort: one query per repo. Slower, but survives a bad batch.
		usedFallback = true
		for _, slug := range slugs {
			if p, err := c.PRs(ctx, slug, viewer); err == nil {
				prs = append(prs, p...)
			}
		}
	}
	if len(errs) > 0 {
		if len(prs) == 0 {
			return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		if usedFallback {
			return prs, fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		prs, enrichErr := c.enrich(ctx, prs)
		if enrichErr != nil {
			return prs, fmt.Errorf("%s; %v", strings.Join(errs, "; "), enrichErr)
		}
		return prs, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return c.enrich(ctx, prs)
}

func (c *Client) searchAuthored(ctx context.Context, viewer string) ([]PR, error) {
	q := fmt.Sprintf("author:%s is:pr is:open sort:updated-desc", viewer)
	var all []PR
	seen := map[string]bool{}
	var after *string
	for page := 0; page < 3; page++ {
		vars := map[string]any{"q": q}
		if after != nil {
			vars["after"] = *after
		}
		var resp struct {
			Search struct {
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []gqlPR `json:"nodes"`
			} `json:"search"`
		}
		if err := c.doGraphQL(ctx, searchBasicQuery, vars, &resp); err != nil {
			return nil, err
		}
		for _, n := range resp.Search.Nodes {
			if n.Number == 0 || n.Repository.NameWithOwner == "" {
				continue
			}
			key := fmt.Sprintf("%s#%d", n.Repository.NameWithOwner, n.Number)
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, convertPR(n.Repository.NameWithOwner, n))
		}
		if !resp.Search.PageInfo.HasNextPage {
			break
		}
		cursor := resp.Search.PageInfo.EndCursor
		after = &cursor
	}
	return all, nil
}

func (c *Client) recentAuthored(ctx context.Context, slugs []string, viewer string) ([]PR, error) {
	var out []PR
	var errs []string
	for start := 0; start < len(slugs); start += 5 {
		end := start + 5
		if end > len(slugs) {
			end = len(slugs)
		}
		query, aliases := basicQuery(slugs[start:end])
		var data map[string]struct {
			PullRequests struct {
				Nodes []gqlPR `json:"nodes"`
			} `json:"pullRequests"`
		}
		if err := c.doGraphQL(ctx, query, nil, &data); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for alias, slug := range aliases {
			for _, n := range data[alias].PullRequests.Nodes {
				if n.Author.Login != viewer {
					continue
				}
				out = append(out, convertPR(slug, n))
			}
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

func basicQuery(slugs []string) (string, map[string]string) {
	var b strings.Builder
	b.WriteString("query {\n")
	aliases := map[string]string{}
	for i, slug := range slugs {
		owner, name, err := config.SplitSlug(slug)
		if err != nil {
			continue
		}
		alias := fmt.Sprintf("r%d", i)
		aliases[alias] = slug
		fmt.Fprintf(&b, "  %s: repository(owner: %s, name: %s) {\n", alias, strconv.Quote(owner), strconv.Quote(name))
		b.WriteString(`    pullRequests(first: 20, states: OPEN, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number title url isDraft state
        headRefName headRefOid baseRefName baseRefOid
        mergeable mergeStateStatus reviewDecision
        createdAt updatedAt additions deletions changedFiles totalCommentsCount
        author { login }
      }
    }
`)
		b.WriteString("  }\n")
	}
	b.WriteString("}")
	return b.String(), aliases
}

func detailQuery(prs []PR) (string, map[int]string) {
	var b strings.Builder
	b.WriteString("query {\n")
	aliases := map[int]string{}
	for i, pr := range prs {
		owner, name, err := config.SplitSlug(pr.Slug)
		if err != nil {
			continue
		}
		alias := fmt.Sprintf("p%d", i)
		aliases[i] = alias
		fmt.Fprintf(&b, "  %s: repository(owner: %s, name: %s) {\n", alias, strconv.Quote(owner), strconv.Quote(name))
		fmt.Fprintf(&b, `    pullRequest(number: %d) {
      body
      reviews(last: 20) {
        nodes { id body state submittedAt author { login } }
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
    }
`, pr.Number)
		b.WriteString("  }\n")
	}
	b.WriteString("}")
	return b.String(), aliases
}

func (c *Client) enrich(ctx context.Context, prs []PR) ([]PR, error) {
	var lastErr error
	for start := 0; start < len(prs); start += 8 {
		end := start + 8
		if end > len(prs) {
			end = len(prs)
		}
		chunk := prs[start:end]
		dq, dalias := detailQuery(chunk)
		var details map[string]struct {
			PullRequest *struct {
				Body          string `json:"body"`
				ReviewThreads struct {
					Nodes []gqlThread `json:"nodes"`
				} `json:"reviewThreads"`
				Reviews struct {
					Nodes []gqlReview `json:"nodes"`
				} `json:"reviews"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							Oid               string     `json:"oid"`
							StatusCheckRollup *gqlRollup `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		}
		if err := c.doGraphQL(ctx, dq, nil, &details); err != nil {
			lastErr = err
			continue
		}
		for j, alias := range dalias {
			d, ok := details[alias]
			if !ok || d.PullRequest == nil {
				continue
			}
			idx := start + j
			prs[idx].Body = d.PullRequest.Body
			if len(d.PullRequest.Commits.Nodes) > 0 {
				rollup := d.PullRequest.Commits.Nodes[len(d.PullRequest.Commits.Nodes)-1].Commit.StatusCheckRollup
				prs[idx].CheckState, prs[idx].Checks = convertRollup(rollup)
			}
			for _, t := range d.PullRequest.ReviewThreads.Nodes {
				prs[idx].Threads = append(prs[idx].Threads, convertThread(t))
			}
			for _, r := range d.PullRequest.Reviews.Nodes {
				prs[idx].Reviews = append(prs[idx].Reviews, Review{
					ID: r.ID, Author: r.Author.Login, Body: r.Body, State: r.State, SubmittedAt: r.SubmittedAt,
				})
			}
		}
	}
	if lastErr != nil {
		return prs, fmt.Errorf("review threads: %w", lastErr)
	}
	return prs, nil
}

// IncomingPRs returns recent open PRs in the given repos that were authored by
// someone other than viewer.
func (c *Client) IncomingPRs(ctx context.Context, slugs []string, viewer string) ([]PR, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	var prs []PR
	var errs []string
	for start := 0; start < len(slugs); start += 5 {
		end := start + 5
		if end > len(slugs) {
			end = len(slugs)
		}
		query, aliases := basicQuery(slugs[start:end])
		var data map[string]struct {
			PullRequests struct {
				Nodes []gqlPR `json:"nodes"`
			} `json:"pullRequests"`
		}
		if err := c.doGraphQL(ctx, query, nil, &data); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		for alias, slug := range aliases {
			for _, n := range data[alias].PullRequests.Nodes {
				if n.Author.Login == viewer {
					continue
				}
				if n.State != "" && !strings.EqualFold(n.State, "OPEN") {
					continue
				}
				if n.IsDraft {
					continue
				}
				prs = append(prs, convertPR(slug, n))
			}
		}
	}
	if len(prs) == 0 {
		if len(errs) > 0 {
			return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
		}
		return nil, nil
	}
	if len(errs) > 0 {
		enriched, err := c.enrich(ctx, prs)
		if err != nil {
			return enriched, fmt.Errorf("%s; %v", strings.Join(errs, "; "), err)
		}
		return enriched, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].UpdatedAt.After(prs[j].UpdatedAt) })
	enriched, err := c.enrich(ctx, prs)
	if err != nil {
		return enriched, err
	}
	return enriched, nil
}
