# Deploying ghfleet

The control plane does not need a checkout of every repo; it only needs the GitHub metadata in `config.yaml`. The runners need local checkouts.

## Linux control plane

```bash
sudo useradd --system --home /var/lib/ghfleet --shell /usr/sbin/nologin ghfleet
sudo mkdir -p /etc/ghfleet /var/lib/ghfleet
sudo install -m 0755 ghfleet /usr/local/bin/ghfleet
sudo install -m 0644 deploy/systemd/ghfleet.service /etc/systemd/system/ghfleet.service
sudo install -m 0600 /dev/null /etc/ghfleet/env
```

Put the config at `/etc/ghfleet/config.yaml`. If you generated it on your Mac with `ghfleet init`, copy `~/.config/ghfleet/config.yaml` over and change:

```yaml
server:
  listen: 0.0.0.0:8787
  api_token: "a-long-random-string"
  state_dir: /var/lib/ghfleet
```

Put a token in `/etc/ghfleet/env`:

```bash
GH_TOKEN=github_pat_...
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ghfleet
sudo systemctl status ghfleet
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8787/api/v1/status
```

Put the listener behind WireGuard/Tailscale or a TLS reverse proxy. The API token is not a substitute for transport encryption on an untrusted network.

## Mac/Linux runners

On each machine:

```bash
ghfleet init --skip-server --server-url http://control-plane:8787
# edit ~/.config/ghfleet/runner.yaml: local paths, labels, checks, token
ghfleet runner
```

Runner labels route jobs. On the server, tag repos that need a specific platform:

```yaml
repos:
  - name: omlx
    upstream: jundot/omlx
    fork: alytaphoenix/omlx
    runner_labels: [darwin, metal]
```

Then a Linux runner with `labels: [linux, cuda]` will not claim that repo's jobs.

### launchd on macOS

```bash
cp deploy/launchd/com.example.ghfleet.runner.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.example.ghfleet.runner.plist
tail -f ~/Library/Logs/ghfleet-runner.log
```

Edit the paths in the plist if your binary or config lives elsewhere.

### systemd on a Linux runner

Use the same `ghfleet.service` shape, but replace the command with:

```ini
ExecStart=/usr/local/bin/ghfleet runner --runner-config /etc/ghfleet/runner.yaml
```

and give it a writable `work_root` (default `~/.local/share/ghfleet-runner`).

## Air-gapped / no Actions

No GitHub Actions are used. The server polls the GitHub GraphQL API with `GH_TOKEN`; runners use `git` and the local agent CLI. `gh auth setup-git` is not required because git commands use `gh auth git-credential` when `gh` is installed. If `gh` is not installed on a runner, configure a normal git credential helper or use SSH remotes.
