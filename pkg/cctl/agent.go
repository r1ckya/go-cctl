package cctl

import (
	"fmt"
	"strings"
)

// Supported coding agents. The TUI's session shortcuts launch whichever one a
// server/repo resolves to (see Defaults.Agent); the `cctl claude` / `cctl
// codex` CLI commands force their own agent regardless.
const (
	agentClaude = "claude"
	agentCodex  = "codex"
)

// firstNonEmpty returns the first argument that isn't the empty string, or ""
// if all are empty. Used for agent precedence (repo → server → defaults).
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// agentLaunchScript builds the launch command for r's agent — the dispatch
// point that lets every lifecycle site (TUI new-session, attach/respawn, the
// `U` upgrade, reconcile revival) launch claude OR codex from one call.
func agentLaunchScript(r *Resolved, cwd, prompt, tname string) string {
	switch r.Agent {
	case agentCodex:
		return codexLaunchScript(cwd, r.CodexFlags, prompt)
	default:
		return claudeLaunchScript(cwd, r.ClaudeFlags, prompt, !r.Server.Local, claudeSessionID(r.ServerName, tname))
	}
}

// agentAttachOrRespawn returns the attach/resurrect command for r's agent.
func agentAttachOrRespawn(r *Resolved, tname, cwd string) string {
	switch r.Agent {
	case agentCodex:
		return codexAttachOrRespawn(r, tname, cwd)
	default:
		return attachOrRespawn(r, tname, cwd)
	}
}

// prepareAgent dispatches session creation/resume to r's agent. Mirrors the
// CLI's runClaude/runCodex split for the TUI's new-session shortcut.
func prepareAgent(r *Resolved, worktreeName, sessionName, branchOverride, prompt string, noWorktree, fresh bool) (string, error) {
	switch r.Agent {
	case agentCodex:
		return prepareCodex(r, worktreeName, sessionName, branchOverride, prompt, noWorktree, fresh)
	default:
		return prepareClaude(r, worktreeName, sessionName, branchOverride, prompt, noWorktree, fresh)
	}
}

// agentUpdateCmd returns the self-update command for the given agent, honoring
// the configured override (claude_update / codex_update) and falling back to
// the agent's built-in updater.
func (c *Config) agentUpdateCmd(agent string) string {
	switch agent {
	case agentCodex:
		if cmd := strings.TrimSpace(c.Defaults.CodexUpdate); cmd != "" {
			return cmd
		}
		return "codex update"
	default:
		return c.claudeUpdateCmd()
	}
}

// agentUpdateScript wraps an update command so it resolves the agent binary the
// way the user's interactive shell would. runRemote executes over bare ssh,
// which gets the minimal non-login PATH — neither claude nor codex usually
// lives there (native installer → ~/.local/bin, mise → shims, npm → a prefix
// dir, nvm → a version dir, all added by profile files). We prepend the
// standard locations and add every installed nvm node version's bin (codex is
// commonly nvm-managed, and sourcing nvm.sh doesn't auto-activate a version)
// so the update command resolves regardless of how the agent was installed.
func agentUpdateScript(updateCmd string) string {
	return fmt.Sprintf(
		`export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$HOME/.claude/local:$HOME/.npm-global/bin:$HOME/bin:$PATH"
for d in "$HOME"/.nvm/versions/node/*/bin; do [ -d "$d" ] && PATH="$d:$PATH"; done
export PATH
exec bash -lc %s`,
		shellQuote(updateCmd))
}
