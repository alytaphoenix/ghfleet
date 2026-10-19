// Package config loads and validates ghfleet server and runner configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from strings like "2m" or "24h".
type Duration time.Duration

func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == 0 || value.Value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// ServerConfig is the control-plane configuration.
type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	APIToken     string   `yaml:"api_token,omitempty"`
	StateDir     string   `yaml:"state_dir"`
	PollInterval Duration `yaml:"poll_interval"`
	Viewer       string   `yaml:"viewer,omitempty"`
	LogLevel     string   `yaml:"log_level,omitempty"`
}

// Approvals controls which actions need a human.
type Approvals struct {
	Push           string `yaml:"push"`            // ask | auto | never
	Comment        string `yaml:"comment"`         // ask | auto | never
	Merge          string `yaml:"merge"`           // ask | auto | never
	ForkSync       string `yaml:"fork_sync"`       // ask | auto
	ResolveThreads string `yaml:"resolve_threads"` // never | bot | all
}

// Limits caps agent work and spend.
type Limits struct {
	MaxJobsPerHour      int      `yaml:"max_jobs_per_hour"`
	MaxFixAttemptsPerPR int      `yaml:"max_fix_attempts_per_pr"`
	MaxBudgetUSDPerDay  float64  `yaml:"max_budget_usd_per_day"`
	JobTimeout          Duration `yaml:"job_timeout"`
	ApprovalTimeout     Duration `yaml:"approval_timeout"`
}

// Repo describes an upstream project and the fork ghfleet is allowed to push to.
type Repo struct {
	Name             string   `yaml:"name"`
	Upstream         string   `yaml:"upstream"`
	Fork             string   `yaml:"fork,omitempty"`
	Own              bool     `yaml:"own,omitempty"`
	ReadOnly         bool     `yaml:"read_only,omitempty"`
	ReviewIncoming   bool     `yaml:"review_incoming,omitempty"`
	DefaultBranch    string   `yaml:"default_branch,omitempty"`
	FetchRemote      string   `yaml:"fetch_remote,omitempty"`
	PushRemote       string   `yaml:"push_remote,omitempty"`
	RunnerLabels     []string `yaml:"runner_labels,omitempty"`
	AutoForkSync     bool     `yaml:"auto_fork_sync,omitempty"`
	ForkSyncInterval Duration `yaml:"fork_sync_interval,omitempty"`
	AutoSecurity     bool     `yaml:"auto_security,omitempty"`
	SecurityInterval Duration `yaml:"security_interval,omitempty"`
	MergeUpstream    bool     `yaml:"merge_upstream,omitempty"`
}

// Config is the server configuration file.
type Config struct {
	Server    ServerConfig `yaml:"server"`
	Approvals Approvals    `yaml:"approvals"`
	Limits    Limits       `yaml:"limits"`
	Repos     []Repo       `yaml:"repos"`
}

// AgentConfig controls how a runner invokes the coding agent.
type AgentConfig struct {
	Engine          string   `yaml:"engine,omitempty"` // claude | opencode
	Command         string   `yaml:"command"`
	Model           string   `yaml:"model"`
	FallbackEngine  string   `yaml:"fallback_engine,omitempty"`
	FallbackModel   string   `yaml:"fallback_model,omitempty"`
	FallbackOnBusy  *bool    `yaml:"fallback_on_busy,omitempty"`
	MaxBudgetUSD    float64  `yaml:"max_budget_usd"`
	Timeout         Duration `yaml:"timeout"`
	UseCaffeinate   bool     `yaml:"use_caffeinate"`
	AllowedTools    []string `yaml:"allowed_tools,omitempty"`
	DisallowedTools []string `yaml:"disallowed_tools,omitempty"`
	ExtraArgs       []string `yaml:"extra_args,omitempty"`
}

// RunnerRepo maps a logical repo to a checkout on this machine.
type RunnerRepo struct {
	Name          string   `yaml:"name"`
	Path          string   `yaml:"path"`
	Checks        []string `yaml:"checks,omitempty"`
	VerifyTimeout Duration `yaml:"verify_timeout,omitempty"`
	FetchRemote   string   `yaml:"fetch_remote,omitempty"`
	PushRemote    string   `yaml:"push_remote,omitempty"`
	DefaultBranch string   `yaml:"default_branch,omitempty"`
}

// RunnerConfig is the configuration for a runner process.
type RunnerConfig struct {
	ServerURL       string         `yaml:"server_url"`
	Token           string         `yaml:"token,omitempty"`
	ID              string         `yaml:"id,omitempty"`
	Labels          []string       `yaml:"labels,omitempty"`
	WorkRoot        string         `yaml:"work_root"`
	Concurrency     int            `yaml:"concurrency"`
	KeepWorktrees   bool           `yaml:"keep_worktrees,omitempty"`
	ApprovalTimeout Duration       `yaml:"approval_timeout,omitempty"`
	Agent           AgentConfig    `yaml:"agent"`
	Security        SecurityConfig `yaml:"security"`
	Repos           []RunnerRepo   `yaml:"repos"`
}

