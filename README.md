# ghfleet

Local-first GitHub PR control plane. One server owns GitHub state and approvals; one or more runners own checkouts, agents, tests, and pushes; the Mac/Linux client is a CLI or TUI.

No GitHub Actions minutes, no webhook endpoint, no SaaS. It polls the GitHub API with your existing `gh` login and stores state in SQLite.

```
GitHub  <---poll--->  ghfleet server (control plane)
                        ├─ policy / reconciler
                        ├─ job queue + approvals
                        ├─ SQLite + artifact store
                        └─ HTTP API
                              ├── ghfleet CLI / TUI   (Mac or Linux)
                              └── ghfleet runner      (Mac and/or Linux)
                                    ├── git worktree
                                    ├── opencode (Valhalla) or claude -p
                                    ├── project checks
                                    └── git push to your fork
```

## Why this shape

Your PRs live in a handful of upstream repos, while the code and toolchains live on your machines. A single laptop daemon is fragile, and GitHub Actions is the wrong place for agents that need your local Metal/CUDA/Xcode setup. Splitting the control plane from the runners lets the server run on a Linux box and jobs land on whichever machine can actually run them.

The server never gives an agent a GitHub token. The agent edits a worktree; the controller verifies, asks for approval, and only then pushes or comments.

## What it automates

| Job | Trigger | What happens |
|---|---|---|
| `review-response` | new review thread from someone else | agent patches the branch, runs checks, waits for approval, pushes to your fork, replies |
| `ci-triage` | failing check on your PR | agent diagnoses and fixes deterministic failures; flaky/infra failures escalate |
| `rebase` | `mergeable: CONFLICTING` | runner rebases onto the current base, resolves mechanical conflicts, waits for approval, force-pushes with lease |
| `fork-sync` | daily, per fork | fast-forwards the fork's default branch from upstream; never force-pushes; diverged forks escalate |
| `upstream-merge` | PR reports `BEHIND` | merges the current base into your PR branch (no history rewrite), resolves conflicts, runs checks, waits for approval, pushes |
| `security-fix` | daily, opt-in per repo | runs `osv-scanner` on the default branch; if CVEs are found, updates only the affected dependencies, runs checks, waits for approval, commits to the default branch |
| `incoming-review` | opt-in, PRs in your own repos | read-only agent review; posts a findings comment after approval; never edits or pushes |
| `incoming-review` (manual) | `ghfleet review-pr repo N` | same read-only review, for any tracked PR, not just your own repos |
| `preflight` | manual | local review of the current diff; no writes |

Approval-first is the default. The agent can run and prepare a patch automatically; pushing and commenting require `ghfleet approve <job>` (or `a` in the TUI). Once a job type has a clean track record, set `approvals.push: auto` to let verified, low-risk results apply without a prompt. `fork-sync` is the exception when configured as `auto` because it is fast-forward-only.

## Quick start (single machine)

```bash
# prerequisites: git, gh (logged in), OpenCode with Valhalla/OpenRouter providers
gh auth status
opencode models | head

go build -o bin/ghfleet ./cmd/ghfleet

# discover ~/repos and write ~/.config/ghfleet/{config,runner}.yaml
./bin/ghfleet init

# read-only check
./bin/ghfleet doctor
./bin/ghfleet scan --local

# terminal 1: control plane
./bin/ghfleet server

# terminal 2: runner on this machine
./bin/ghfleet runner

# terminal 3: client
./bin/ghfleet status
./bin/ghfleet jobs
./bin/ghfleet tui
```

`ghfleet init` writes a server config and a runner config. Edit `~/.config/ghfleet/runner.yaml` to set checks, labels, and the agent command. The generated check commands are conservative guesses: narrow them to the suite you actually want an agent to run before pushing.

## Split server + runners

On the Linux host:

```bash
# If the host has checkouts:
ghfleet init --skip-runner --repos-root /srv/repos
# Otherwise generate the config on your Mac and copy ~/.config/ghfleet/config.yaml
# to /etc/ghfleet/config.yaml, then change listen/state_dir/api_token.

# edit config.yaml; set runner_labels on repos if needed
ghfleet server
```

On each Mac/Linux runner:

```bash
ghfleet init --skip-server --server-url http://linux-host:8787
# edit ~/.config/ghfleet/runner.yaml: local paths, labels, checks
ghfleet runner
```

On the Mac client:

```bash
ghfleet --server http://linux-host:8787 status
ghfleet --server http://linux-host:8787 tui
```

