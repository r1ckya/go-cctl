package cctl

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// newCodexCmd is the OpenAI Codex analogue of newClaudeCmd: same target /
// session / worktree semantics, but it launches the `codex` CLI instead of
// `claude`. The two share tmux naming, so pick distinct session names if you
// want to run both agents on the same worktree.
func newCodexCmd() *cobra.Command {
	var (
		branchFlag string
		noWorktree bool
		fresh      bool
	)
	cmd := &cobra.Command{
		Use:   "codex <server[/repo]> <session> [-- <prompt...>]",
		Short: "Create or resume an OpenAI Codex session on a remote server",
		Args:  cobra.MinimumNArgs(2),
		Long: `Create or resume a tmux+Codex session on a remote server.

Works like "cctl claude" but launches the OpenAI Codex CLI instead.

The target is "<server>/<repo>" — required unless the server has exactly one
repo (in which case bare "<server>" works). Examples:
  cctl codex local/rxtx.dev audit
  cctl codex workspace audit          # only works if workspace has one repo

If the session does not exist, a git worktree is created on a new branch
(<branch_prefix>/<session> by default) and codex is launched inside it.

If the session already exists, you are attached to it. When a prompt is given
on resume, the prompt is typed into the running codex TUI via tmux send-keys.

Codex has no equivalent of claude's --session-id, so resume is keyed on the
worktree directory: the most recent codex session recorded for that directory
is continued (codex resume --last).

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
			return runCodex(r, sessionName, branchFlag, prompt, noWorktree, fresh)
		},
	}
	cmd.Flags().StringVar(&branchFlag, "branch", "", "branch name (defaults to <branch_prefix>/<session>)")
	cmd.Flags().BoolVar(&noWorktree, "no-worktree", false, "run in the repo root on its current branch (skip worktree setup)")
	cmd.Flags().BoolVar(&fresh, "fresh", false, "if the session already exists, kill it first and start over")
	return cmd
}

func runCodex(r *Resolved, sessionName, branchOverride, prompt string, noWorktree, fresh bool) error {
	// As with `cctl claude`, the CLI uses one name for both the worktree and
	// the session; the TUI is where you create multiple sessions on one
	// worktree.
	cmdStr, err := prepareCodex(r, sessionName, sessionName, branchOverride, prompt, noWorktree, fresh)
	if err != nil {
		return err
	}
	return execInteractive(r.Server, r.UseMosh, cmdStr)
}

// prepareCodex is the codex counterpart of prepareClaude: it performs the same
// pre-attach side-effects (kill on --fresh, worktree creation, send-keys for
// prompt-on-resume) and returns the shell command to run interactively. The
// only agent-specific differences are the launch script (codexLaunchScript)
// and respawn helper (codexAttachOrRespawn). The worktree/tmux orchestration is
// intentionally parallel to prepareClaude so the claude path stays untouched.
func prepareCodex(r *Resolved, worktreeName, sessionName, branchOverride, prompt string, noWorktree, fresh bool) (string, error) {
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

	var cwd string
	if noWorktree || worktreeName == "main" {
		cwd = r.Repo.Path
	} else {
		cwd = worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
	}

	if exists {
		if prompt != "" {
			send := fmt.Sprintf(
				"tmux send-keys -t %s -l %s && tmux send-keys -t %s Enter",
				shellQuote(tname), shellQuote(prompt), shellQuote(tname),
			)
			if _, err := runRemote(r.Server, send); err != nil {
				return "", fmt.Errorf("send-keys to %s: %w", tname, err)
			}
		}
		return codexAttachOrRespawn(r, tname, cwd), nil
	}

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
			log().Warn("worktree-create-fail",
				"server", r.ServerName, "repo", r.RepoName,
				"worktree", worktreeName, "branch", branch, "cwd", cwd,
				"repo_path", r.Repo.Path, "script_bytes", len(script),
			)
			return "", fmt.Errorf("create worktree on %s: %w (see ~/.cctl.log for the full remote script)", r.ServerName, err)
		}
	}

	launch := codexLaunchScript(cwd, r.CodexFlags, prompt)
	return fmt.Sprintf("tmux new-session -A -s %s %s", shellQuote(tname), shellQuote(launch)), nil
}

// codexAttachOrRespawn mirrors attachOrRespawn but relaunches codex (not
// claude) if the session has died. Terminal sessions (the TUI's `t` key,
// named term/term2/…) resurrect as plain shells, exactly like the claude path.
func codexAttachOrRespawn(r *Resolved, tname, cwd string) string {
	if _, _, sess, ok := parseTmuxName(tname); ok && isTerminalSession(sess) {
		return terminalCmd(tname, cwd)
	}
	launch := codexLaunchScript(cwd, r.CodexFlags, "")
	return fmt.Sprintf("tmux new-session -A -s %s %s", shellQuote(tname), shellQuote(launch))
}