// SecurityConfig controls the local dependency scanner on a runner.
type SecurityConfig struct {
	Command string   `yaml:"command,omitempty"`
	Timeout Duration `yaml:"timeout,omitempty"`
}

// DefaultPath returns ~/.config/ghfleet/config.yaml.
func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ghfleet", "config.yaml")
}

// DefaultRunnerPath returns ~/.config/ghfleet/runner.yaml.
func DefaultRunnerPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "ghfleet", "runner.yaml")
}

// DefaultStateDir returns ~/.local/share/ghfleet.
func DefaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ghfleet")
}

// DefaultRunnerWorkRoot returns ~/.local/share/ghfleet-runner.
func DefaultRunnerWorkRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "ghfleet-runner")
}

// ExpandPath expands a leading ~ and cleans the result.
func ExpandPath(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return filepath.Clean(p)
}

// Load reads and validates the server config.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(ExpandPath(path))
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills in server defaults.
func (c *Config) ApplyDefaults() { c.applyDefaults() }

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8787"
	}
	if c.Server.StateDir == "" {
		c.Server.StateDir = DefaultStateDir()
	}
	c.Server.StateDir = ExpandPath(c.Server.StateDir)
	if c.Server.PollInterval == 0 {
		c.Server.PollInterval = Duration(2 * time.Minute)
	}
	if c.Server.LogLevel == "" {
		c.Server.LogLevel = "info"
	}
	if c.Approvals.Push == "" {
		c.Approvals.Push = "ask"
	}
	if c.Approvals.Comment == "" {
		c.Approvals.Comment = "ask"
	}
	if c.Approvals.Merge == "" {
		c.Approvals.Merge = "ask"
	}
	if c.Approvals.ForkSync == "" {
		c.Approvals.ForkSync = "auto"
	}
	if c.Approvals.ResolveThreads == "" {
		c.Approvals.ResolveThreads = "bot"
	}
	if c.Limits.MaxJobsPerHour == 0 {
		c.Limits.MaxJobsPerHour = 6
	}
	if c.Limits.MaxFixAttemptsPerPR == 0 {
		c.Limits.MaxFixAttemptsPerPR = 2
	}
	if c.Limits.MaxBudgetUSDPerDay == 0 {
		c.Limits.MaxBudgetUSDPerDay = 10
	}
	if c.Limits.JobTimeout == 0 {
		c.Limits.JobTimeout = Duration(30 * time.Minute)
	}
	if c.Limits.ApprovalTimeout == 0 {
		c.Limits.ApprovalTimeout = Duration(24 * time.Hour)
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		if r.DefaultBranch == "" {
			r.DefaultBranch = "main"
		}
		if r.FetchRemote == "" {
			r.FetchRemote = "origin"
		}
		if r.PushRemote == "" {
			if r.Own {
				r.PushRemote = "origin"
			} else if r.Fork != "" {
				r.PushRemote = "fork"
			} else {
				r.PushRemote = "origin"
			}
		}
		if r.ForkSyncInterval == 0 {
			r.ForkSyncInterval = Duration(24 * time.Hour)
		}
		if r.SecurityInterval == 0 {
			r.SecurityInterval = Duration(24 * time.Hour)
		}
		if r.Own && r.Fork == "" {
			r.Fork = r.Upstream
		}
		if !r.Own && (r.Fork == "" || r.Fork == r.Upstream) {
			// Never let a config that has no writable fork push to upstream.
			r.ReadOnly = true
		}
	}
}

// Validate checks the server config invariants.
func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if r.Name == "" {
			return fmt.Errorf("repos[%d].name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate repo name %q", r.Name)
		}
		seen[r.Name] = true
		if !strings.Contains(r.Upstream, "/") {
			return fmt.Errorf("repo %q: upstream must be owner/name", r.Name)
		}
		if r.Fork != "" && !strings.Contains(r.Fork, "/") {
			return fmt.Errorf("repo %q: fork must be owner/name", r.Name)
		}
	}
	for _, v := range []struct{ name, value string }{
		{"approvals.push", c.Approvals.Push},
		{"approvals.comment", c.Approvals.Comment},
		{"approvals.merge", c.Approvals.Merge},
	} {
		switch v.value {
		case "ask", "auto", "never":
		default:
			return fmt.Errorf("%s must be ask, auto, or never", v.name)
		}
	}
	switch c.Approvals.ForkSync {
	case "ask", "auto":
	default:
		return fmt.Errorf("approvals.fork_sync must be ask or auto")
	}
	switch c.Approvals.ResolveThreads {
	case "never", "bot", "all":
	default:
		return fmt.Errorf("approvals.resolve_threads must be never, bot, or all")
	}
	return nil
}

// Marshal returns YAML with a short header.
func (c *Config) Marshal() ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	header := "# ghfleet server configuration\n# Generated by `ghfleet init`; edit freely.\n"
	return append([]byte(header), body...), nil
}

