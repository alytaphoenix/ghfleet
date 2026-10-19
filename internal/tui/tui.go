// Package tui is a small terminal client for a ghfleet server.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/alytaphoenix/ghfleet/internal/client"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	tabStyle    = lipgloss.NewStyle().Padding(0, 1)
	activeTab   = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("230")).Background(lipgloss.Color("62"))
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selected    = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("238"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("120"))
	statusStyle = map[string]lipgloss.Style{
		"awaiting_approval": lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true),
		"pending":           lipgloss.NewStyle().Foreground(lipgloss.Color("111")),
		"running":           lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		"applied":           lipgloss.NewStyle().Foreground(lipgloss.Color("120")),
		"failed":            lipgloss.NewStyle().Foreground(lipgloss.Color("203")),
		"denied":            mutedStyle,
	}
)

type refreshMsg struct {
	status  server.StatusResponse
	jobs    []store.Job
	prs     []store.PR
	runners []store.Runner
	err     error
}

type detailMsg struct {
	detail server.JobDetail
	err    error
}

type actionMsg struct {
	text string
	err  error
}

type tickMsg time.Time

type Model struct {
	client  *client.Client
	tab     int // 0 inbox, 1 PRs, 2 runners
	cursor  int
	status  server.StatusResponse
	jobs    []store.Job
	prs     []store.PR
	runners []store.Runner
	detail  *server.JobDetail
	err     error
	flash   string
	width   int
	height  int
}

// New creates a TUI model.
func New(c *client.Client) *Model {
	return &Model{client: c, width: 100, height: 30}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick())
}

