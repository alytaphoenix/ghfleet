// Command ghfleet is a local-first GitHub PR control plane with remote runners.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/alytaphoenix/ghfleet/internal/agent"
	"github.com/alytaphoenix/ghfleet/internal/client"
	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/discover"
	"github.com/alytaphoenix/ghfleet/internal/git"
	"github.com/alytaphoenix/ghfleet/internal/github"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
	"github.com/alytaphoenix/ghfleet/internal/reconcile"
	"github.com/alytaphoenix/ghfleet/internal/runner"
	"github.com/alytaphoenix/ghfleet/internal/server"
	"github.com/alytaphoenix/ghfleet/internal/store"
	"github.com/alytaphoenix/ghfleet/internal/tui"
)

var (
	flagConfig       string
	flagRunnerConfig string
	flagServer       string
	flagToken        string
	flagJSON         bool
)

func main() {
	root := &cobra.Command{
		Use:           "ghfleet",
		Short:         "Local-first GitHub PR control plane with remote runners",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&flagConfig, "config", config.DefaultPath(), "server config path")
	root.PersistentFlags().StringVar(&flagRunnerConfig, "runner-config", config.DefaultRunnerPath(), "runner config path")
	root.PersistentFlags().StringVar(&flagServer, "server", "", "server URL (default: from config, or http://127.0.0.1:<listen>)")
	root.PersistentFlags().StringVar(&flagToken, "token", "", "API token (default: GHFLEET_TOKEN or config)")
	root.PersistentFlags().BoolVar(&flagJSON, "json", false, "emit JSON")

	root.AddCommand(
		initCmd(), doctorCmd(), scanCmd(), statusCmd(), prsCmd(), jobsCmd(), showCmd(),
		approveCmd(), denyCmd(), patchCmd(), runnersCmd(), tuiCmd(), reviewCmd(), serverCmd(), runnerCmd(),
		syncCmd(), securityCmd(), actionsCmd(), reviewPRCmd(), versionCmd(),
	)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func initCmd() *cobra.Command {
	var reposRoot string
	var force bool
	var serverURL string
	var skipServer, skipRunner bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Discover local checkouts and write starter configs",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			gh, err := github.New(ctx)
			if err != nil {
				return err
			}
			found, err := discover.Repos(ctx, reposRoot, gh.Login)
			if err != nil {
				return err
			}
			if len(found) == 0 {
				return fmt.Errorf("no git repositories with GitHub remotes found under %s", reposRoot)
			}
			if !skipServer {
				path := config.ExpandPath(flagConfig)
				if _, statErr := os.Stat(path); statErr == nil && !force {
					fmt.Printf("server config exists: %s (use --force to overwrite)\n", path)
				} else {
					cfg := discover.ServerConfig(found, gh.Login)
					raw, err := cfg.Marshal()
					if err != nil {
						return err
					}
					if err := writeFile(path, raw); err != nil {
						return err
					}
					fmt.Printf("wrote server config: %s\n", path)
				}
			}
			if !skipRunner {
				path := config.ExpandPath(flagRunnerConfig)
				if _, statErr := os.Stat(path); statErr == nil && !force {
					fmt.Printf("runner config exists: %s (use --force to overwrite)\n", path)
				} else {
					if serverURL == "" {
						serverURL = "http://127.0.0.1:8787"
					}
					rc := discover.RunnerConfig(found, serverURL)
					raw, err := rc.Marshal()
					if err != nil {
						return err
					}
					if err := writeFile(path, raw); err != nil {
						return err
					}
					fmt.Printf("wrote runner config: %s\n", path)
				}
			}
			fmt.Printf("%s\n", discover.Summary(found))
			fmt.Println("next: ghfleet doctor && ghfleet scan --local")
			return nil
		},
	}
	cmd.Flags().StringVar(&reposRoot, "repos-root", "~/repos", "directory containing local checkouts")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing configs")
	cmd.Flags().StringVar(&serverURL, "server-url", "", "server URL for the runner config")
	cmd.Flags().BoolVar(&skipServer, "skip-server", false, "do not write the server config")
	cmd.Flags().BoolVar(&skipRunner, "skip-runner", false, "do not write the runner config")
	return cmd
}

func writeFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o644)
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check config, auth, remotes, runners, and the server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			cfg, cfgErr := config.Load(flagConfig)
			if cfgErr != nil {
				cfg = &config.Config{}
				cfg.ApplyDefaults()
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "CHECK\tSTATUS\tDETAIL")
			ok := func(name, detail string) { _, _ = fmt.Fprintf(w, "%s\tOK\t%s\n", name, detail) }
			warn := func(name, detail string) { _, _ = fmt.Fprintf(w, "%s\tWARN\t%s\n", name, detail) }
			fail := func(name, detail string) { _, _ = fmt.Fprintf(w, "%s\tFAIL\t%s\n", name, detail) }

			if _, err := exec.LookPath("gh"); err == nil {
				ok("gh", "installed")
			} else {
				fail("gh", "not found")
			}
			gh, err := github.New(ctx)
			if err != nil {
				fail("github auth", err.Error())
			} else {
				ok("github auth", "viewer="+gh.Login)
			}
			if cfgErr != nil {
				warn("server config", cfgErr.Error())
			} else {
				ok("server config", config.ExpandPath(flagConfig))
			}
			ok("state dir", cfg.Server.StateDir)
			if err := os.MkdirAll(cfg.Server.StateDir, 0o755); err != nil {
				fail("state dir writable", err.Error())
			} else {
				ok("state dir writable", cfg.Server.StateDir)
			}
			if cfg.Server.APIToken == "" {
				if strings.HasPrefix(cfg.Server.Listen, "127.0.0.1") || strings.HasPrefix(cfg.Server.Listen, "localhost") {
					ok("api token", "empty (loopback listener)")
				} else {
					warn("api token", "empty on a non-loopback listener")
				}
			} else {
				ok("api token", "configured")
			}

			var runnerCfg *config.RunnerConfig
			if rc, err := config.LoadRunner(flagRunnerConfig); err == nil {
				runnerCfg = rc
				ok("runner config", config.ExpandPath(flagRunnerConfig))
				if _, err := exec.LookPath(rc.Agent.Command); err == nil {
					ok("agent", rc.Agent.Command)
				} else {
					fail("agent", rc.Agent.Command+" not found")
				}
				if fields := strings.Fields(rc.Security.Command); len(fields) > 0 {
					if _, err := exec.LookPath(fields[0]); err == nil {
						ok("security scanner", fields[0])
					} else {
						warn("security scanner", fields[0]+" not found (needed only for auto_security repos)")
					}
				}
				for _, repo := range rc.Repos {
					if _, err := os.Stat(filepath.Join(repo.Path, ".git")); err != nil {
						fail("repo "+repo.Name, "not a git checkout: "+repo.Path)
						continue
					}
					remotes, err := git.Remotes(ctx, repo.Path)
					if err != nil {
						fail("repo "+repo.Name, err.Error())
						continue
					}
					if repo.FetchRemote != "" {
						if _, ok := remotes[repo.FetchRemote]; !ok {
							warn("repo "+repo.Name, "fetch remote missing: "+repo.FetchRemote)
							continue
						}
					}
					if repo.PushRemote != "" {
						if _, ok := remotes[repo.PushRemote]; !ok {
							warn("repo "+repo.Name, "push remote missing: "+repo.PushRemote)
							continue
						}
					}
					fetch := repo.FetchRemote
					if fetch == "" {
						fetch = "(from server)"
					}
					push := repo.PushRemote
					if push == "" {
						push = "(from server)"
					}
					detail := "path=" + repo.Path + " fetch=" + fetch + " push=" + push
					if len(repo.Checks) > 0 {
						detail += " checks=" + strings.Join(repo.Checks, "; ")
					} else {
						detail += " checks=(none)"
					}
					ok("repo "+repo.Name, detail)
				}
			} else {
				warn("runner config", err.Error())
			}

			checkURL, checkToken := serverURL(), apiToken()
			if flagServer == "" && runnerCfg != nil {
				checkURL = runnerCfg.ServerURL
				if runnerCfg.Token != "" {
					checkToken = runnerCfg.Token
				}
			}
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			st, err := client.New(checkURL, checkToken).Status(cctx)
			if err != nil {
				warn("server", "not reachable at "+checkURL)
			} else {
				ok("server", fmt.Sprintf("%s version=%s prs=%d runners=%d", checkURL, st.Version, st.Stats.PRs, st.Stats.RunnersOnline))
			}
			return w.Flush()
		},
	}
}

