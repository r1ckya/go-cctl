package cctl

import (
	"crypto/sha1"
	"fmt"
	"strings"
)

// worktreePath returns the remote-absolute (~-expanded by the remote shell)
// path for a session's worktree under base.
func worktreePath(base, repo, session string) string {
	base = strings.TrimRight(base, "/")
	return base + "/" + repo + "/" + session
}

// ensureWorktreeScript returns a bash one-liner that creates the worktree if
// missing — and otherwise uses it as-is, whatever branch it's on.
//
// postCreate is a list of shell commands run inside the worktree after the
// branch is guaranteed to exist on disk. They run regardless of whether we
// created the worktree this invocation or it was already there: hooks like
// `mise trust`, `direnv allow`, `pre-commit install` are idempotent and the
// user wants them applied to existing worktrees too (otherwise pre-existing
// worktrees would never get the trust applied and `mise.toml not trusted`
// would persist forever). Each command runs via `sh -c` and is best-effort:
// a failure is logged to stderr but does not abort the script.
func ensureWorktreeScript(repoPath, wtPath, branch, baseBranch string, postCreate []string) string {
	// REPO_CFG keeps the original config string (e.g. `~/myrepo`) for the
	// diagnostic — `$REPO` itself has already been expanded by the shell,
	// so just printing it leaves the user guessing which config key to fix.
	lines := []string{
		"set -euo pipefail",
		"REPO=" + shellPath(repoPath),
		"REPO_CFG=" + shellQuote(repoPath),
		"WT=" + shellPath(wtPath),
		"BRANCH=" + shellQuote(branch),
		"BASE=" + shellQuote(baseBranch),
		`if [ ! -d "$REPO" ]; then`,
		`  echo "cctl: repo path $REPO_CFG (resolved to $REPO) does not exist on this host — fix repos.<name>.path in ~/.cctl.yaml or clone the repo there" >&2`,
		`  exit 2`,
		`fi`,
		`if [ ! -e "$REPO/.git" ]; then`,
		`  echo "cctl: $REPO is not a git repository (no .git) — fix repos.<name>.path in ~/.cctl.yaml" >&2`,
		`  exit 2`,
		`fi`,
		`mkdir -p "$(dirname "$WT")"`,
		`cd "$REPO"`,
		`if [ ! -e "$WT/.git" ]; then`,
		`  if git rev-parse --verify --quiet "refs/heads/$BRANCH" >/dev/null; then`,
		`    git worktree add "$WT" "$BRANCH"`,
		`  else`,
		`    git worktree add "$WT" -b "$BRANCH" "$BASE"`,
		`  fi`,
		`fi`,
	}
	if len(postCreate) > 0 {
		lines = append(lines, `cd "$WT"`)
		for _, cmd := range postCreate {
			// Run via sh -c and log on failure. The user's cmd may contain
			// any chars (we don't sanitize) — shellQuote handles the
			// quoting and the echo string only uses safe replacements.
			safeLabel := strings.NewReplacer(`"`, `'`, "`", `'`).Replace(cmd)
			lines = append(lines, fmt.Sprintf(
				`sh -c %s || echo "cctl post-create: %s failed (exit $?)" >&2`,
				shellQuote(cmd), safeLabel,
			))
		}
	}
	return strings.Join(lines, "\n")
}

