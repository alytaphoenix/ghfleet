// Package agent invokes a headless coding agent inside a runner worktree.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/jobs"
)

//go:embed prompts/*.md schemas/*.json
var assets embed.FS

// Runner invokes the configured agent CLI.
type Runner struct {
	Cfg config.AgentConfig
	Log *slog.Logger
}

// Request is one agent invocation.
type Request struct {
	Kind          string
	Workdir       string
	Spec          jobs.Spec
	Diff          string
	Guidelines    string
	ChecksOutput  string
	CheckCommands []string
}

// Result is the parsed agent result.
type Result struct {
	JSON      json.RawMessage
	Text      string
	CostUSD   float64
	Duration  time.Duration
	Turns     int
	SessionID string
	Engine    string
	Model     string
}

type envelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Result           json.RawMessage `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	IsError          bool            `json:"is_error"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int64           `json:"duration_ms"`
	NumTurns         int             `json:"num_turns"`
	SessionID        string          `json:"session_id"`
}

// Run executes the primary engine, falling back to a secondary model when the
// primary is busy or unavailable.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	engine := r.Cfg.Engine
	if engine == "" {
		engine = "claude"
	}
	command := r.Cfg.Command
	if command == "" {
		if engine == "opencode" {
			command = "opencode"
		} else {
			command = "claude"
		}
	}
	model := r.Cfg.Model
	if model == "" {
		if engine == "opencode" {
			model = "valhalla/valhalla-flash-next"
		} else {
			model = "sonnet"
		}
	}

	prompt, err := buildPrompt(req)
	if err != nil {
		return nil, err
	}
	schema, err := schemaFor(req.Kind)
	if err != nil {
		return nil, err
	}

	res, err := r.runEngine(ctx, engine, command, model, prompt, schema, req)
	if err == nil {
		return res, nil
	}
	if !r.fallbackEnabled() || r.Cfg.FallbackModel == "" || !isBusy(err) {
		return nil, err
	}

	fbEngine := r.Cfg.FallbackEngine
	if fbEngine == "" {
		fbEngine = engine
	}
	fbCommand := command
	if fbEngine != engine {
		if fbEngine == "opencode" {
			fbCommand = "opencode"
		} else {
			fbCommand = "claude"
		}
	}
	if r.Log != nil {
		r.Log.Warn("primary agent busy; using fallback", "engine", engine, "model", model,
			"fallback_engine", fbEngine, "fallback_model", r.Cfg.FallbackModel, "err", err)
	}
	return r.runEngine(ctx, fbEngine, fbCommand, r.Cfg.FallbackModel, prompt, schema, req)
}

func (r *Runner) fallbackEnabled() bool {
	return r.Cfg.FallbackOnBusy != nil && *r.Cfg.FallbackOnBusy
}

func (r *Runner) runEngine(ctx context.Context, engine, command, model, prompt, schema string, req Request) (*Result, error) {
	switch engine {
	case "opencode":
		return r.runOpenCode(ctx, command, model, prompt, schema, req)
	default:
		return r.runClaude(ctx, command, model, prompt, schema, req)
	}
}