func scanCmd() *cobra.Command {
	var local bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Poll GitHub and enqueue work (local one-shot or via server)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			remote := !local
			if remote && flagServer == "" && os.Getenv("GHFLEET_SERVER") == "" {
				// Prefer a running server, but fall back to a one-shot local scan.
				pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				_, pingErr := newClient().Status(pingCtx)
				cancel()
				remote = pingErr == nil
			}
			if remote {
				summary, err := newClient().Scan(ctx)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(summary)
				}
				fmt.Printf("scanned %d repos, %d PRs, enqueued %d jobs\n", summary.Repos, summary.PRs, summary.NewJobs)
				for _, e := range summary.Errors {
					fmt.Println("error:", e)
				}
				return nil
			}
			summary, prs, err := localScan(ctx)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]any{"summary": summary, "prs": prs})
			}
			fmt.Printf("scanned %d repos, %d PRs, enqueued %d jobs\n", summary.Repos, summary.PRs, summary.NewJobs)
			for _, e := range summary.Errors {
				fmt.Println("error:", e)
			}
			printPRs(prs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&local, "local", false, "run a one-shot scan without a server")
	return cmd
}

func localScan(ctx context.Context) (reconcile.Summary, []store.PR, error) {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		return reconcile.Summary{}, nil, err
	}
	if err := os.MkdirAll(cfg.Server.StateDir, 0o755); err != nil {
		return reconcile.Summary{}, nil, err
	}
	st, err := store.Open(filepath.Join(cfg.Server.StateDir, "ghfleet.db"))
	if err != nil {
		return reconcile.Summary{}, nil, err
	}
	defer func() { _ = st.Close() }()
	gh, err := github.New(ctx)
	if err != nil {
		return reconcile.Summary{}, nil, err
	}
	rec := &reconcile.Reconciler{Cfg: cfg, Store: st, GH: gh, Viewer: gh.Login}
	summary := rec.Run(ctx)
	prs, err := st.ListPRs()
	return summary, prs, err
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show server status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			st, err := newClient().Status(ctx)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(st)
			}
			fmt.Printf("server %s  version %s  viewer %s\n", serverURL(), st.Version, st.Viewer)
			fmt.Printf("PRs %d  open threads %d  runners %d/%d  agent cost 24h $%.2f\n",
				st.Stats.PRs, st.Stats.ThreadsOpen, st.Stats.RunnersOnline, st.Stats.RunnersTotal, st.Stats.CostUSD24h)
			fmt.Printf("jobs: pending %d  running %d  awaiting approval %d  approved %d  applied %d  failed %d\n",
				st.Stats.JobsPending, st.Stats.JobsRunning, st.Stats.JobsAwaitingApproval,
				st.Stats.JobsApproved, st.Stats.JobsApplied, st.Stats.JobsFailed)
			if !st.LastSummary.At.IsZero() {
				fmt.Printf("last scan: %s  new jobs %d  errors %d\n",
					st.LastSummary.At.Local().Format(time.RFC3339), st.LastSummary.NewJobs, len(st.LastSummary.Errors))
			}
			return nil
		},
	}
}

func prsCmd() *cobra.Command {
	var state string
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "List tracked pull requests",
		RunE: func(cmd *cobra.Command, args []string) error {
			prs, err := newClient().PRs(context.Background())
			if err != nil {
				return err
			}
			if state != "" && state != "all" {
				filtered := prs[:0]
				for _, pr := range prs {
					if server.PRState(pr) == state {
						filtered = append(filtered, pr)
					}
				}
				prs = filtered
			}
			if flagJSON {
				return printJSON(prs)
			}
			printPRs(prs)
			return nil
		},
	}
	cmd.Flags().StringVar(&state, "state", "", "filter: conflict, ci, changes, draft, approved, open")
	return cmd
}

