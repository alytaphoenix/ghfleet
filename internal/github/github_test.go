package github

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConvertPR(t *testing.T) {
	var n gqlPR
	raw := `{
      "number": 42,
      "title": "title",
      "body": "body",
      "headRefName": "feature",
      "headRefOid": "abc",
      "baseRefName": "main",
      "baseRefOid": "def",
      "mergeable": "CONFLICTING",
      "reviewDecision": "CHANGES_REQUESTED",
      "author": {"login": "alytaphoenix"},
      "updatedAt": "2026-10-19T00:00:00Z",
      "commits": {"nodes": [{"commit": {"oid": "abc", "statusCheckRollup": {"state": "FAILURE", "contexts": {"nodes": [
        {"__typename": "CheckRun", "name": "ci", "status": "COMPLETED", "conclusion": "FAILURE"}
      ]}}}}]},
      "reviewThreads": {"nodes": [{"id": "t1", "path": "a.go", "line": 7, "comments": {"nodes": [
        {"id": "c1", "body": "fix", "author": {"login": "reviewer"}}
      ]}}]}
    }`
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatal(err)
	}
	pr := convertPR("jundot/omlx", n)
	if pr.Number != 42 || pr.Slug != "jundot/omlx" || pr.CheckState != "FAILURE" {
		t.Fatalf("unexpected PR: %+v", pr)
	}
	if len(pr.Threads) != 1 || pr.Threads[0].Line != 7 || pr.Threads[0].Comments[0].Author != "reviewer" {
		t.Fatalf("unexpected threads: %+v", pr.Threads)
	}
}

func TestConvertThreadUsesNumericRootCommentID(t *testing.T) {
	raw := `{
      "id": "PRRT_node",
      "isResolved": false,
      "path": "a.go",
      "line": 3,
      "firstComment": {"nodes": [{"id": "PRRC_root", "databaseId": 9001}]},
      "comments": {"nodes": [
        {"id": "PRRC_latest", "databaseId": 9002, "body": "latest", "author": {"login": "reviewer"}}
      ]}
    }`
	var tdata gqlThread
	if err := json.Unmarshal([]byte(raw), &tdata); err != nil {
		t.Fatal(err)
	}
	th := convertThread(tdata)
	if th.RootCommentID != "9001" {
		t.Fatalf("root comment ID = %q, want numeric database ID", th.RootCommentID)
	}
	if len(th.Comments) != 1 || th.Comments[0].DatabaseID != 9002 {
		t.Fatalf("unexpected comments: %+v", th.Comments)
	}
}

func TestConvertRollup(t *testing.T) {
	var r gqlRollup
	if err := json.Unmarshal([]byte(`{"state":"SUCCESS"}`), &r); err != nil {
		t.Fatal(err)
	}
	if out, _ := convertRollup(&r); out != "SUCCESS" {
		t.Fatalf("state = %s", out)
	}
	if err := json.Unmarshal([]byte(`{"state":"PENDING","contexts":{"nodes":[{"__typename":"CheckRun","name":"ci","status":"IN_PROGRESS"}]}}`), &r); err != nil {
		t.Fatal(err)
	}
	if out, _ := convertRollup(&r); out != "PENDING" {
		t.Fatalf("state = %s", out)
	}
	if err := json.Unmarshal([]byte(`{"state":"SUCCESS","contexts":{"nodes":[{"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"FAILURE"}]}}`), &r); err != nil {
		t.Fatal(err)
	}
	if out, _ := convertRollup(&r); out != "FAILURE" {
		t.Fatalf("state = %s", out)
	}
}

func TestThreadNeedsAttention(t *testing.T) {
	now := time.Now()
	th := Thread{ID: "t", Comments: []Comment{{Author: "reviewer", ID: "1", CreatedAt: now}}}
	if !th.NeedsAttention("alytaphoenix") {
		t.Fatal("reviewer comment should need attention")
	}
	th.Comments = append(th.Comments, Comment{Author: "alytaphoenix", ID: "2"})
	if th.NeedsAttention("alytaphoenix") {
		t.Fatal("own latest comment should not need attention")
	}
	th.Comments = append(th.Comments, Comment{Author: "dependabot[bot]", ID: "3"})
	if th.NeedsAttention("alytaphoenix") {
		t.Fatal("bot comment should not need attention")
	}
	th.IsResolved = true
	if th.NeedsAttention("alytaphoenix") {
		t.Fatal("resolved thread should not need attention")
	}
}

func TestQueries(t *testing.T) {
	q, aliases := basicQuery([]string{"jundot/omlx", "huggingface/candle"})
	if len(aliases) != 2 || !strings.Contains(q, `r0: repository(owner: "jundot", name: "omlx")`) {
		t.Fatalf("unexpected basic query: %s", q)
	}
	if !strings.Contains(q, "first: 20") {
		t.Fatal("basic query should only fetch recent PRs")
	}
	d, dalias := detailQuery([]PR{{Slug: "jundot/omlx", Number: 3141}})
	if len(dalias) != 1 || !strings.Contains(d, "pullRequest(number: 3141)") || !strings.Contains(d, "reviewThreads") {
		t.Fatalf("unexpected detail query: %s", d)
	}
}