func (r *Runner) runClaude(ctx context.Context, command, model, prompt, schema string, req Request) (*Result, error) {
	timeout := r.Cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"-p",
		"--output-format", "json",
		"--json-schema", schema,
		"--model", model,
		"--permission-mode", "acceptEdits",
		"--no-session-persistence",
	}
	if tools := allowedTools(r.Cfg, req); tools != "" {
		args = append(args, "--allowedTools", tools)
	}
	if denied := deniedTools(r.Cfg); denied != "" {
		args = append(args, "--disallowedTools", denied)
	}
	if r.Cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", r.Cfg.MaxBudgetUSD))
	}
	if settings := agentSettings(); settings != "" {
		args = append(args, "--settings", settings)
	}
	args = append(args, r.Cfg.ExtraArgs...)

	cmdName := command
	cmdArgs := args
	if r.Cfg.UseCaffeinate && runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("caffeinate"); err == nil {
			cmdName = "caffeinate"
			cmdArgs = append([]string{"-i", command}, args...)
		}
	}

	cmd := exec.CommandContext(runCtx, cmdName, cmdArgs...)
	cmd.Dir = req.Workdir
	cmd.Env = append(sanitizedEnv(), "PWD="+req.Workdir)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("agent timed out after %s", timeout)
	}
	var env envelope
	parsed := json.Unmarshal(stdout.Bytes(), &env) == nil
	if err != nil {
		// Hooks or wrappers can change the exit code after a successful run.
		// Keep valid structured output; otherwise surface stdout and stderr.
		if parsed && !env.IsError && (len(env.StructuredOutput) > 0 || len(env.Result) > 0) {
			return &Result{
				JSON: firstNonEmptyRaw(env.StructuredOutput, env.Result), Text: string(env.Result),
				CostUSD: env.TotalCostUSD, Duration: elapsed, Turns: env.NumTurns,
				SessionID: env.SessionID, Engine: "claude", Model: model,
			}, nil
		}
		if parsed && env.IsError {
			return nil, fmt.Errorf("agent reported an error after %s: %s", elapsed.Round(time.Second), tail(string(env.Result), 2000))
		}
		return nil, fmt.Errorf("agent failed after %s: %w: stdout=%s stderr=%s",
			elapsed.Round(time.Second), err, tail(stdout.String(), 2000), tail(stderr.String(), 2000))
	}
	if !parsed {
		// Some agent versions emit text even in json mode.
		return &Result{JSON: fallbackJSON("agent returned non-JSON output", stdout.String()), Text: stdout.String(), Duration: elapsed, Engine: "claude", Model: model}, nil
	}
	if env.IsError {
		return nil, fmt.Errorf("agent reported an error: %s", tail(string(env.Result), 2000))
	}
	out := normalize(env.StructuredOutput)
	if len(out) == 0 {
		out = normalize(env.Result)
	}
	if len(out) == 0 {
		out = fallbackJSON("agent returned no structured result", string(env.Result))
	}
	return &Result{
		JSON: out, Text: string(env.Result), CostUSD: env.TotalCostUSD, Duration: elapsed,
		Turns: env.NumTurns, SessionID: env.SessionID, Engine: "claude", Model: model,
	}, nil
}

func (r *Runner) runOpenCode(ctx context.Context, command, model, prompt, schema string, req Request) (*Result, error) {
	timeout := r.Cfg.Timeout.D()
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt += "\n\nReturn only a JSON object matching this JSON Schema:\n```json\n" + schema + "\n```\n"
	args := []string{"run", "--standalone", "--format", "json", "--model", model, "--auto"}
	args = append(args, r.Cfg.ExtraArgs...)

	cmdName := command
	cmdArgs := args
	if r.Cfg.UseCaffeinate && runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("caffeinate"); err == nil {
			cmdName = "caffeinate"
			cmdArgs = append([]string{"-i", command}, args...)
		}
	}

	cmd := exec.CommandContext(runCtx, cmdName, cmdArgs...)
	cmd.Dir = req.Workdir
	cmd.Env = append(sanitizedEnv(), "PWD="+req.Workdir, "OPENCODE_CONFIG_CONTENT="+opencodeConfig(req))
	cmd.Stdin = strings.NewReader(prompt)
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)
	if runCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("opencode timed out after %s", timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("opencode failed after %s: %w: stdout=%s stderr=%s",
			elapsed.Round(time.Second), err, tail(stdout.String(), 3000), tail(stderr.String(), 3000))
	}
	raw, text, turns, err := parseOpenCodeOutput(stdout.Bytes())
	if err != nil {
		return nil, fmt.Errorf("opencode: %w: stdout=%s stderr=%s", err, tail(stdout.String(), 3000), tail(stderr.String(), 3000))
	}
	return &Result{JSON: raw, Text: text, Duration: elapsed, Turns: turns, Engine: "opencode", Model: model}, nil
}

type ocEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseOpenCodeOutput(raw []byte) (json.RawMessage, string, int, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var texts []string
	turns := 0
	var errMsg string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev ocEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text != "" {
				texts = append(texts, ev.Part.Text)
			}
		case "step_start":
			turns++
		case "error":
			errMsg = ev.Error.Message
		}
	}
	text := strings.Join(texts, "")
	if text == "" {
		if errMsg != "" {
			return nil, "", turns, fmt.Errorf("provider error: %s", errMsg)
		}
		return nil, "", turns, fmt.Errorf("no text output")
	}
	js := extractJSON(text)
	if js == nil {
		return nil, text, turns, fmt.Errorf("model did not return JSON")
	}
	return js, text, turns, nil
}

func extractJSON(text string) json.RawMessage {
	text = strings.TrimSpace(text)
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	start := strings.Index(text, "{")
	for start >= 0 {
		end := strings.LastIndex(text, "}")
		for end > start {
			candidate := text[start : end+1]
			if json.Valid([]byte(candidate)) {
				return json.RawMessage(candidate)
			}
			end = strings.LastIndex(text[:end], "}")
		}
		next := strings.Index(text[start+1:], "{")
		if next < 0 {
			break
		}
		start += next + 1
	}
	return nil
}