func jobsCmd() *cobra.Command {
	var status string
	cmd := &cobra.Command{
		Use:     "jobs",
		Aliases: []string{"inbox"},
		Short:   "List jobs (the approval inbox)",
		RunE: func(cmd *cobra.Command, args []string) error {
			list, err := newClient().Jobs(context.Background(), status)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(list)
			}
			printJobs(list)
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "inbox", "comma-separated statuses, or inbox")
	return cmd
}

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <job>",
		Short: "Show a job, its result, and its patch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			detail, err := newClient().Job(context.Background(), args[0], true)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(detail)
			}
			printJobDetail(detail)
			return nil
		},
	}
}

func approveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <job>",
		Short: "Approve a job (allows the runner to push/comment)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newClient().Approve(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Println("approved", args[0])
			return nil
		},
	}
}

func denyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deny <job>",
		Short: "Deny a job and mark its review threads handled",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := newClient().Deny(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Println("denied", args[0])
			return nil
		},
	}
}

func patchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "patch <job>",
		Short: "Print a job's patch",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			patch, err := newClient().Patch(context.Background(), args[0])
			if err != nil {
				return err
			}
			fmt.Print(patch)
			return nil
		},
	}
}

func runnersCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runners",
		Short: "List registered runners",
		RunE: func(cmd *cobra.Command, args []string) error {
			rs, err := newClient().Runners(context.Background())
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(rs)
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATUS\tOS/ARCH\tLABELS\tREPOS\tLAST SEEN")
			for _, r := range rs {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s/%s\t%s\t%s\t%s\n", r.ID, r.Status, r.OS, r.Arch,
					strings.Join(r.Labels, ","), strings.Join(r.Repos, ","), ago(r.LastSeen))
			}
			return w.Flush()
		},
	}
}

