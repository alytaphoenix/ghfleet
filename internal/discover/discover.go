// Package discover finds local checkouts and builds starter configs.
package discover

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/alytaphoenix/ghfleet/internal/config"
	"github.com/alytaphoenix/ghfleet/internal/git"
)

// Found is one discovered repository.
type Found struct {
	Name          string
	Path          string
	Upstream      string
	Fork          string
	Own           bool
	ReadOnly      bool
	FetchRemote   string
	PushRemote    string
	DefaultBranch string
	Checks        []string
}

// Repos scans one level below root for git checkouts with GitHub remotes.
func Repos(ctx context.Context, root, login string) ([]Found, error) {
	root = config.ExpandPath(root)
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Found
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name())
		info, err := os.Stat(filepath.Join(path, ".git"))
		if err != nil || !info.IsDir() {
			// Skip linked worktrees (.git is a file); they share a main checkout.
			continue
		}
		remotes, err := git.Remotes(ctx, path)
		if err != nil {
			continue
		}
		bySlug := map[string]string{} // slug -> remote name
		for name, url := range remotes {
			if slug, ok := ParseRemote(url); ok {
				bySlug[slug] = name
			}
		}
		originURL := remotes["origin"]
		origin, ok := ParseRemote(originURL)
		if !ok {
			continue
		}
		f := Found{Name: e.Name(), Path: path, Upstream: origin, FetchRemote: "origin", PushRemote: "origin"}
		f.DefaultBranch = defaultBranch(ctx, path, "origin")
		if up, ok := ParseRemote(remotes["upstream"]); ok && up != origin {
			f.Upstream = up
			f.Fork = origin
			f.FetchRemote = "upstream"
			f.PushRemote = "origin"
		} else if fork, ok := ParseRemote(remotes["fork"]); ok && fork != origin {
			f.Upstream = origin
			f.Fork = fork
			f.FetchRemote = "origin"
			f.PushRemote = "fork"
		} else if slug, ok := ParseRemote(remotes["upstream"]); ok && slug == origin {
			// origin is upstream and there is no distinct fork remote.
		}
		f.Own = owner(f.Upstream) == login
		if f.Fork != "" && owner(f.Fork) != login {
			// Never treat someone else's fork as writable.
			f.Fork = ""
			f.ReadOnly = true
			f.PushRemote = ""
		}
		if f.Fork == "" && !f.Own {
			f.ReadOnly = true
			f.PushRemote = ""
		}
		if f.Fork == "" && f.Own {
			f.Fork = f.Upstream
		}
		// Prefer a remote whose URL matches the upstream if the conventional name is missing.
		if _, ok := remotes[f.FetchRemote]; !ok {
			for slug, name := range bySlug {
				if slug == f.Upstream {
					f.FetchRemote = name
					break
				}
			}
		}
		f.Checks = detectChecks(path)
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func defaultBranch(ctx context.Context, path, remote string) string {
	if out, err := git.MustRun(ctx, path, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		parts := strings.SplitN(out, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			return parts[1]
		}
	}
	return "main"
}

// ParseRemote extracts owner/name from a GitHub remote URL.
func ParseRemote(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSuffix(raw, "/")
	raw = strings.TrimSuffix(raw, ".git")
	switch {
	case strings.HasPrefix(raw, "git@github.com:"):
		return strings.TrimPrefix(raw, "git@github.com:"), true
	case strings.HasPrefix(raw, "ssh://git@github.com/"):
		return strings.TrimPrefix(raw, "ssh://git@github.com/"), true
	case strings.HasPrefix(raw, "https://github.com/"):
		return strings.TrimPrefix(raw, "https://github.com/"), true
	case strings.HasPrefix(raw, "http://github.com/"):
		return strings.TrimPrefix(raw, "http://github.com/"), true
	case strings.HasPrefix(raw, "github.com/"):
		return strings.TrimPrefix(raw, "github.com/"), true
	}
	return "", false
}

func owner(slug string) string {
	parts := strings.SplitN(slug, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func detectChecks(path string) []string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(path, name))
		return err == nil
	}
	switch {
	case exists("go.mod"):
		return []string{"go test ./..."}
	case exists("Cargo.toml"):
		return []string{"cargo check"}
	case exists("pyproject.toml"):
		if exists("tests") || exists("test") {
			if exists("uv.lock") {
				return []string{"uv run pytest -q"}
			}
			return []string{"python3 -m pytest -q"}
		}
	case exists("package.json"):
		if exists("bun.lock") || exists("bun.lockb") {
			return []string{"bun test"}
		}
	}
	return nil
}

// ServerConfig builds a starter server config.
func ServerConfig(found []Found, login string) *config.Config {
	cfg := &config.Config{}
	for _, f := range found {
		if f.ReadOnly {
			continue
		}
		cfg.Repos = append(cfg.Repos, config.Repo{
			Name:             f.Name,
			Upstream:         f.Upstream,
			Fork:             f.Fork,
			Own:              f.Own,
			ReadOnly:         f.ReadOnly,
			DefaultBranch:    f.DefaultBranch,
			FetchRemote:      f.FetchRemote,
			PushRemote:       f.PushRemote,
			AutoForkSync:     f.Fork != "" && f.Fork != f.Upstream && !f.ReadOnly,
			ForkSyncInterval: config.Duration(24 * time.Hour),
			AutoSecurity:     f.Own && !f.ReadOnly,
			SecurityInterval: config.Duration(24 * time.Hour),
			MergeUpstream:    !f.Own && !f.ReadOnly && f.Fork != "",
		})
	}
	cfg.Server.Viewer = login
	cfg.ApplyDefaults()
	return cfg
}

// RunnerConfig builds a starter runner config for the current machine.
func RunnerConfig(found []Found, serverURL string) *config.RunnerConfig {
	cfg := &config.RunnerConfig{ServerURL: serverURL}
	for _, f := range found {
		if f.ReadOnly {
			continue
		}
		cfg.Repos = append(cfg.Repos, config.RunnerRepo{
			Name:          f.Name,
			Path:          f.Path,
			Checks:        append([]string(nil), f.Checks...),
			FetchRemote:   f.FetchRemote,
			PushRemote:    f.PushRemote,
			DefaultBranch: f.DefaultBranch,
		})
	}
	cfg.ApplyDefaults()
	cfg.Agent.UseCaffeinate = runtime.GOOS == "darwin"
	return cfg
}

// Summary prints a short human-readable count.
func Summary(found []Found) string {
	var own, forks, readOnly int
	for _, f := range found {
		switch {
		case f.ReadOnly:
			readOnly++
		case f.Own:
			own++
		default:
			forks++
		}
	}
	return fmt.Sprintf("%d repos (%d owned, %d forks, %d read-only)", len(found), own, forks, readOnly)
}
