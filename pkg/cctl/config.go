package cctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level on-disk schema for ~/.cctl.yaml.
type Config struct {
	Defaults Defaults          `yaml:"defaults"`
	Servers  map[string]Server `yaml:"servers"`

	// repoCache memoises discoverRepos() per server for the lifetime of the
	// process so the TUI / repeated resolve() calls don't re-shell each time.
	repoCache map[string]map[string]Repo
}

type Defaults struct {
	BranchPrefix string   `yaml:"branch_prefix"`
	WorktreeBase string   `yaml:"worktree_base"`
	ClaudeFlags  []string `yaml:"claude_flags"`
	// CodexFlags are passed to the OpenAI Codex CLI by `cctl codex`, the
	// analogue of ClaudeFlags for `cctl claude` (e.g. ["-m", "gpt-5.1"]).
	CodexFlags []string `yaml:"codex_flags"`
	// Agent selects which coding agent the TUI's session shortcuts (new
	// session, attach/respawn, the `U` upgrade, reconcile revival) launch:
	// "claude" (default) or "codex". Override per-server or per-repo. The
	// `cctl claude` / `cctl codex` CLI commands always force their own agent
	// regardless of this setting.
	Agent    string `yaml:"agent"`
	Mosh     *bool  `yaml:"mosh"`
	Shell    string `yaml:"shell"`
	LogLevel string `yaml:"log_level"` // debug, info (default), warn, error
	// SyncAllServers controls whether the cmux↔cctl sync (R/S + the startup
	// pass) checks tmux liveness and closes stale tabs on EVERY server, not
	// just the local one. Default true. Set false to limit liveness/close to
	// the local server (remote tabs then left to cmux's own ssh restore).
	SyncAllServers *bool `yaml:"sync_all_servers"`
	// SyncCloseUnmatched controls whether sync also closes a DEAD workspace
	// that follows cctl's "repo/worktree/session" naming but can't be matched
	// to a tracked or live session (i.e. cctl can't prove it owns it). Default
	// false: such tabs are left alone so a manually-opened tab is never
	// closed. Set true to aggressively prune them. Tabs whose name isn't
	// cctl-shaped (plain shells, custom names) are never auto-closed either way.
	SyncCloseUnmatched *bool `yaml:"sync_close_unmatched"`
	// BetaRemoteClaudeStatus (beta) makes cmux show live Claude status for
	// REMOTE sessions. cmux's native integration can't reach a remote claude
	// (it runs on another host, behind mosh+tmux); with this on, cctl relays
	// the remote claude hook events it already captures and re-fires them
	// locally as `cmux hooks claude <event>` against the session's surface, so
	// cmux's own status machinery ("needs input", reorder, …) lights up.
	// Default false. Experimental — verify it behaves before relying on it.
	BetaRemoteClaudeStatus *bool `yaml:"beta_remote_claude_status"`
	// Spawn chooses the terminal-spawn provider when launching sessions
	// from the TUI: "auto" (default — detect from $TERM_PROGRAM), or one
	// of "ghostty", "cmux", "wezterm", "kitty", "iterm2", "inline".
	Spawn string `yaml:"spawn"`
	// WorktreePostCreate is a list of shell commands to run inside a
	// freshly-created worktree directory, right after `git worktree add`
	// succeeds and before the tmux session launches. Useful for things
	// like `mise trust`, `direnv allow`, `pre-commit install`, etc.
	// Each command is run via `sh -c "<cmd>"` and a failure is logged
	// but does NOT abort the session — these are conveniences, not
	// gating checks.
	WorktreePostCreate []string `yaml:"worktree_post_create"`
	// ClaudeUpdate is the shell command the TUI's `U` key runs on a
	// server to upgrade claude before restarting that server's claude
	// sessions. Override when claude is managed by npm/mise/brew on a
	// given setup (e.g. "npm install -g @anthropic-ai/claude-code").
	ClaudeUpdate string `yaml:"claude_update"`
	// CodexUpdate is the codex equivalent of ClaudeUpdate (the `U` key's
	// upgrade command for codex sessions). Defaults to "codex update".
	CodexUpdate string `yaml:"codex_update"`
}

// syncAllServers reports whether sync should check liveness + close stale
// tabs on every server (default) or just the local one.
func (c *Config) syncAllServers() bool {
	if c.Defaults.SyncAllServers != nil {
		return *c.Defaults.SyncAllServers
	}
	return true
}

// syncCloseUnmatched reports whether sync may close dead cctl-shaped
// workspaces it can't match to a tracked/live session. Default false (protect
// manually-opened tabs).
func (c *Config) syncCloseUnmatched() bool {
	if c.Defaults.SyncCloseUnmatched != nil {
		return *c.Defaults.SyncCloseUnmatched
	}
	return false
}