func isBusy(err error) bool {
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"429", "502", "503", "504", "busy", "overloaded", "rate limit",
		"too many requests", "connection refused", "econnrefused", "no available",
		"temporarily unavailable", "timed out", "timeout", "provider error",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

func opencodeConfig(req Request) string {
	editable := req.Kind != jobs.KindPreflight && req.Kind != jobs.KindIncomingReview
	edit := "deny"
	if editable {
		edit = "allow"
	}
	bash := map[string]any{
		"*":               "deny",
		"pwd*":            "allow",
		"ls*":             "allow",
		"git status*":     "allow",
		"git diff*":       "allow",
		"git log*":        "allow",
		"git show*":       "allow",
		"git branch*":     "allow",
		"git rev-parse*":  "allow",
		"git merge-base*": "allow",
		"git cat-file*":   "allow",
	}
	if editable {
		bash["git add*"] = "allow"
		bash["git restore*"] = "allow"
	}
	if req.Kind == jobs.KindRebase {
		bash["git rebase*"] = "allow"
		bash["git checkout*"] = "allow"
	}
	if req.Kind == jobs.KindSecurity {
		for _, cmd := range []string{
			"cargo update*", "cargo add*", "go get*", "go mod tidy*",
			"uv lock*", "uv sync*", "uv add*", "npm update*", "npm install*",
			"npm audit fix*", "bun update*", "bun install*", "bun add*",
			"pip install*", "python -m pip install*", "yarn up*", "pnpm update*",
			"osv-scanner*",
		} {
			bash[cmd] = "allow"
		}
	}
	for _, cmd := range req.CheckCommands {
		if p := bashPrefix(cmd); p != "" {
			bash[p+"*"] = "allow"
		}
	}
	read := map[string]any{
		"*":                          "allow",
		"*.env":                      "deny",
		"*.env.*":                    "deny",
		"**/.ssh/**":                 "deny",
		"**/.aws/**":                 "deny",
		"**/.config/gh/**":           "deny",
		"**/.config/ghfleet/**":      "deny",
		"**/.local/share/ghfleet/**": "deny",
		"**/.docker/**":              "deny",
		"**/.kube/**":                "deny",
		"**/.gnupg/**":               "deny",
		"**/proc/**":                 "deny",
		"**/sys/**":                  "deny",
		"**/dev/**":                  "deny",
	}
	perm := map[string]any{
		"*":                  "deny",
		"read":               read,
		"edit":               edit,
		"glob":               "allow",
		"grep":               "allow",
		"bash":               bash,
		"webfetch":           "deny",
		"websearch":          "deny",
		"task":               "deny",
		"skill":              "deny",
		"lsp":                "deny",
		"question":           "deny",
		"external_directory": "deny",
		"doom_loop":          "deny",
	}
	cfg := map[string]any{
		"$schema":    "https://opencode.ai/config.json",
		"permission": perm,
		"snapshot":   false,
		"autoupdate": false,
		"share":      "disabled",
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

func firstNonEmptyRaw(values ...json.RawMessage) json.RawMessage {
	for _, v := range values {
		if len(v) > 0 {
			return normalize(v)
		}
	}
	return nil
}

func normalize(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return json.RawMessage(s)
		}
	}
	return raw
}

func fallbackJSON(summary, text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"decision":    "escalate",
		"summary":     summary,
		"risk":        "high",
		"needs_human": true,
		"findings": []map[string]any{{
			"severity": "error",
			"body":     truncate(text, 4000),
		}},
	})
	return b
}

