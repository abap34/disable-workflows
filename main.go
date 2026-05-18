package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/abap34/disable-workflows/internal/ghapi"
	"github.com/abap34/disable-workflows/internal/tui"
)

func main() {
	var cfg tui.Config
	var cacheDir string
	var minInterval time.Duration
	var maxRateWait time.Duration
	var cacheMaxAge time.Duration
	var tokenEnv string
	var noCache bool
	var noGhAuth bool

	flag.StringVar(&cfg.Owner, "owner", "", "GitHub user or organization login to inspect; defaults to the authenticated user")
	flag.StringVar(&cfg.RepoFilter, "repo", "", "case-insensitive substring filter for repository full name")
	flag.BoolVar(&cfg.IncludeArchived, "include-archived", false, "include archived repositories")
	flag.IntVar(&cfg.MaxRepos, "max-repos", 0, "maximum repositories to scan; 0 means no explicit limit")
	flag.IntVar(&cfg.Concurrency, "concurrency", 2, "number of repositories to inspect concurrently")
	flag.StringVar(&cfg.LastRunMode, "last-run", ghapi.LastRunWorkflow, "last run lookup mode: workflow (exact), repo (fast approximate), or off")
	flag.StringVar(&tokenEnv, "token-env", "GH_TOKEN", "environment variable containing the GitHub token; GITHUB_TOKEN is also checked")
	flag.StringVar(&cacheDir, "cache-dir", defaultCacheDir(), "directory for conditional request cache")
	flag.BoolVar(&noCache, "no-cache", false, "disable ETag response cache")
	flag.DurationVar(&cacheMaxAge, "cache-max-age", 5*time.Minute, "reuse cached GET responses without validation while younger than this; 0 always validates with ETag")
	flag.DurationVar(&minInterval, "min-request-interval", 250*time.Millisecond, "minimum interval between GitHub API requests")
	flag.DurationVar(&maxRateWait, "max-rate-wait", 2*time.Minute, "maximum time to wait automatically for rate-limit recovery")
	flag.BoolVar(&noGhAuth, "no-gh-auth", false, "do not fall back to gh auth token when no token env var is set")
	flag.Parse()

	if cfg.Owner == "" && flag.NArg() > 0 {
		cfg.Owner = flag.Arg(0)
	}
	cfg.Owner = strings.TrimSpace(cfg.Owner)

	token, tokenSource := resolveToken(tokenEnv, !noGhAuth)
	if token == "" {
		fmt.Fprintln(os.Stderr, "missing GitHub token: set GH_TOKEN or GITHUB_TOKEN, or authenticate `gh`")
		os.Exit(2)
	}
	cfg.TokenSource = tokenSource

	var cache *ghapi.Cache
	if !noCache {
		cache = ghapi.NewCache(cacheDir)
	}

	client := ghapi.NewClient(token,
		ghapi.WithCache(cache),
		ghapi.WithMinRequestInterval(minInterval),
		ghapi.WithMaxRateLimitWait(maxRateWait),
		ghapi.WithCacheMaxAge(cacheMaxAge),
	)

	if cfg.Owner == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		user, err := client.AuthenticatedUser(ctx)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not determine authenticated user; pass --owner explicitly: %v\n", err)
			os.Exit(2)
		}
		cfg.Owner = user.Login
	}

	model := tui.NewModel(context.Background(), client, cfg)
	program := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tui failed: %v\n", err)
		os.Exit(1)
	}
}

func resolveToken(primaryEnv string, allowGhAuth bool) (string, string) {
	if primaryEnv != "" {
		if token := strings.TrimSpace(os.Getenv(primaryEnv)); token != "" {
			return token, primaryEnv
		}
	}
	if primaryEnv != "GH_TOKEN" {
		if token := strings.TrimSpace(os.Getenv("GH_TOKEN")); token != "" {
			return token, "GH_TOKEN"
		}
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token, "GITHUB_TOKEN"
	}
	if !allowGhAuth {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", ""
	}
	if token := strings.TrimSpace(string(out)); token != "" {
		return token, "gh auth token"
	}
	return "", ""
}

func defaultCacheDir() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ".disable-workflows-cache"
	}
	return base + string(os.PathSeparator) + "disable-workflows"
}