// betaRemoteClaudeStatus reports whether to relay remote claude hook events
// into cmux's native status (beta). Default false.
func (c *Config) betaRemoteClaudeStatus() bool {
	if c.Defaults.BetaRemoteClaudeStatus != nil {
		return *c.Defaults.BetaRemoteClaudeStatus
	}
	return false
}

// claudeUpdateCmd returns the configured upgrade command, defaulting to
// claude's built-in self-updater.
func (c *Config) claudeUpdateCmd() string {
	if cmd := strings.TrimSpace(c.Defaults.ClaudeUpdate); cmd != "" {
		return cmd
	}
	return "claude update"
}

type Server struct {
	Host        string          `yaml:"host"`
	User        string          `yaml:"user"`
	Port        int             `yaml:"port"`
	SSHKey      string          `yaml:"ssh_key"`
	SSHOpts     []string        `yaml:"ssh_opts"`
	Mosh        *bool           `yaml:"mosh"`
	Local       bool            `yaml:"local"` // run commands locally instead of via ssh/mosh
	RepoSources []RepoSource    `yaml:"repo_sources"`
	Repos       map[string]Repo `yaml:"repos"`
	// Agent overrides defaults.agent for this server (see Defaults.Agent).
	Agent string `yaml:"agent"`
	// Transport selects how the TUI opens this server's sessions in cmux.
	// "" / "mosh" (default): a local workspace running mosh/ssh inside a
	// wrapper script. "cmux-ssh": a cmux remote-SSH workspace (`cmux ssh`)
	// — the Files panel lists the REMOTE filesystem, browser panes route
	// through the remote network, and the cmux CLI relay works on the
	// remote — at the cost of mosh (cmux reconnects over ssh instead).
	// Non-interactive commands (list/kill/worktree ops) always use ssh
	// regardless.
	Transport string `yaml:"transport"`
	// Timeout bounds how long ssh waits to connect to this server, as a
	// Go duration string (e.g. "10s", "1m500ms"). Applied as ssh's
	// ConnectTimeout so an unreachable host fails fast instead of hanging on
	// the OS default. Empty / unparseable → defaultConnectTimeout (10s).
	Timeout string `yaml:"timeout"`
}

// defaultConnectTimeout is the ssh connect timeout when a server doesn't set
// one. Short enough that a dead remote settles quickly (startup sync waits on
// every server settling), long enough to tolerate a slow handshake.
const defaultConnectTimeout = 10 * time.Second

// connectTimeout returns the server's ssh connect timeout (defaultConnectTimeout
// when unset or unparseable). Non-positive durations fall back to the default.
func (s Server) connectTimeout() time.Duration {
	if s.Timeout != "" {
		if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
			return d
		}
	}
	return defaultConnectTimeout
}

// useCmuxSSH reports whether interactive sessions on this server should
// open as cmux remote-SSH workspaces.
func (s Server) useCmuxSSH() bool {
	return !s.Local && strings.EqualFold(strings.TrimSpace(s.Transport), "cmux-ssh")
}

// RepoSource is a search root: cctl walks it up to MaxDepth looking for git
// repos and surfaces each as a Repo named by its path relative to the source.
type RepoSource struct {
	Path          string `yaml:"path"`
	MaxDepth      int    `yaml:"max_depth"`
	DefaultBranch string `yaml:"default_branch"`
}

type Repo struct {
	Path          string `yaml:"path"`
	DefaultBranch string `yaml:"default_branch"`
	WorktreeBase  string `yaml:"worktree_base"`
	// Agent overrides server/defaults agent for this repo (see Defaults.Agent).
	Agent string `yaml:"agent"`
}

// Resolved is a server+repo pair with defaults already merged in.
type Resolved struct {
	ServerName         string
	Server             Server
	RepoName           string
	Repo               Repo
	BranchPrefix       string
	WorktreeBase       string
	ClaudeFlags        []string
	CodexFlags         []string
	Agent              string
	UseMosh            bool
	DefaultBranch      string
	WorktreePostCreate []string
}

func loadConfig() (*Config, string, error) {
	path, err := resolveConfigPath()
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, path, fmt.Errorf("config not found at %s — create one (run `cctl init` to auto-generate or `cctl config sample` for an example)", path)
		}
		return nil, path, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Servers) == 0 {
		return nil, path, fmt.Errorf("%s: no servers defined", path)
	}
	setLogLevel(c.Defaults.LogLevel)
	log().Info("config-loaded", "path", path, "servers", len(c.Servers))
	return &c, path, nil
}