func (m *Model) refresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		status, err := m.client.Status(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		jobs, err := m.client.Jobs(ctx, "inbox")
		if err != nil {
			return refreshMsg{err: err}
		}
		prs, err := m.client.PRs(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		runners, err := m.client.Runners(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		return refreshMsg{status: status, jobs: jobs, prs: prs, runners: runners}
	}
}

func tick() tea.Cmd {
	return tea.Tick(10*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case refreshMsg:
		m.err = msg.err
		if msg.err == nil {
			m.status, m.jobs, m.prs, m.runners = msg.status, msg.jobs, msg.prs, msg.runners
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case detailMsg:
		if msg.err != nil {
			m.flash = "error: " + msg.err.Error()
		} else {
			d := msg.detail
			m.detail = &d
		}
	case actionMsg:
		if msg.err != nil {
			m.flash = "error: " + msg.err.Error()
		} else {
			m.flash = msg.text
		}
		return m, m.refresh()
	case tea.KeyMsg:
		if m.detail != nil {
			switch msg.String() {
			case "esc", "q", "enter":
				m.detail = nil
			case "a":
				if m.detail.Job.Status == "awaiting_approval" {
					return m, m.approveCmd(m.detail.Job)
				}
			case "d":
				if m.detail.Job.Status != "applied" && m.detail.Job.Status != "denied" {
					return m, m.denyCmd(m.detail.Job)
				}
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab", "right", "l":
			m.tab = (m.tab + 1) % 3
			m.cursor = 0
		case "shift+tab", "left", "h":
			m.tab = (m.tab + 2) % 3
			m.cursor = 0
		case "1":
			m.tab, m.cursor = 0, 0
		case "2":
			m.tab, m.cursor = 1, 0
		case "3":
			m.tab, m.cursor = 2, 0
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < m.listLen()-1 {
				m.cursor++
			}
		case "r":
			return m, m.refresh()
		case "enter":
			if job, ok := m.selectedJob(); ok {
				return m, func() tea.Msg {
					d, err := m.client.Job(context.Background(), job.ID, true)
					return detailMsg{detail: d, err: err}
				}
			}
		case "a":
			if job, ok := m.selectedJob(); ok && job.Status == "awaiting_approval" {
				return m, m.approveCmd(job)
			}
		case "d":
			if job, ok := m.selectedJob(); ok && job.Status != "applied" && job.Status != "denied" {
				return m, m.denyCmd(job)
			}
		}
	}
	return m, nil
}

func (m *Model) approveCmd(job store.Job) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Approve(context.Background(), job.ID)
		return actionMsg{text: "approved " + shortID(job.ID), err: err}
	}
}

func (m *Model) denyCmd(job store.Job) tea.Cmd {
	return func() tea.Msg {
		err := m.client.Deny(context.Background(), job.ID)
		return actionMsg{text: "denied " + shortID(job.ID), err: err}
	}
}

func (m *Model) listLen() int {
	switch m.tab {
	case 1:
		return len(m.prs)
	case 2:
		return len(m.runners)
	default:
		return len(m.jobs)
	}
}

func (m *Model) selectedJob() (store.Job, bool) {
	if m.tab != 0 || m.cursor < 0 || m.cursor >= len(m.jobs) {
		return store.Job{}, false
	}
	return m.jobs[m.cursor], true
}

func (m *Model) View() string {
	if m.detail != nil {
		return m.detailView()
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(" ghfleet "))
	b.WriteString(mutedStyle.Render(fmt.Sprintf("  %s  viewer %s  ", m.client.BaseURL, m.status.Viewer)))
	b.WriteString("\n\n")

	tabs := []string{"Inbox", "PRs", "Runners"}
	for i, t := range tabs {
		if i == m.tab {
			b.WriteString(activeTab.Render(t))
		} else {
			b.WriteString(tabStyle.Render(t))
		}
	}
	b.WriteString(mutedStyle.Render(fmt.Sprintf("   prs %d  threads %d  runners %d/%d",
		m.status.Stats.PRs, m.status.Stats.ThreadsOpen, m.status.Stats.RunnersOnline, m.status.Stats.RunnersTotal)))
	b.WriteString("\n\n")

	if m.err != nil {
		b.WriteString(errStyle.Render("server error: " + m.err.Error()))
		b.WriteString("\n")
	} else {
		switch m.tab {
		case 1:
			b.WriteString(m.prsView())
		case 2:
			b.WriteString(m.runnersView())
		default:
			b.WriteString(m.jobsView())
		}
	}

	if m.flash != "" {
		b.WriteString("\n" + okStyle.Render(m.flash))
	}
	b.WriteString("\n\n")
	b.WriteString(mutedStyle.Render("tab switch  ↑/↓ move  enter detail  a approve  d deny  r refresh  q quit"))
	return b.String()
}

func (m *Model) jobsView() string {
	if len(m.jobs) == 0 {
		return mutedStyle.Render("No jobs. The inbox is clear.")
	}
	var b strings.Builder
	header := fmt.Sprintf("%-30s %-18s %-10s %-7s %-9s %s", "JOB", "STATUS", "REPO", "PR", "KIND", "SUMMARY")
	b.WriteString(mutedStyle.Render(header) + "\n")
	for i, j := range m.jobs {
		pr := "-"
		if j.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", j.PRNumber)
		}
		line := fmt.Sprintf("%-30s %-18s %-10s %-7s %-9s %s", shortID(j.ID), j.Status, j.Repo, pr, j.Kind, truncate(j.Summary, 48))
		if i == m.cursor {
			b.WriteString(selected.Render(line))
		} else {
			if st, ok := statusStyle[j.Status]; ok {
				b.WriteString(st.Render(fmt.Sprintf("%-30s %-18s", shortID(j.ID), j.Status)))
				b.WriteString(fmt.Sprintf(" %-10s %-7s %-9s %s", j.Repo, pr, j.Kind, truncate(j.Summary, 48)))
			} else {
				b.WriteString(line)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) prsView() string {
	if len(m.prs) == 0 {
		return mutedStyle.Render("No PRs tracked yet. Run `ghfleet scan`.")
	}
	var b strings.Builder
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%-18s %-7s %-10s %-18s %-8s %s", "REPO", "PR", "STATE", "REVIEW", "CHECKS", "TITLE")) + "\n")
	for i, pr := range m.prs {
		repo := pr.Repo
		if pr.Slug != "" {
			repo = pr.Slug
		}
		line := fmt.Sprintf("%-18s %-7s %-10s %-18s %-8s %s", truncate(repo, 18), fmt.Sprintf("#%d", pr.Number),
			server.PRState(pr), empty(pr.ReviewDecision, "-"), empty(pr.CheckState, "-"), truncate(pr.Title, 50))
		if i == m.cursor {
			b.WriteString(selected.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) runnersView() string {
	if len(m.runners) == 0 {
		return mutedStyle.Render("No runners registered. Start one with `ghfleet runner`.")
	}
	var b strings.Builder
	b.WriteString(mutedStyle.Render(fmt.Sprintf("%-16s %-10s %-12s %-24s %-20s %s", "ID", "STATUS", "OS/ARCH", "LABELS", "REPOS", "LAST SEEN")) + "\n")
	for i, r := range m.runners {
		line := fmt.Sprintf("%-16s %-10s %-12s %-24s %-20s %s", truncate(r.ID, 16), r.Status,
			r.OS+"/"+r.Arch, truncate(strings.Join(r.Labels, ","), 24),
			truncate(strings.Join(r.Repos, ","), 20), ago(r.LastSeen))
		if i == m.cursor {
			b.WriteString(selected.Render(line))
		} else {
			b.WriteString(line)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) detailView() string {
	d := *m.detail
	var b strings.Builder
	b.WriteString(titleStyle.Render("job " + d.Job.ID))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("status:  %s\nrepo:    %s  PR #%d  kind %s\nrisk:    %s  decision %s  needs_human %v\n",
		d.Job.Status, d.Job.Repo, d.Job.PRNumber, d.Job.Kind, d.Job.Risk,
		empty(d.Result.Decision, "-"), d.Result.NeedsHuman))
	if d.PRTitle != "" {
		b.WriteString("title:   " + d.PRTitle + "\n")
	}
	if d.Job.Summary != "" {
		b.WriteString("\n" + d.Job.Summary + "\n")
	}
	if d.Result.Reply != "" {
		b.WriteString("\nreply:\n" + d.Result.Reply + "\n")
	}
	if len(d.Result.Findings) > 0 {
		b.WriteString("\nfindings:\n")
		for _, f := range d.Result.Findings {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Fprintf(&b, "  [%s] %s: %s\n", empty(f.Severity, "info"), empty(loc, "-"), f.Body)
		}
	}
	if d.Result.ChecksOutput != "" {
		b.WriteString("\nverification:\n" + truncate(d.Result.ChecksOutput, 2000) + "\n")
	}
	if d.Patch != "" {
		b.WriteString("\npatch:\n")
		lines := strings.Split(d.Patch, "\n")
		if len(lines) > 200 {
			lines = lines[:200]
			lines = append(lines, "... (truncated)")
		}
		b.WriteString(strings.Join(lines, "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(mutedStyle.Render("esc back  a approve  d deny"))
	return b.String()
}

func shortID(id string) string {
	if len(id) > 30 {
		return id[:29] + "…"
	}
	return id
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func empty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Minute {
		return d.String() + " ago"
	}
	return d.Round(time.Minute).String() + " ago"
}