func reviewCmd() *cobra.Command {
	var repoPath, base string
	var working, strict bool
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Run a local preflight review of the current diff (no writes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			rc, err := config.LoadRunner(flagRunnerConfig)
			if err != nil {
				return err
			}
			if repoPath == "" {
				repoPath, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			repoPath = config.ExpandPath(repoPath)
			rr, ok := runnerRepoForPath(rc, repoPath)
			if !ok {
				return fmt.Errorf("%s is not in %s; add it to the runner config", repoPath, config.ExpandPath(flagRunnerConfig))
			}
			fetch := rr.FetchRemote
			if fetch == "" {
				fetch = "origin"
			}
			branch := rr.DefaultBranch
			if branch == "" {
				branch = "main"
			}
			if base == "" {
				base = fetch + "/" + branch
			}
			var diff string
			if working {
				diff, err = git.DiffWorking(ctx, repoPath)
			} else {
				if _, err := git.MustRun(ctx, repoPath, "rev-parse", "--verify", base); err != nil {
					return fmt.Errorf("base %q not found; fetch it first (git fetch %s %s)", base, fetch, branch)
				}
				diff, err = git.DiffPR(ctx, repoPath, base, "HEAD")
			}
			if err != nil {
				return err
			}
			if strings.TrimSpace(diff) == "" {
				fmt.Println("no changes to review")
				return nil
			}
			head, _ := git.HeadSHA(ctx, repoPath)
			spec := jobs.Spec{
				Repo: jobs.RepoSpec{Name: rr.Name, Upstream: "local"},
				PR:   jobs.PRSpec{Title: "local preflight", HeadSHA: head, BaseRef: base},
			}
			ar := &agent.Runner{Cfg: rc.Agent}
			res, err := ar.Run(ctx, agent.Request{
				Kind: jobs.KindPreflight, Workdir: repoPath, Spec: spec, Diff: diff,
				Guidelines: localGuidelines(repoPath),
			})
			if err != nil {
				return err
			}
			var out jobs.Result
			if err := json.Unmarshal(res.JSON, &out); err != nil {
				return fmt.Errorf("decode review: %w", err)
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("preflight: %s  risk %s\n", empty(out.Summary, "no summary"), empty(out.Risk, "-"))
			for _, f := range out.Findings {
				loc := f.File
				if f.Line > 0 {
					loc = fmt.Sprintf("%s:%d", f.File, f.Line)
				}
				fmt.Printf("  [%s] %s: %s\n", empty(f.Severity, "info"), empty(loc, "-"), f.Body)
			}
			if strict {
				for _, f := range out.Findings {
					if strings.EqualFold(f.Severity, "error") {
						return fmt.Errorf("preflight found %d error-level finding(s)", countErrors(out.Findings))
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "repo path (default: cwd)")
	cmd.Flags().StringVar(&base, "base", "", "base ref (default: <fetch-remote>/<default-branch>)")
	cmd.Flags().BoolVar(&working, "working", false, "review the working tree instead of base...HEAD")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit non-zero on error-level findings")
	return cmd
}

func runnerRepoForPath(rc *config.RunnerConfig, path string) (config.RunnerRepo, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return config.RunnerRepo{}, false
	}
	for _, rr := range rc.Repos {
		rp, err := filepath.Abs(rr.Path)
		if err != nil {
			continue
		}
		if abs == rp || strings.HasPrefix(abs, rp+string(filepath.Separator)) {
			return rr, true
		}
	}
	return config.RunnerRepo{}, false
}

func localGuidelines(dir string) string {
	var b strings.Builder
	for _, name := range []string{"CLAUDE.md", "AGENTS.md", ".github/copilot-instructions.md"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", name, raw)
	}
	return b.String()
}

func countErrors(findings []jobs.Finding) int {
	n := 0
	for _, f := range findings {
		if strings.EqualFold(f.Severity, "error") {
			n++
		}
	}
	return n
}

func tuiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the terminal client",
		RunE: func(cmd *cobra.Command, args []string) error {
			p := tea.NewProgram(tui.New(newClient()), tea.WithAltScreen())
			_, err := p.Run()
			return err
		},
	}
}

func serverCmd() *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the control-plane server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(flagConfig)
			if err != nil {
				return err
			}
			st, err := store.Open(filepath.Join(cfg.Server.StateDir, "ghfleet.db"))
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			gh, err := github.New(context.Background())
			if err != nil {
				return err
			}
			srv := server.New(cfg, st, gh, gh.Login, nil)
			if once {
				summary := srv.ReconcileOnce(context.Background())
				return printJSON(summary)
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "run one reconcile pass and exit")
	return cmd
}

func runnerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "runner",
		Short: "Run a job runner for this machine",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadRunner(flagRunnerConfig)
			if err != nil {
				return err
			}
			if flagServer != "" {
				cfg.ServerURL = flagServer
			}
			if flagToken != "" {
				cfg.Token = flagToken
			}
			r := runner.New(cfg, nil)
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return r.Run(ctx)
		},
	}
}

func syncCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "sync [repo]",
		Short: "Scan, or enqueue a manual fork sync for one repo",
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			if len(args) > 0 {
				if err := c.SyncRepo(context.Background(), args[0]); err != nil {
					return err
				}
				fmt.Println("fork sync enqueued for", args[0])
				return nil
			}
			summary, err := c.Scan(context.Background())
			if err != nil {
				return err
			}
			fmt.Printf("scanned %d repos, %d PRs, enqueued %d jobs\n", summary.Repos, summary.PRs, summary.NewJobs)
			return nil
		},
	}
}

func securityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "security <repo>",
		Short: "Enqueue a dependency CVE scan/fix for one repo",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			if err := c.SecurityRepo(context.Background(), args[0]); err != nil {
				return err
			}
			fmt.Println("dependency security scan enqueued for", args[0])
			return nil
		},
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("ghfleet", server.Version)
		},
	}
}

func newClient() *client.Client {
	return client.New(serverURL(), apiToken())
}