// removeWorktreeScript returns a bash script that removes a git worktree
// directory. Branch is preserved (matching `cctl rm`).
//
// **Safety model** — after a past incident where cctl rm-rf'd a user's
// main checkout, this script MUST refuse anything that isn't unambiguously
// a worktree. The contract:
//
//  1. Resolve real paths (pwd -P) so symlinks can't sneak past checks.
//  2. Refuse if $WT == $REPO (the main checkout — what blew us up).
//  3. Refuse if $WT is an ancestor of $REPO (so we never delete a
//     parent dir that contains the repo).
//  4. Refuse if $WT is $HOME, "/", or empty.
//  5. If $WT is OUTSIDE the configured worktree_base, it must be a
//     git-REGISTERED worktree of this repo (per `git worktree list
//     --porcelain`) — worktrees created by other tools (cmux, manual
//     `git worktree add`) live anywhere, and refusing them outright made
//     dd useless on those rows. Unregistered outside paths are refused.
//  6. Use `git worktree remove [--force]` for the actual removal.
//  7. Only fall back to `rm -rf` when the path remains AND it's inside
//     the worktree_base. Outside the base there is NO rm fallback — if
//     git can't remove it, the script fails with the reason instead.
//
// If any check fails, exit 3 with a message naming the cause. The TUI/CLI
// surface this as a refusal so the user knows what happened.
//
// worktreeBase is the user's configured `defaults.worktree_base` (e.g.
// "~/worktrees"). It anchors the rm fallback so even if every other check
// were broken, the worst case for rm -rf is a path inside that base.
func removeWorktreeScript(repoPath, wtPath, worktreeBase string) string {
	if worktreeBase == "" {
		worktreeBase = "~/worktrees"
	}
	return strings.Join([]string{
		"set -euo pipefail",
		"REPO=" + shellPath(repoPath),
		"WT=" + shellPath(wtPath),
		"WT_BASE=" + shellPath(worktreeBase),
		`# Resolve real paths once so symlinks can't sneak past the guards.`,
		`WT_REAL="$(cd "$WT" 2>/dev/null && pwd -P || true)"`,
		`REPO_REAL="$(cd "$REPO" 2>/dev/null && pwd -P || true)"`,
		`WT_BASE_REAL="$(cd "$WT_BASE" 2>/dev/null && pwd -P || true)"`,
		`HOME_REAL="$(cd "$HOME" 2>/dev/null && pwd -P || true)"`,
		`# Guard 1 + 2: WT must not be the repo itself, nor an ancestor of it.`,
		`if [ -n "$WT_REAL" ] && [ -n "$REPO_REAL" ]; then`,
		`  if [ "$WT_REAL" = "$REPO_REAL" ]; then`,
		`    echo "cctl: refusing — $WT_REAL is the repo's main checkout" >&2; exit 3`,
		`  fi`,
		`  case "$REPO_REAL/" in`,
		`    "$WT_REAL"/*)`,
		`      echo "cctl: refusing — $WT_REAL contains the repo ($REPO_REAL); deleting it would delete the repo" >&2; exit 3`,
		`      ;;`,
		`  esac`,
		`fi`,
		`# Guard 3: never touch $HOME or "/".`,
		`case "${WT_REAL:-$WT}" in`,
		`  ""|"/"|"$HOME_REAL"|"$HOME")`,
		`    echo "cctl: refusing — $WT looks like home or root" >&2; exit 3`,
		`    ;;`,
		`esac`,
		`# Guard 4: rm -rf is only ever allowed INSIDE the configured`,
		`# worktree_base — that's the load-bearing safety net from the old`,
		`# delete-the-repo incident. A path OUTSIDE the base is still`,
		`# removable, but only when git itself lists it as a worktree of`,
		`# this repo, and only via git worktree remove (no rm fallback).`,
		`check_under_base() {`,
		`  local wt="$1" base="$2"`,
		`  [ -n "$wt" ] && [ -n "$base" ] || return 1`,
		`  case "$wt/" in`,
		`    "$base"/*) return 0 ;;`,
		`  esac`,
		`  return 1`,
		`}`,
		`# Prefer real-path containment; fall back to lexical if the dir`,
		`# is already gone (can't pwd -P on a missing path).`,
		`UNDER_BASE=0`,
		`if check_under_base "${WT_REAL:-$WT}" "$WT_BASE_REAL" || check_under_base "$WT" "$WT_BASE"; then`,
		`  UNDER_BASE=1`,
		`fi`,
		`if [ "$UNDER_BASE" != 1 ]; then`,
		`  REGISTERED=0`,
		`  if [ -d "$REPO/.git" ]; then`,
		`    if git -C "$REPO" worktree list --porcelain 2>/dev/null | grep -Fx -e "worktree ${WT_REAL:-$WT}" -e "worktree $WT" >/dev/null; then`,
		`      REGISTERED=1`,
		`    fi`,
		`  fi`,
		`  if [ "$REGISTERED" != 1 ]; then`,
		`    echo "cctl: refusing — $WT is outside worktree_base ($WT_BASE) and not a registered worktree of $REPO. Remove this manually if you're certain." >&2`,
		`    exit 3`,
		`  fi`,
		`fi`,
		`# Past all guards. Let git handle it first.`,
		`if [ -e "$WT/.git" ] && [ -d "$REPO/.git" ]; then`,
		`  cd "$REPO"`,
		`  git worktree remove "$WT" 2>/dev/null || git worktree remove --force "$WT" 2>/dev/null || true`,
		`fi`,
		`# Fallback: rm -rf only inside worktree_base; outside it git was`,
		`# the only tool allowed to touch the path.`,
		`if [ -e "$WT" ] && [ "$UNDER_BASE" = 1 ]; then`,
		`  rm -rf -- "$WT"`,
		`fi`,
		`if [ -e "$WT" ]; then`,
		`  echo "worktree at $WT still exists after removal attempts (git worktree remove failed; rm fallback is disabled outside worktree_base)" >&2`,
		`  exit 1`,
		`fi`,
	}, "\n")
}