func resolveConfigPath() (string, error) {
	if configPath != "" {
		return expandPath(configPath), nil
	}
	if env := os.Getenv("CCTL_CONFIG"); env != "" {
		return expandPath(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cctl.yaml"), nil
}

// expandPath expands a leading ~ to the user's home directory.
// Remote paths (those starting with ~ that we send over SSH) are left alone —
// the remote shell expands them. Only call this for *local* paths.
func expandPath(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// resolve picks a server and repo. repoName may be empty when the server has
// exactly one repo (discovered + explicit combined); otherwise it must be
// passed in via the <server>/<repo> CLI form or the TUI.
func (c *Config) resolve(serverName, repoName string) (*Resolved, error) {
	srv, ok := c.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("unknown server %q (configured: %s)", serverName, strings.Join(c.serverNames(), ", "))
	}
	allRepos, err := c.repos(serverName)
	if err != nil {
		return nil, err
	}
	if repoName == "" && len(allRepos) == 1 {
		for n := range allRepos {
			repoName = n
		}
	}
	repo, ok := allRepos[repoName]
	if !ok {
		if repoName == "" {
			return nil, fmt.Errorf("server %q has %d repos; specify one with %s/<repo> (available: %s)", serverName, len(allRepos), serverName, strings.Join(repoNames(allRepos), ", "))
		}
		return nil, fmt.Errorf("server %q has no repo %q (available: %s)", serverName, repoName, strings.Join(repoNames(allRepos), ", "))
	}

	useMosh := true
	if c.Defaults.Mosh != nil {
		useMosh = *c.Defaults.Mosh
	}
	if srv.Mosh != nil {
		useMosh = *srv.Mosh
	}
	if srv.Local {
		useMosh = false
	}

	wtBase := repo.WorktreeBase
	if wtBase == "" {
		wtBase = c.Defaults.WorktreeBase
	}
	if wtBase == "" {
		wtBase = "~/worktrees"
	}

	branchPrefix := c.Defaults.BranchPrefix
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Agent precedence: repo → server → defaults → "claude". Validated here
	// so a typo fails loudly at resolve time rather than launching the wrong
	// (or no) agent.
	agent := firstNonEmpty(repo.Agent, srv.Agent, c.Defaults.Agent, agentClaude)
	if agent != agentClaude && agent != agentCodex {
		return nil, fmt.Errorf("invalid agent %q for %s/%s (want %q or %q)", agent, serverName, repoName, agentClaude, agentCodex)
	}

	return &Resolved{
		ServerName:         serverName,
		Server:             srv,
		RepoName:           repoName,
		Repo:               repo,
		BranchPrefix:       branchPrefix,
		WorktreeBase:       wtBase,
		ClaudeFlags:        c.Defaults.ClaudeFlags,
		CodexFlags:         c.Defaults.CodexFlags,
		Agent:              agent,
		UseMosh:            useMosh,
		DefaultBranch:      defaultBranch,
		WorktreePostCreate: append([]string(nil), c.Defaults.WorktreePostCreate...),
	}, nil
}

func (c *Config) serverNames() []string {
	out := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		out = append(out, n)
	}
	return out
}

// repos returns the merged map of repos for a server: discovered (via
// repo_sources) plus explicit (via repos:), with explicit entries winning on
// name collision. Discovery is cached for the lifetime of the *Config.
func (c *Config) repos(serverName string) (map[string]Repo, error) {
	srv, ok := c.Servers[serverName]
	if !ok {
		return nil, fmt.Errorf("unknown server %q", serverName)
	}
	out := map[string]Repo{}
	if len(srv.RepoSources) > 0 {
		if c.repoCache == nil {
			c.repoCache = map[string]map[string]Repo{}
		}
		cached, ok := c.repoCache[serverName]
		if !ok {
			d, err := discoverRepos(srv)
			if err != nil {
				return nil, fmt.Errorf("discover repos on %s: %w", serverName, err)
			}
			c.repoCache[serverName] = d
			cached = d
		}
		for k, v := range cached {
			out[k] = v
		}
	}
	for k, v := range srv.Repos {
		out[k] = v
	}
	return out, nil
}

// invalidateRepoCache forces the next repos() call to re-run discovery.
//
//nolint:unused // wired in by the next TUI refresh task
func (c *Config) invalidateRepoCache(serverName string) {
	if c.repoCache == nil {
		return
	}
	delete(c.repoCache, serverName)
}

func repoNames(m map[string]Repo) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	return out
}

// parseTarget splits a "server" or "server/repo" CLI argument.
func parseTarget(s string) (server, repo string) {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}
