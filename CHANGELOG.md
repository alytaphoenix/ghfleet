# Changelog

## v0.2.0 - 2026-10-19

- Added GitHub Actions control: list runs, inspect jobs, rerun failed/all jobs, cancel runs, list and dispatch workflows.
- Added `ghfleet review-pr` for on-demand, read-only agent review of any tracked PR.
- Added `security-fix`: daily OSV-Scanner dependency scan on opted-in repos, with approval-gated dependency updates and a silent clean-scan path.
- Added `upstream-merge`: merge the current base into PR branches that GitHub reports as `BEHIND`, with conflict resolution and non-force pushes.
- Added dependency CVE fixes for the scanner/agent toolchain.
- Fixed OpenCode jobs running with the runner's `PWD` instead of the job worktree.
- Hardened agent environments against cloud/registry credentials and denied reads of ghfleet config/state.
- Added CI, SECURITY.md, architecture docs, and release packaging.