// LoadRunner reads and validates a runner config.
func LoadRunner(path string) (*RunnerConfig, error) {
	raw, err := os.ReadFile(ExpandPath(path))
	if err != nil {
		return nil, fmt.Errorf("read runner config: %w", err)
	}
	var cfg RunnerConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("parse runner config: %w", err)
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyDefaults fills in runner defaults.
func (c *RunnerConfig) ApplyDefaults() { c.applyDefaults() }

func (c *RunnerConfig) applyDefaults() {
	if c.ServerURL == "" {
		c.ServerURL = "http://127.0.0.1:8787"
	}
	if c.ID == "" {
		host, _ := os.Hostname()
		c.ID = host
	}
	if len(c.Labels) == 0 {
		c.Labels = []string{runtime.GOOS, runtime.GOARCH}
	}
	if c.WorkRoot == "" {
		c.WorkRoot = DefaultRunnerWorkRoot()
	}
	c.WorkRoot = ExpandPath(c.WorkRoot)
	if c.Concurrency == 0 {
		c.Concurrency = 1
	}
	if c.ApprovalTimeout == 0 {
		c.ApprovalTimeout = Duration(24 * time.Hour)
	}
	if c.Agent.Engine == "" {
		switch c.Agent.Command {
		case "", "opencode":
			c.Agent.Engine = "opencode"
		default:
			// Preserve explicit custom/Claude commands from older configs.
			c.Agent.Engine = "claude"
		}
	}
	if c.Agent.Engine == "opencode" {
		if c.Agent.Command == "" || c.Agent.Command == "claude" {
			c.Agent.Command = "opencode"
		}
		if c.Agent.Model == "" {
			c.Agent.Model = "valhalla/valhalla-flash-next"
		}
		if c.Agent.FallbackEngine == "" {
			c.Agent.FallbackEngine = "opencode"
		}
		if c.Agent.FallbackModel == "" {
			c.Agent.FallbackModel = "openrouter/deepseek/deepseek-v4.1-flash-1017"
		}
		if c.Agent.FallbackOnBusy == nil {
			enabled := true
			c.Agent.FallbackOnBusy = &enabled
		}
	} else {
		if c.Agent.Command == "" {
			c.Agent.Command = "claude"
		}
		if c.Agent.Model == "" {
			c.Agent.Model = "sonnet"
		}
	}
	if c.Agent.Timeout == 0 {
		c.Agent.Timeout = Duration(30 * time.Minute)
	}
	if c.Agent.MaxBudgetUSD == 0 {
		c.Agent.MaxBudgetUSD = 2
	}
	if c.Security.Command == "" {
		c.Security.Command = "osv-scanner scan source --recursive --format json ."
	}
	if c.Security.Timeout == 0 {
		c.Security.Timeout = Duration(10 * time.Minute)
	}
	for i := range c.Repos {
		r := &c.Repos[i]
		r.Path = ExpandPath(r.Path)
		if r.VerifyTimeout == 0 {
			r.VerifyTimeout = Duration(15 * time.Minute)
		}
		// Leave remotes and the default branch empty so the server's RepoSpec
		// can supply them (for example, garglk uses "master").

	}
}

// Validate checks the runner config invariants.
func (c *RunnerConfig) Validate() error {
	if c.ServerURL == "" {
		return fmt.Errorf("server_url is required")
	}
	if c.Concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1")
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if r.Name == "" {
			return fmt.Errorf("repos[%d].name is required", i)
		}
		if seen[r.Name] {
			return fmt.Errorf("duplicate runner repo name %q", r.Name)
		}
		seen[r.Name] = true
		if r.Path == "" {
			return fmt.Errorf("repo %q: path is required", r.Name)
		}
		if fi, err := os.Stat(r.Path); err != nil || !fi.IsDir() {
			return fmt.Errorf("repo %q: path %s is not a directory", r.Name, r.Path)
		}
	}
	return nil
}

// Marshal returns YAML with a short header.
func (c *RunnerConfig) Marshal() ([]byte, error) {
	body, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	header := "# ghfleet runner configuration\n# Generated by `ghfleet init`; edit freely.\n"
	return append([]byte(header), body...), nil
}

// RepoByName finds a server repo by name.
func (c *Config) RepoByName(name string) (Repo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return Repo{}, false
}

// RunnerRepoByName finds a runner repo by name.
func (c *RunnerConfig) RunnerRepoByName(name string) (RunnerRepo, bool) {
	for _, r := range c.Repos {
		if r.Name == name {
			return r, true
		}
	}
	return RunnerRepo{}, false
}

// LabelsString returns a comma-separated, sorted label list.
func LabelsString(labels []string) string {
	cp := append([]string(nil), labels...)
	sort.Strings(cp)
	return strings.Join(cp, ",")
}

// SplitSlug splits owner/name.
func SplitSlug(slug string) (owner, name string, err error) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo slug %q, want owner/name", slug)
	}
	return parts[0], parts[1], nil
}
