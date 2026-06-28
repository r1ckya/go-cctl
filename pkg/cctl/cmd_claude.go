package cctl

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

func newClaudeCmd() *cobra.Command {
	var (
		branchFlag string
		noWorktree bool
		fresh      bool
	)
	cmd := &cobra.Command{
		Use:   "claude <server[/repo]> <session> [-- <prompt...>]",
		Short: "Create or resume a Claude session on a remote server",
		Args:  cobra.MinimumNArgs(2),
		Long: `Create or resume a tmux+Claude session on a remote server.

The target is "<server>/<repo>" — required unless the server has exactly one
repo (in which case bare "<server>" works). Examples:
  cctl claude local/rxtx.dev audit
  cctl claude workspace audit          # only works if workspace has one repo

If the session does not exist, a git worktree is created on a new branch
(<branch_prefix>/<session> by default) and Claude is launched inside it.

If the session already exists, you are attached to it. When a prompt is given
on resume, the prompt is typed into the running Claude REPL via tmux send-keys.

Anything after "--" is treated as the initial prompt.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			serverName, repoName := parseTarget(args[0])
			sessionName := args[1]
			prompt := strings.TrimSpace(strings.Join(args[2:], " "))

			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			r, err := cfg.resolve(serverName, repoName)
			if err != nil {
				return err
			}
			return runClaude(r, sessionName, branchFlag, prompt, noWorktree, fresh)
		},
	}
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch name (defaults to <branch_prefix>/<session>)")
	cmd.Flags().BoolVar(&noWorktree, "no-worktree", false, "run in the repo root on its current branch (skip worktree setup)")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "if the session already exists, kill it first and start over")
	return cmd
}

func runClaude(r *Resolved, sessionName, branchOverride, prompt string, noWorktree, fresh bool) error {
	// CLI command takes a single name and uses it for both the worktree and
	// the session — the TUI is where you create multiple sessions on one
	// worktree.
	cmdStr, err := prepareClaude(r, sessionName, sessionName, branchOverride, prompt, noWorktree, fresh)
	if err != nil {
		return err
	}
	return execInteractive(r.Server, r.UseMosh, cmdStr)
}

// prepareClaude performs all side-effects needed before attaching (kill on
// --fresh, worktree creation, send-keys for prompt-on-resume) and returns the
// shell command string the caller should run interactively (mosh / ssh -t /
// bash) to land in the tmux session. The worktree and session are
// independent identifiers: when both already exist together, we attach
// (reuse); when only the worktree exists, we start a new tmux session inside
// it; when neither exists, we create the worktree first.
func prepareClaude(r *Resolved, worktreeName, sessionName, branchOverride, prompt string, noWorktree, fresh bool) (string, error) {
	tname := tmuxName(r.RepoName, worktreeName, sessionName)

	if fresh {
		exists, err := hasSession(r.Server, tname)
		if err != nil {
			return "", err
		}
		if exists {
			if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tname))); err != nil {
				return "", fmt.Errorf("kill existing session: %w", err)
			}
		}
	}

	exists, err := hasSession(r.Server, tname)
	if err != nil {
		return "", err
	}

	// Work out the session's working directory up front — both branches
	// need it. "main" is the alias for the repo's own checkout; no extra
	// worktree is created and claude runs in the original repo path.
	var cwd string
	if noWorktree || worktreeName == "main" {
		cwd = r.Repo.Path
	} else {
		cwd = worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
	}

	if exists {
		if prompt != "" {
			// Type the prompt into the running claude REPL, then attach.
			// Use `-l` (literal) for the prompt body and a separate Enter keystroke.
			send := fmt.Sprintf(
				"tmux send-keys -t %s -l %s && tmux send-keys -t %s Enter",
				shellQuote(tname), shellQuote(prompt), shellQuote(tname),
			)
			if _, err := runRemote(r.Server, send); err != nil {
				return "", fmt.Errorf("send-keys to %s: %w", tname, err)
			}
		}
		return attachOrRespawn(r, tname, cwd), nil
	}

	// New session — create the worktree if needed.
	branch := branchOverride
	if branch == "" {
		if r.BranchPrefix != "" {
			branch = r.BranchPrefix + "/" + worktreeName
		} else {
			branch = worktreeName
		}
	}

	if !noWorktree && worktreeName != "main" {
		script := ensureWorktreeScript(r.Repo.Path, cwd, branch, r.DefaultBranch, r.WorktreePostCreate)
		if out, err := runRemote(r.Server, script); err != nil {
			if out != "" {
				fmt.Fprintln(os.Stderr, strings.TrimSpace(out))
			}
			// Log the failing script in full so the user can pipe
			// `tail ~/.cctl.log` to a bug report and we can reproduce
			// exactly. remote-run-fail already logged stderr+exit.
			log().Warn("worktree-create-fail",
				"server", r.ServerName, "repo", r.RepoName,
				"worktree", worktreeName, "branch", branch, "cwd", cwd,
				"repo_path", r.Repo.Path, "script_bytes", len(script),
			)
			return "", fmt.Errorf("create worktree on %s: %w (see ~/.cctl.log for the full remote script)", r.ServerName, err)
		}
	}

	launch := claudeLaunchScript(cwd, r.ClaudeFlags, prompt, !r.Server.Local, claudeSessionID(r.ServerName, tname))
	// `new-session -A` creates if absent, attaches if present — which is what we
	// want, and avoids a race between has-session and new-session.
	return fmt.Sprintf("tmux new-session -A -s %s %s", shellQuote(tname), shellQuote(launch)), nil
}

// attachOrRespawn returns the shell command for landing in a session that
// exists right now. A bare `tmux attach` would be the obvious choice, but
// the command gets baked into a wrapper script that can outlive the
// session: cmux persists the script path in its workspace layout and
// re-runs it when restoring the workspace, by which point the session may
// have died (claude exited, server rebooted, ...) and attach would fail
// with tmux's terse "can't find session". `new-session -A` attaches while
// the session is alive and otherwise resurrects it in the same worktree —
// claudeLaunchScript's `claude --continue` branch picks the conversation
// back up.
//
// Terminal sessions (created by the TUI's `t` key, named term/term2/…)
// resurrect as plain shells instead of relaunching claude.
func attachOrRespawn(r *Resolved, tname, cwd string) string {
	if _, _, sess, ok := parseTmuxName(tname); ok && isTerminalSession(sess) {
		return terminalCmd(tname, cwd)
	}
	launch := claudeLaunchScript(cwd, r.ClaudeFlags, "", !r.Server.Local, claudeSessionID(r.ServerName, tname))
	return fmt.Sprintf("tmux new-session -A -s %s %s", shellQuote(tname), shellQuote(launch))
}

// terminalSessionRe matches the names the TUI's `t` key generates for
// plain-shell sessions: "term", "term2", "term3", …
var terminalSessionRe = regexp.MustCompile(`^term\d*$`)

func isTerminalSession(name string) bool {
	return terminalSessionRe.MatchString(name)
}

// claudeUpdateScript is the claude-specific name for the update wrapper; it
// delegates to agentUpdateScript (which prepends the well-known install dirs
// and sources nvm). Retained so existing callers/tests keep working.
func claudeUpdateScript(updateCmd string) string {
	return agentUpdateScript(updateCmd)
}

// terminalCmd is the idempotent command for a plain-shell session: attach
// if it's alive, otherwise create it with the worktree as the working
// directory. Same transport as claude sessions (tmux on local/ssh/mosh),
// just without claude — the `t` key's "give me a terminal here" action.
func terminalCmd(tname, cwd string) string {
	return fmt.Sprintf("tmux new-session -A -s %s -c %s", shellQuote(tname), shellPath(cwd))
}