// claudeLaunchScript builds the full shell command tmux will execute in the
// new session: cd into the worktree (or repo) and run claude with flags+prompt.
//
// If claude has a prior conversation for this directory (stored under
// ~/.claude/projects/<cwd-with-/-and-.-replaced-by-->), the launcher uses
// `claude --continue` so quitting and re-running `cctl claude` lands you back
// in the same conversation. Otherwise it starts fresh.
//
// When this script runs inside a cmux pane (CMUX_WORKSPACE_ID is set) and
// cmux's claude wrapper is on disk, we prepend cmux's bin to $PATH so the
// `claude` lookup resolves to cmux's wrapper instead of the user's real
// claude. The wrapper injects session tracking + notification hooks so
// cmux's "Claude Code Integration" actually fires when a session asks
// for input or finishes. On remote targets (mosh/ssh) the path doesn't
// exist and the guard is a harmless no-op.
//
// On claude exit the wrapper waits for Enter, keeping the tmux session alive
// so you can detach (Ctrl-b d) and resume later if you want, without losing
// the exit output.
//
// `bridge` (remote sessions) installs the cctl→cmux notification bridge:
// a hook script + claude --settings overlay under ~/.cctl/ on the remote
// host, so Notification/Stop events land in ~/.cctl/notify.jsonl where
// the TUI's notifyWatcher picks them up. Local sessions get the same
// effect natively from cmux's claude wrapper via the PATH injection.
func claudeLaunchScript(cwd string, claudeFlags []string, prompt string, bridge bool, sessionID string) string {
	fresh := append([]string{"claude", "--session-id", sessionID}, claudeFlags...)
	resume := append([]string{"claude", "--resume", sessionID}, claudeFlags...)
	if prompt != "" {
		fresh = append(fresh, prompt)
		resume = append(resume, prompt)
	}
	resumeCmd := joinShell(resume)
	freshCmd := joinShell(fresh)
	ensure := ""
	if bridge {
		// The settings flag is appended OUTSIDE joinShell so $HOME
		// expands on the remote.
		resumeCmd += notifyBridgeSettingsFlag
		freshCmd += notifyBridgeSettingsFlag
		ensure = notifyBridgeEnsureBlock() + "\n"
	}
	return fmt.Sprintf(`cd %s || {
  echo "cctl: worktree directory is gone — press Enter to close"
  read _ || true
  exit 1
}
# Ensure claude is found. A respawn (UU) or any non-login shell starts with a
# minimal PATH that omits the usual install dirs (native installer, mise,
# npm, …); a fresh mosh/ssh login shell happens to include them, so the first
# launch worked but a respawn hit "claude: command not found". Prepend the
# standard locations so every launch path resolves claude.
export PATH="$HOME/.local/bin:$HOME/.local/share/mise/shims:$HOME/.claude/local:$HOME/.npm-global/bin:$HOME/bin:$PATH"
%s# Route claude through cmux's wrapper when running inside cmux so the
# notification + session-tracking hooks attach. Harmless on remote
# (the path simply doesn't exist).
if [ -n "${CMUX_WORKSPACE_ID:-}" ] && [ -x /Applications/cmux.app/Contents/Resources/bin/claude ]; then
  export PATH="/Applications/cmux.app/Contents/Resources/bin:$PATH"
fi
# Per-session claude conversation: resume THIS session's own transcript if it
# exists, else start a fresh one pinned to this id. Keyed on the session id,
# not the cwd — so two sessions sharing a worktree don't resume each other
# (resuming by cwd picked the worktree's most recent chat regardless).
sid=%s
proj="$HOME/.claude/projects/$(pwd -P | tr '/.' '--')"
if [ -f "$proj/$sid.jsonl" ]; then
  %s
else
  %s
fi
status=$?
echo
echo "(claude exited with status $status — press Enter to close, Ctrl-b d to keep session)"
read _ || true`,
		shellPath(cwd),
		ensure,
		shellQuote(sessionID),
		resumeCmd,
		freshCmd,
	)
}