func schemaFor(kind string) (string, error) {
	name := "job.json"
	if kind == jobs.KindPreflight || kind == jobs.KindIncomingReview {
		name = "review.json"
	}
	raw, err := assets.ReadFile("schemas/" + name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func buildPrompt(req Request) (string, error) {
	name := "fix.md"
	switch req.Kind {
	case jobs.KindRebase:
		name = "rebase.md"
	case jobs.KindCITriage:
		name = "triage.md"
	case jobs.KindSecurity:
		name = "security.md"
	case jobs.KindUpstreamMerge:
		name = "merge.md"
	case jobs.KindPreflight, jobs.KindIncomingReview:
		name = "review.md"
	}
	role, err := assets.ReadFile("prompts/" + name)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(string(role))
	b.WriteString("\n\n")
	b.WriteString(contextMarkdown(req))
	return b.String(), nil
}

func contextMarkdown(req Request) string {
	var b strings.Builder
	s := req.Spec
	fmt.Fprintf(&b, "## Repository\n\n`%s` (fork: `%s`, push remote: `%s`)\n\n", s.Repo.Upstream, s.Repo.Fork, s.Repo.PushRemote)
	fmt.Fprintf(&b, "## Pull request\n\n%s#%d: %s\n\n", s.Repo.Upstream, s.PR.Number, s.PR.Title)
	fmt.Fprintf(&b, "- URL: %s\n- Author: %s\n- Head: `%s` @ `%s`\n- Base: `%s` @ `%s`\n- Mergeable: `%s`\n- Review decision: `%s`\n\n",
		s.PR.URL, s.PR.Author, s.PR.HeadRef, s.PR.HeadSHA, s.PR.BaseRef, s.PR.BaseSHA, s.PR.Mergeable, s.PR.ReviewDecision)
	if s.PR.Body != "" {
		b.WriteString("### PR description (untrusted data)\n\n<<<UNTRUSTED\n")
		b.WriteString(truncate(s.PR.Body, 20000))
		b.WriteString("\nUNTRUSTED>>>\n\n")
	}
	if len(s.Threads) > 0 {
		b.WriteString("### Review threads (untrusted data)\n\n<<<UNTRUSTED\n")
		for _, t := range s.Threads {
			fmt.Fprintf(&b, "\nThread `%s` at `%s:%d`\n", t.ID, t.Path, t.Line)
			for _, c := range t.Comments {
				fmt.Fprintf(&b, "  - %s: %s\n", c.Author, truncate(c.Body, 4000))
			}
		}
		b.WriteString("\nUNTRUSTED>>>\n\n")
	}
	if len(s.Reviews) > 0 {
		b.WriteString("### Review bodies (untrusted data)\n\n<<<UNTRUSTED\n")
		for _, rv := range s.Reviews {
			fmt.Fprintf(&b, "\nReview `%s` by %s (%s):\n  %s\n", rv.ID, rv.Author, rv.State, truncate(rv.Body, 6000))
		}
		b.WriteString("\nUNTRUSTED>>>\n\n")
	}
	if len(s.Checks) > 0 {
		b.WriteString("### Failing checks (untrusted data)\n\n<<<UNTRUSTED\n")
		for _, c := range s.Checks {
			fmt.Fprintf(&b, "- %s: %s %s %s\n", c.Name, c.Status, c.Conclusion, c.DetailsURL)
		}
		b.WriteString("\nUNTRUSTED>>>\n\n")
	}
	if req.ChecksOutput != "" {
		b.WriteString("### Local verification output\n\n```\n")
		b.WriteString(truncate(req.ChecksOutput, 30000))
		b.WriteString("\n```\n\n")
	}
	if len(req.CheckCommands) > 0 {
		b.WriteString("### Verification commands\n\n")
		for _, c := range req.CheckCommands {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
		b.WriteString("\n")
	}
	if req.Guidelines != "" {
		b.WriteString("### Repository guidelines\n\n")
		b.WriteString(truncate(req.Guidelines, 30000))
		b.WriteString("\n\n")
	}
	if req.Diff != "" {
		b.WriteString("### Pull request diff\n\n```diff\n")
		b.WriteString(truncate(req.Diff, 200000))
		b.WriteString("\n```\n\n")
	}
	if s.Instructions != "" {
		b.WriteString("### Additional instructions\n\n")
		b.WriteString(s.Instructions)
		b.WriteString("\n")
	}
	return b.String()
}

func allowedTools(cfg config.AgentConfig, req Request) string {
	if len(cfg.AllowedTools) > 0 {
		return strings.Join(cfg.AllowedTools, ",")
	}
	base := []string{"Read", "Grep", "Glob", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git log:*)", "Bash(git show:*)"}
	switch req.Kind {
	case jobs.KindPreflight, jobs.KindIncomingReview:
		return strings.Join(base, ",")
	case jobs.KindRebase:
		base = append(base, "Edit", "Write", "Bash(git add:*)", "Bash(git rebase:*)", "Bash(git checkout:*)", "Bash(git restore:*)")
	case jobs.KindSecurity:
		base = append(base, "Edit", "Write", "Bash(git add:*)",
			"Bash(cargo update:*)", "Bash(cargo add:*)",
			"Bash(go get:*)", "Bash(go mod tidy:*)",
			"Bash(uv lock:*)", "Bash(uv sync:*)", "Bash(uv add:*)",
			"Bash(npm update:*)", "Bash(npm install:*)", "Bash(npm audit fix:*)",
			"Bash(bun update:*)", "Bash(bun install:*)", "Bash(bun add:*)",
			"Bash(pip install:*)", "Bash(python -m pip install:*)",
			"Bash(yarn up:*)", "Bash(pnpm update:*)",
			"Bash(osv-scanner:*)")
	default:
		base = append(base, "Edit", "Write", "Bash(git add:*)")
	}
	for _, cmd := range req.CheckCommands {
		if p := bashPrefix(cmd); p != "" {
			base = append(base, "Bash("+p+":*)")
		}
	}
	return dedupeJoin(base)
}

func deniedTools(cfg config.AgentConfig) string {
	base := []string{
		"WebFetch", "WebSearch",
		"Bash(gh:*)", "Bash(curl:*)", "Bash(wget:*)", "Bash(ssh:*)", "Bash(scp:*)",
		"Bash(git push:*)", "Bash(git remote:*)", "Bash(git fetch:*)",
		"Bash(env:*)", "Bash(printenv:*)", "Bash(security:*)",
		"Bash(cat:*)", "Bash(head:*)", "Bash(tail:*)", "Bash(less:*)", "Bash(more:*)",
		"Bash(find:*)", "Bash(rg:*)", "Bash(grep:*)",
	}
	base = append(base, cfg.DisallowedTools...)
	return dedupeJoin(base)
}

func bashPrefix(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	if len(fields) == 1 {
		return fields[0]
	}
	return fields[0] + " " + fields[1]
}

func dedupeJoin(items []string) string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return strings.Join(out, ",")
}

func agentSettings() string {
	deny := []string{
		"Read(//Users/**/.ssh/**)",
		"Read(//Users/**/.aws/**)",
		"Read(//Users/**/.config/gh/**)",
		"Read(//Users/**/.config/ghfleet/**)",
		"Read(//Users/**/.local/share/ghfleet/**)",
		"Read(//Users/**/.docker/**)",
		"Read(//Users/**/.kube/**)",
		"Read(//Users/**/.gnupg/**)",
		"Read(//home/**/.ssh/**)",
		"Read(//home/**/.aws/**)",
		"Read(//home/**/.config/gh/**)",
		"Read(//home/**/.config/ghfleet/**)",
		"Read(//home/**/.local/share/ghfleet/**)",
		"Read(//home/**/.docker/**)",
		"Read(//home/**/.kube/**)",
		"Read(//home/**/.gnupg/**)",
		"Read(//proc/**)",
		"Read(//sys/**)",
		"Read(//dev/**)",
		"Read(//**/.env)",
		"Read(//**/.env.*)",
		"Read(//**/.netrc)",
		"Read(//**/.npmrc)",
		"Read(//**/.pypirc)",
		"Read(//**/.config/gcloud/**)",
		"Read(//**/.netrc)",
		"Read(//**/.npmrc)",
		"Read(//**/.pypirc)",
	}
	b, _ := json.Marshal(map[string]any{"permissions": map[string]any{"deny": deny}})
	return string(b)
}

// sensitiveEnvKey keeps cloud and package-registry credentials out of the
// agent environment. Provider API keys are intentionally left in place: the
// agent is the provider client and may need them to reach the model.
func sensitiveEnvKey(key string) bool {
	switch key {
	case "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AZURE_CLIENT_SECRET", "AZURE_TENANT_ID", "GOOGLE_APPLICATION_CREDENTIALS",
		"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN", "NPM_TOKEN",
		"CARGO_REGISTRY_TOKEN", "PYPI_TOKEN", "DATABASE_URL",
		"PGPASSWORD", "MYSQL_PWD", "DOCKER_AUTH_CONFIG",
		"CI_JOB_TOKEN", "GITLAB_TOKEN", "SENTRY_AUTH_TOKEN":
		return true
	}
	for _, prefix := range []string{
		"AWS_", "AZURE_", "GCP_", "GOOGLE_", "SLACK_", "STRIPE_", "SENTRY_",
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func sanitizedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		switch {
		case key == "GITHUB_TOKEN", key == "GH_TOKEN", key == "GHFLEET_GH_TOKEN":
			continue
		case strings.HasPrefix(key, "GH_"):
			continue
		case key == "SSH_AUTH_SOCK", key == "GIT_ASKPASS", key == "GIT_SSH_COMMAND":
			continue
		case sensitiveEnvKey(key):
			continue
		}
		out = append(out, kv)
	}
	out = append(out,
		"GHFLEET_AGENT=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_MERGE_AUTOEDIT=no",
	)
	return out
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n\n[truncated]"
}