func serverURL() string {
	if flagServer != "" {
		return normalizeURL(flagServer)
	}
	if v := os.Getenv("GHFLEET_SERVER"); v != "" {
		return normalizeURL(v)
	}
	if cfg, err := config.Load(flagConfig); err == nil {
		addr := cfg.Server.Listen
		if strings.HasPrefix(addr, ":") {
			addr = "127.0.0.1" + addr
		}
		if strings.HasPrefix(addr, "0.0.0.0:") {
			addr = "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
		}
		return "http://" + addr
	}
	if rc, err := config.LoadRunner(flagRunnerConfig); err == nil {
		return normalizeURL(rc.ServerURL)
	}
	return "http://127.0.0.1:8787"
}

func normalizeURL(v string) string {
	if strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") {
		return strings.TrimRight(v, "/")
	}
	return "http://" + strings.TrimRight(v, "/")
}

func apiToken() string {
	if flagToken != "" {
		return flagToken
	}
	if v := os.Getenv("GHFLEET_TOKEN"); v != "" {
		return v
	}
	if cfg, err := config.Load(flagConfig); err == nil && cfg.Server.APIToken != "" {
		return cfg.Server.APIToken
	}
	if rc, err := config.LoadRunner(flagRunnerConfig); err == nil {
		return rc.Token
	}
	return ""
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printPRs(prs []store.PR) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "REPO\tPR\tSTATE\tREVIEW\tCHECKS\tTHREADS\tTITLE")
	for _, pr := range prs {
		repo := pr.Repo
		if pr.Slug != "" {
			repo = pr.Slug
		}
		_, _ = fmt.Fprintf(w, "%s\t#%d\t%s\t%s\t%s\t%d\t%s\n", repo, pr.Number, server.PRState(pr),
			empty(pr.ReviewDecision, "-"), empty(pr.CheckState, "-"), openThreads(pr), truncate(pr.Title, 70))
	}
	_ = w.Flush()
}

func openThreads(pr store.PR) int {
	n := 0
	for _, t := range pr.Threads {
		if !t.IsResolved && !t.IsOutdated {
			n++
		}
	}
	return n
}

func printJobs(list []store.Job) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "JOB\tSTATUS\tREPO\tPR\tKIND\tRISK\tSUMMARY")
	for _, j := range list {
		pr := "-"
		if j.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", j.PRNumber)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", truncate(j.ID, 28), j.Status, j.Repo, pr, j.Kind,
			empty(j.Risk, "-"), truncate(j.Summary, 55))
	}
	_ = w.Flush()
}

func printJobDetail(d server.JobDetail) {
	j := d.Job
	fmt.Printf("job:     %s\n", j.ID)
	fmt.Printf("status:  %s\n", j.Status)
	fmt.Printf("repo:    %s  PR #%d  kind %s\n", j.Repo, j.PRNumber, j.Kind)
	if d.PRTitle != "" {
		fmt.Printf("title:   %s\n", d.PRTitle)
	}
	if d.PRURL != "" {
		fmt.Printf("url:     %s\n", d.PRURL)
	}
	fmt.Printf("risk:    %s  decision %s  needs_human %v\n", j.Risk, empty(d.Result.Decision, "-"), d.Result.NeedsHuman)
	if j.Summary != "" {
		fmt.Printf("summary: %s\n", j.Summary)
	}
	if d.Result.Reply != "" {
		fmt.Printf("\nreply:\n%s\n", d.Result.Reply)
	}
	if len(d.Result.Findings) > 0 {
		fmt.Println("\nfindings:")
		for _, f := range d.Result.Findings {
			loc := f.File
			if f.Line > 0 {
				loc = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			fmt.Printf("  [%s] %s: %s\n", empty(f.Severity, "info"), empty(loc, "-"), f.Body)
		}
	}
	if d.Result.ChecksOutput != "" {
		fmt.Printf("\nverification:\n%s\n", truncate(d.Result.ChecksOutput, 4000))
	}
	if d.Patch != "" {
		fmt.Printf("\npatch:\n%s\n", d.Patch)
	}
	if j.Status == "awaiting_approval" {
		fmt.Printf("\napprove: ghfleet approve %s\ndeny:    ghfleet deny %s\n", j.ID, j.ID)
	}
}

func empty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
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
