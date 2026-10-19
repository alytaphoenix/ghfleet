# Architecture

ghfleet separates GitHub state and approvals from the machines that own checkouts and toolchains.

```
GitHub <--- poll ---> server (control plane)
                        |  SQLite, policy, job queue, artifacts
                        |  HTTP API (token)
                        +--> CLI / TUI
                        +--> runner (Mac/Linux)
                                |  git worktree
                                |  opencode / claude
                                |  checks + scanner
                                +--> push to configured fork
```

## Components

- `internal/github`: batched GraphQL polling for PRs, review threads, reviews, and checks, plus REST calls for comments, thread replies, and Actions.
- `internal/reconcile`: idempotent job creation from PR state. Keys include the job kind, repo, PR, head SHA, and a state fingerprint so a new commit or review creates new work.
- `internal/store`: SQLite schema for PRs, jobs, leases, reviews, thread handling, artifacts, and runners.
- `internal/server`: HTTP API, approval checks, stale-head checks, and the only GitHub write surface.
- `internal/runner`: claims jobs, creates worktrees, invokes the agent, runs checks, uploads patches, waits for approval, then pushes.
- `internal/agent`: OpenCode/Claude wrappers, per-job permissions, model fallback, and prompt construction.
- `internal/tui`: Bubble Tea inbox for approvals and job state.

## Job lifecycle

```
pending -> claimed -> running -> awaiting_approval -> approved -> applied
                              \-> failed
                              \-> denied
```

A clean security scan and a no-op upstream merge auto-apply because they write nothing. A failed scan or merge is recorded and escalated; a real dependency patch waits for the normal push approval.

## Safety invariants

1. Agents do not receive GitHub credentials; the controller performs writes.
2. A job is bound to the PR head SHA (and base SHA for rebase/merge) and denied if that state moved.
3. Rebase pushes use `--force-with-lease`; fork sync is fast-forward-only; merges and dependency commits are non-force pushes.
4. Only one runner can apply an approved job (lease + runner identity).
5. Read-only repos never enqueue or apply writes.