Set `GHFLEET_SERVER` and `GHFLEET_TOKEN` (or the runner config `token`) to avoid repeating flags. For a non-loopback server, set `server.api_token` and use the same token on clients/runners; put it behind TLS (reverse proxy or WireGuard/Tailscale) for anything beyond a trusted LAN.

Runner labels route work by platform:

```yaml
# server config
repos:
  - name: omlx
    upstream: jundot/omlx
    fork: alytaphoenix/omlx
    runner_labels: [darwin, metal]
```

A Linux runner with `labels: [linux, cuda]` will not claim that job. See `deploy/README.md` for systemd and launchd examples.

## Commands

```
ghfleet init            discover local checkouts and write configs
ghfleet doctor          check auth, remotes, agent, server
ghfleet scan            poll GitHub and enqueue work (--local for one-shot)
ghfleet status          dashboard counts
ghfleet prs             tracked PRs, with conflict/CI/review state
ghfleet jobs | inbox    approval inbox
ghfleet show <job>      job result + patch
ghfleet review          local preflight review of the current diff (no writes)
ghfleet approve <job>   allow the runner to push/comment
ghfleet deny <job>      reject and mark its threads handled
ghfleet patch <job>     raw patch
ghfleet runners         registered runners
ghfleet tui             terminal client
ghfleet server          control plane (--once for one reconcile pass)
ghfleet runner          runner for this machine
ghfleet sync [repo]     scan, or enqueue a manual fork sync for one repo
ghfleet security <repo> enqueue a dependency CVE scan/fix for one repo
ghfleet actions [repo]   list recent GitHub Actions runs
ghfleet actions jobs <repo> <run-id>       jobs and steps for one run
ghfleet actions rerun <repo> <run-id>      rerun failed jobs (--all for every job)
ghfleet actions cancel <repo> <run-id>     cancel a queued/running run
ghfleet actions dispatch <repo> <workflow> --ref main --input k=v
ghfleet review-pr <repo> <number>          enqueue a read-only agent PR review
```

## Job lifecycle

```
pending -> claimed -> running -> awaiting_approval -> approved -> applying -> applied
                              \-> failed (retried up to max_fix_attempts_per_pr)
                              \-> denied
```

A job is idempotent per `(kind, repo, PR, head SHA, extra)`. A new commit creates a new job; a new review thread changes the thread fingerprint. Applied jobs keep their key so a scan does not re-do them. Failed/denied jobs release the key so a bounded retry can run.

## Configuration

Server (`~/.config/ghfleet/config.yaml`):

```yaml
server:
  listen: 127.0.0.1:8787
  state_dir: ~/.local/share/ghfleet
  poll_interval: 2m
  viewer: your-login
approvals:
  push: ask          # ask | auto | never
  comment: ask
  merge: ask
  fork_sync: auto
  resolve_threads: bot   # never | bot | all
limits:
  max_jobs_per_hour: 6
  max_fix_attempts_per_pr: 2
  max_budget_usd_per_day: 10
  job_timeout: 30m
  approval_timeout: 24h
repos:
  - name: omlx
    upstream: jundot/omlx
    fork: alytaphoenix/omlx
    default_branch: main
    fetch_remote: origin
    push_remote: fork
    auto_fork_sync: true
    # auto_security: true     # owned repos only; requires osv-scanner on the runner
    security_interval: 24h
    merge_upstream: true      # merge the base branch into PRs that are BEHIND
```

Set `review_incoming: true` on a repo you own to review contributor PRs automatically. `ghfleet review-pr <repo> <number>` reviews any tracked PR on demand, including PRs in upstream projects; the findings comment still waits for `approvals.comment`.

## Actions, CI/CD, and PR review

ghfleet already reacts to failing checks with `ci-triage` and to review feedback with `review-response`. The Actions console adds the human controls:

```bash
ghfleet actions                      # recent runs across configured repos
ghfleet actions omlx                 # one repo
ghfleet actions jobs omlx 123456789  # jobs/steps for a run
ghfleet actions rerun omlx 123456789 # failed jobs only
ghfleet actions rerun omlx 123456789 --all
ghfleet actions cancel omlx 123456789
ghfleet actions workflows omlx
ghfleet actions dispatch omlx release.yml --ref main --input dry_run=false
```

