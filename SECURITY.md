# Security

ghfleet is a single-user, local-first control plane. It is designed for a trusted operator, trusted runners, and a machine that already has your `gh` login. It is not a multi-tenant service and it is not a sandbox.

## Trust boundaries

- **Server**: holds the GitHub token and is the only component that writes to GitHub (comments, thread replies, pushes go through runners after server approval). Protect it with `server.api_token` on any non-loopback listener and put it behind TLS, WireGuard, or Tailscale. The server refuses to start without a token on a non-loopback address.
- **Runner**: receives job specs and repository paths. It can run git, the configured checks, the configured security scanner, and the agent CLI. A compromised runner can modify its own checkouts and the forks it is configured to push to.
- **Agent**: never receives a GitHub token or SSH agent. It runs in a fresh worktree with a deny-by-default tool configuration, a narrow bash allowlist, secret-bearing environment variables stripped, and no network tools such as `curl`/`wget`/`gh`. It can still execute repository code through the configured verification commands and dependency managers; treat a runner as a trusted machine.
- **Review agents** are read-only. Fix, rebase, merge, and security jobs can edit files; only the controller pushes, and only after approval unless `approvals.push: auto`.

## Defaults

- Push and comment approvals default to `ask`.
- Fork sync is fast-forward-only and never force-pushes.
- Rebases use `git push --force-with-lease` against the expected head.
- Read-only repos never enqueue writes.
- The agent environment removes `GH_*`, `GITHUB_TOKEN`, `SSH_AUTH_SOCK`, cloud credentials, registry tokens, and database passwords. Provider API keys may remain because the agent is the model client.
- Agent file reads deny `~/.ssh`, `~/.aws`, `~/.config/gh`, `~/.config/ghfleet`, `~/.docker`, `~/.kube`, `~/.gnupg`, `.env`, `.netrc`, and common cloud credential paths.
- Artifact names are validated against path traversal.

## Operational guidance

- Use a fine-grained GitHub token scoped to the repositories you manage.
- Run runners under a dedicated OS user when possible.
- Do not enable `approvals.push: auto` until a job type has a clean track record.
- Review `security-fix` patches like any other dependency change; `osv-scanner` is advisory, not a proof of exploitability.
- Back up the SQLite state directory and keep it out of version control.

## Reporting

Please report suspected vulnerabilities privately through GitHub's "Report a vulnerability" advisory flow on the repository, not in a public issue. Include the version, configuration shape (redact tokens), and a minimal reproduction.