// claudeSessionID derives a stable claude --session-id (a v5-style UUID) for a
// cctl session from its server + tmux name. Deterministic so create and any
// later revive resolve to the SAME conversation, and distinct per session so
// two sessions on one worktree stay isolated.
func claudeSessionID(server, tmuxName string) string {
	h := sha1.Sum([]byte("cctl-claude-session\x00" + server + "\x00" + tmuxName))
	var u [16]byte
	copy(u[:], h[:16])
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// codexLaunchScript builds the full shell command tmux will execute for a
// `cctl codex` session: cd into the worktree (or repo) and run the OpenAI
// Codex CLI with the configured flags + optional prompt.
//
// It mirrors claudeLaunchScript but is deliberately simpler. Codex has no
// equivalent of claude's cmux notification wrapper or the cctl notify bridge,
// and — unlike claude's --session-id — codex cannot pin a session id at
// creation, so cctl can't key resume on a deterministic id. Instead we key on
// the working directory, the way `claude --continue` originally did: codex
// records each session's cwd under ~/.codex/sessions, so if any session for
// this directory exists we continue the most recent one (`codex resume
// --last`, which is cwd-filtered by default); otherwise we start fresh. If the
// probe misses (e.g. a future codex changes its on-disk layout) it falls back
// to a fresh session — a safe degradation, never a hard error.
//
// PATH is fixed up the same way as the claude launcher: a respawn or any
// non-login shell starts with a minimal PATH that omits the usual install
// dirs. Codex is commonly installed via npm (global prefix) or nvm, so we
// prepend the standard locations and source nvm if present.
//
// On codex exit the launcher waits for Enter, keeping the tmux session alive
// so you can detach (Ctrl-b d) and resume later — matching the claude path.
func codexLaunchScript(cwd string, codexFlags []string, prompt string) string {
	fresh := append([]string{"codex"}, codexFlags...)
	resume := append([]string{"codex", "resume", "--last"}, codexFlags...)
	if prompt != "" {
		fresh = append(fresh, prompt)
		resume = append(resume, prompt)
	}
	freshCmd := joinShell(fresh)
	resumeCmd := joinShell(resume)
	return fmt.Sprintf(`cd %s || {
  echo "cctl: worktree directory is gone — press Enter to close"
  read _ || true
  exit 1
}
# Ensure codex is found. A respawn or any non-login shell starts with a minimal
# PATH that omits the usual install dirs; codex is commonly installed via npm
# (global prefix) or nvm. Prepend the standard locations and source nvm so
# every launch path resolves codex.
export PATH="$HOME/.local/bin:$HOME/.npm-global/bin:$HOME/bin:$PATH"
if [ -s "$HOME/.nvm/nvm.sh" ]; then . "$HOME/.nvm/nvm.sh" >/dev/null 2>&1 || true; fi
# Resume THIS directory's most recent codex session if one is recorded, else
# start fresh. Codex can't pin a session id at creation, so we key resume on
# the working directory: codex records each session's cwd under
# ~/.codex/sessions, and "codex resume --last" is cwd-filtered by default.
sessdir="$HOME/.codex/sessions"
if [ -d "$sessdir" ] && grep -rqsF -- "$(pwd -P)" "$sessdir" 2>/dev/null; then
  %s
else
  %s
fi
status=$?
echo
echo "(codex exited with status $status — press Enter to close, Ctrl-b d to keep session)"
read _ || true`,
		shellPath(cwd),
		resumeCmd,
		freshCmd,
	)
}
