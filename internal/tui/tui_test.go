package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/client"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

func TestViewRenders(t *testing.T) {
	m := New(client.New("http://127.0.0.1:8787", ""))
	m.status.Viewer = "me"
	m.jobs = []store.Job{{ID: "job1", Status: "awaiting_approval", Repo: "omlx", PRNumber: 1, Kind: "review-response", Summary: "fix"}}
	m.runners = []store.Runner{{ID: "mac", Status: "online", OS: "darwin", Arch: "arm64", LastSeen: time.Now()}}
	for _, tab := range []int{0, 1, 2} {
		m.tab = tab
		out := m.View()
		if !strings.Contains(out, "ghfleet") {
			t.Fatalf("tab %d missing header: %s", tab, out)
		}
	}
}

func TestDetailView(t *testing.T) {
	m := New(client.New("http://127.0.0.1:8787", ""))
	m.detail = &server.JobDetail{
		Job:    store.Job{ID: "job1", Status: "awaiting_approval", Repo: "omlx", PRNumber: 1, Kind: "review-response"},
		Result: jobs.Result{Decision: "fix", Summary: "fixed", Risk: "low"},
		Patch:  "diff --git a/a b/a\n+hello",
	}
	out := m.View()
	if !strings.Contains(out, "patch:") || !strings.Contains(out, "+hello") {
		t.Fatalf("detail missing patch: %s", out)
	}
}