Rerun, cancel, and dispatch are direct GitHub writes from the server, gated by the API token (and refused for `read_only` repos). They are for deliberate human actions; they do not need a job approval. Agent-driven changes, including edits to `.github/workflows` made while fixing CI, still go through the normal patch approval and push flow.

Manual review:

```bash
ghfleet scan                         # make sure the PR is in the local store
ghfleet review-pr omlx 3141
ghfleet jobs                         # find the incoming-review job
ghfleet show <job>                   # findings
ghfleet approve <job>                # post the findings comment
```

`review-pr` runs a read-only review; it never edits or pushes. Set `runner_labels: [darwin]` (or `[linux]`) to pin a repo to a platform.

Runner (`~/.config/ghfleet/runner.yaml`):

```yaml
server_url: http://127.0.0.1:8787
id: mac-studio
labels: [darwin, arm64]
work_root: ~/.local/share/ghfleet-runner
concurrency: 1
security:
  command: osv-scanner scan source --recursive --format json .
  timeout: 10m
agent:
  engine: opencode
  command: opencode
  model: valhalla/valhalla-flash-next
  fallback_engine: opencode
  fallback_model: openrouter/deepseek/deepseek-v4.1-flash-1017
  fallback_on_busy: true
  max_budget_usd: 2
  timeout: 30m
  use_caffeinate: true
repos:
  - name: omlx
    path: ~/repos/omlx
    checks: ["uv run pytest -q"]
    fetch_remote: origin
    push_remote: fork
  - name: candle
    path: ~/repos/candle
    checks: ["cargo check"]
    fetch_remote: upstream
    push_remote: origin
```

`security-fix` needs `osv-scanner` on the runner (`go install github.com/google/osv-scanner/v2/cmd/osv-scanner@latest`). Set `auto_security: true` only on repos you own, or on forks where you deliberately want dependency updates on the fork's default branch. A clean scan is silent; a failed scan is recorded as a job failure.

`push_remote` must never point at an upstream you do not own. The runner only pushes to the configured remote and branch; rebases use `--force-with-lease`. For a read-only clone, set `read_only: true` on the server repo; the reconciler will not enqueue work for it.

## Agent safety

- The agent runs in a fresh `git worktree`, not your dirty checkout.
- `GH_*`, `GITHUB_TOKEN`, `SSH_AUTH_SOCK`, `GIT_ASKPASS`, and `GIT_SSH_COMMAND` are removed from the agent's environment.
- The agent is denied `gh`, `curl`, `wget`, `ssh`, `scp`, `git push`, `git fetch`, `git remote`, `env`, and `security`.
- `--settings` denies reads of `~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.docker`, `~/.kube`, and `~/.gnupg`.
- Tests run with a sanitized environment too.
- Prompt context wraps PR bodies, review comments, and CI logs in explicit untrusted-data markers.
- The controller, not the agent, does all GitHub writes.
- The default engine is OpenCode on Valhalla (`valhalla/valhalla-flash-next`). If Valhalla returns 429/503, is unreachable, or times out, the runner retries once with `openrouter/deepseek/deepseek-v4.1-flash-1017`.
- OpenCode gets a per-job `OPENCODE_CONFIG_CONTENT` with deny-by-default permissions and a narrow bash allowlist; review-only jobs cannot edit. Claude remains available with `engine: claude`.
- Approve-first is the default. Auto-approval should wait until a job type has a clean track record.
- The runner is not a security sandbox. Agents can run the repository's configured checks and dependency-manager commands, and those commands execute repository code. Run ghfleet on a machine/account you trust, and use a dedicated OS user or container for untrusted repositories. See `SECURITY.md`.

## Development

```bash
make build
make test
make race
```

Layout:

```
cmd/ghfleet          CLI entry point
internal/config      server + runner config
internal/discover    local checkout discovery and config generation
internal/github      batched GraphQL polling
internal/store       SQLite schema and state
internal/reconcile   policy -> jobs
internal/server      HTTP API, scheduler, approvals
internal/runner      claim -> worktree -> agent -> verify -> wait -> push
internal/agent       headless agent invocation
internal/tui         Bubble Tea client
```

The integration tests create local bare remotes and run a real runner against an `httptest` server. They cover fork fast-forward, the full review-fix-approve-push path, rebase conflict resolution with a force-push lease, and an incoming review that must never push to the contributor's branch.

## Not built yet

- Auto-merge with a trust budget.
- Webhook mode (polling only).
- A GitHub App identity (uses your `gh` token).
- Patch-stack-aware rebases beyond a single branch.
