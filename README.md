# cctl

A TUI for managing [Claude Code](https://claude.com/claude-code) sessions
across local and remote (mosh + ssh + tmux) git checkouts. Each session
lives in its own [git worktree](https://git-scm.com/docs/git-worktree) on
its own branch, so you can run multiple long-running agents on the same
repo without stomping on each other.

Built around [cmux](https://github.com/manaflow-ai/cmux) for the
multi-workspace UI: each worktree gets one cmux workspace and every
session on it opens as a tab inside that workspace (`t` adds a plain
terminal tab, no claude); re-attaching focuses the existing workspace
instead of duplicating it.

## Install

The recommended path is via [mise](https://mise.jdx.dev), which manages
both the Go toolchain and the resulting `cctl` shim in one command:

```bash
# Pin globally so cctl is always on $PATH:
mise use -g go:github.com/geoah/go-cctl/cmd/cctl@latest

# Or scope it to the current project's mise.toml:
mise use go:github.com/geoah/go-cctl/cmd/cctl@latest
```

mise will install a Go toolchain on demand, build the binary via
`go install`, and put it behind a shim called `cctl` on your `$PATH`.
Replace `@latest` with a tag (e.g. `@v0.1.0`) or commit SHA to pin.

If you'd rather use a system Go directly:

```bash
go install github.com/geoah/go-cctl/cmd/cctl@latest
```

Either way the binary is named `cctl`.

## Setup

cctl is config-driven — it needs to know which hosts to look at and
where to find git repos on them. Generate a starter config and edit it:

```bash
cctl init                       # writes ~/.cctl.yaml if absent
$EDITOR ~/.cctl.yaml
```

The minimum useful config has at least one server with one
`repo_sources` entry. A realistic example covering both a local box
and a remote workspace:

```yaml
defaults:
  branch_prefix: yourname        # branches go yourname/<name>
  worktree_base: ~/worktrees     # cctl puts every worktree here
  mosh: true                     # use mosh for remotes when available
  claude_flags: ["--dangerously-skip-permissions"]
  worktree_post_create:          # run inside each fresh worktree
    - mise trust
  # claude_update: claude update # what UU runs before restarting sessions

servers:
  local:
    local: true
    repo_sources:
      - path: ~/src/github.com   # walks recursively up to max_depth
        max_depth: 3
        default_branch: main

  workspace:
    host: 10.0.0.1
    user: you
    ssh_key: ~/.ssh/id_ed25519
    ssh_opts:
      - "-o"
      - "IdentitiesOnly=yes"
    # transport: cmux-ssh  # open sessions as cmux remote-SSH workspaces:
    #                      # Files panel lists the REMOTE fs, browser panes
    #                      # route through the remote, cmux CLI works there.
    #                      # Default (omitted) uses mosh/ssh in a local tab.
    repo_sources:
      - path: "~/"
        max_depth: 4
        default_branch: main
```

The knobs:

- **`servers.<name>`** — one block per host. `local: true` for the
  current machine; otherwise set `host:` + `user:` (+ optional
  `ssh_key:` / `ssh_opts:` for non-default ssh).
- **`repo_sources:`** — directories cctl walks (up to `max_depth`) to
  auto-discover git repos. Anything with a `.git` becomes a row in
  the tree without you having to enumerate it.
- **`repos:`** (optional, not shown) — explicit overrides for repos
  that live outside any `repo_sources` root or need a non-default
  branch.
- **`defaults.worktree_base`** — root for new worktrees
  (`<base>/<repo>/<worktree>`). The default is `~/worktrees`; cctl
  refuses to delete anything outside this path, so put it somewhere
  you're happy treating as scratch.
- **`defaults.worktree_post_create`** — list of shell commands run
  inside each freshly-created worktree (e.g. `mise trust`,
  `direnv allow`, `pre-commit install`). Best-effort; failures log
  but don't abort the session.
- **`defaults.branch_prefix`** — branches for new worktrees are named
  `<prefix>/<worktree-name>`.
- **`defaults.mosh: true`** uses [mosh](https://mosh.org) instead of
  ssh for remote sessions (better roaming + latency). Falls back
  to ssh per-server with `mosh: false`.

Re-run `cctl init` later for a refreshed example; it never overwrites
an existing `~/.cctl.yaml`. Run `cctl doctor` if a host or repo looks
wrong — it prints a per-server health report (transport, tools, repo
status, orphan sessions).

Then launch the TUI:

```bash
cctl
```

## What it does

- Discovers git repos under configured roots (local + remote).
- Lets you start a new Claude session on a repo (worktree auto-created)
  or attach to an existing one. Single keystroke for both.
- `cctl codex` is the OpenAI Codex equivalent of `cctl claude`: same
  target/session/worktree semantics, but it launches the `codex` CLI
  (configure flags via `codex_flags`). Codex has no
  `--session-id`, so resume is keyed on the worktree directory
  (`codex resume --last`).
- Names tmux sessions `cctl/<repo>/<worktree>/<session>` and matches them
  back into the tree, so the TUI is the single source of truth.
- Guards aggressively against deleting anything outside `worktree_base` —
  the repo itself can never be removed via `dd`.
- Per-server connection status (probe + retry/backoff) with spinners on
  rows that are still loading.

## Layout

- `cmd/cctl/main.go` — entry point (so `go install …/cmd/cctl@latest`
  produces a binary called `cctl`), thin shim around `pkg/cctl.Run()`.
- `pkg/cctl/` — all logic (TUI, CLI, spawner, config, discovery, scripts).
- `AGENTS.md` — guidance for AI agents and contributors editing the
  package (especially the testing rules).
- `.mise.toml` — task definitions for `mise run cctl:install` etc.

## License

MIT.
