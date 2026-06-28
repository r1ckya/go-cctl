package cctl

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"
)

func newInitCmd() *cobra.Command {
	var (
		force      bool
		searchRoot string
		maxScan    int
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a starter ~/.cctl.yaml, or init a specific tool (tmux/ghostty/...)",
		Long: `init (no arg) scans $HOME (or --root) for git repos, finds the most
populated common ancestor, and writes a config that uses it as a
repo_source. Existing ~/.cctl.yaml files are not overwritten without
--force.

Subcommands configure individual tools so they play nicely together
(scroll, select, copy/paste) across local and mosh-attached tmux sessions:

  cctl init tmux       upsert managed block in ~/.tmux.conf
  cctl init ghostty    upsert managed block in ~/.config/ghostty/config
  cctl init mosh       version + scrollback hints (no edits)
  cctl init claude     verify claude install + projects dir
  cctl init all        do all of the above

Each tool subcommand accepts --server <name> to apply on a configured
remote (tmux is the only one that writes files remotely; mosh/claude
just report).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveConfigPath()
			if err != nil {
				return err
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s already exists — pass --force to overwrite", path)
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", path, err)
			}

			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("resolve $HOME: %w", err)
			}
			root := searchRoot
			if root == "" {
				root = home
			}
			root = expandPath(root)

			fmt.Fprintf(os.Stderr, "scanning %s for git repos (maxdepth=%d)...\n", root, maxScan)
			repos, err := findGitRepos(root, maxScan)
			if err != nil {
				return fmt.Errorf("scan %s: %w", root, err)
			}
			if len(repos) == 0 {
				return fmt.Errorf("no git repos found under %s — pass --root to point elsewhere", root)
			}

			source, count, depth := pickRepoSource(home, repos)
			sourceForConfig := tildify(home, source)

			username := "me"
			if u, err := user.Current(); err == nil && u.Username != "" {
				username = u.Username
			}

			fmt.Fprintf(os.Stderr, "found %d repo(s); using %s as repo_source (%d repos at depth %d)\n",
				len(repos), source, count, depth)

			content := renderInitConfig(username, sourceForConfig, depth)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			fmt.Fprintf(os.Stderr, "wrote %s\n", path)

			// Show a handful of discovered repos so the user can sanity-check.
			previewRepos(os.Stderr, source, repos, 8)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")
	cmd.Flags().StringVar(&searchRoot, "root", "", "directory to scan (default: $HOME)")
	cmd.Flags().IntVar(&maxScan, "max-depth", 5, "maximum directory depth to scan for .git")
	for _, sub := range newInitToolsCmds() {
		cmd.AddCommand(sub)
	}
	return cmd
}

// findGitRepos returns absolute paths of every git repo under root, up to
// maxDepth levels deep. .git directories are pruned so we don't descend into
// repo internals.
func findGitRepos(root string, maxDepth int) ([]string, error) {
	if _, err := exec.LookPath("find"); err == nil {
		// +1 because .git is one level below the repo dir.
		out, err := exec.Command("find", root, "-maxdepth", fmt.Sprint(maxDepth+1), "-type", "d", "-name", ".git", "-prune").Output()
		if err == nil {
			return parseFindOutput(string(out)), nil
		}
		// fall through to filepath.Walk on error
	}
	return walkGitRepos(root, maxDepth)
}

func parseFindOutput(s string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		repo := strings.TrimSuffix(line, "/.git")
		repo = strings.TrimSuffix(repo, ".git")
		if repo == "" {
			continue
		}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}

// walkGitRepos is a pure-Go fallback for systems without find(1).
func walkGitRepos(root string, maxDepth int) ([]string, error) {
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable
		}
		if !d.IsDir() {
			return nil
		}
		depth := strings.Count(filepath.Clean(path), string(os.PathSeparator)) - rootDepth
		if depth > maxDepth+1 {
			return filepath.SkipDir
		}
		base := d.Name()
		// Hidden dirs (except the user's home itself) are usually noise.
		if depth > 0 && base != ".git" && strings.HasPrefix(base, ".") {
			return filepath.SkipDir
		}
		if base == "node_modules" || base == "vendor" {
			return filepath.SkipDir
		}
		if base == ".git" {
			out = append(out, filepath.Dir(path))
			return filepath.SkipDir
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// pickRepoSource chooses the directory under home that best summarizes
// where the user keeps their repos. depth is measured in path components
// below home (0 = $HOME itself).
//
// Naive "highest count wins" picks $HOME for any layout that has one
// stray repo at the top (because $HOME covers literally every repo).
// The user almost always wants the deepest *cluster* — e.g. they have
// 7 repos under ~/src/github.com plus a stray ~/scratch, and the right
// answer is ~/src/github.com, not $HOME.
//
// We score each candidate ancestor as `count * (depth + 1)`. That keeps
// deeper ancestors competitive even with slightly lower counts, while
// still falling back to $HOME when no real cluster exists (single repo
// at $HOME/myrepo → only candidate is $HOME, score 1, picked).
// Tie-break: deeper wins, then lexicographic for stability.
func pickRepoSource(home string, repos []string) (path string, count, depth int) {
	type cand struct{ count, depth int }
	counts := map[string]cand{}
	for _, r := range repos {
		rel, err := filepath.Rel(home, r)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(rel, string(os.PathSeparator))
		// candidates are ancestors at depths 0..len(parts)-1 below home.
		for d := range parts {
			var anc string
			if d == 0 {
				anc = home
			} else {
				anc = filepath.Join(home, filepath.Join(parts[:d]...))
			}
			c := counts[anc]
			c.count++
			c.depth = d
			counts[anc] = c
		}
	}
	bestScore := 0
	for p, c := range counts {
		score := c.count * (c.depth + 1)
		better := score > bestScore ||
			(score == bestScore && c.depth > depth) ||
			(score == bestScore && c.depth == depth && p < path)
		if better {
			path, count, depth, bestScore = p, c.count, c.depth, score
		}
	}
	return path, count, depth
}

// tildify rewrites a home-relative absolute path back to ~-form for config
// readability. Paths outside $HOME are returned unchanged.
func tildify(home, p string) string {
	if p == home {
		return "~"
	}
	if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return p
}

func previewRepos(w *os.File, root string, repos []string, n int) {
	if len(repos) == 0 {
		return
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "discovered repos:")
	for i, r := range repos {
		if i >= n {
			fmt.Fprintf(w, "  ... and %d more\n", len(repos)-n)
			break
		}
		display := r
		if rel, err := filepath.Rel(root, r); err == nil && !strings.HasPrefix(rel, "..") {
			display = rel
		}
		fmt.Fprintf(w, "  • %s\n", display)
	}
}

// renderInitConfig is a tiny templater for the starter config so we don't drag
// in text/template for two substitutions.
func renderInitConfig(username, source string, depthFromHome int) string {
	// max_depth is how far below source we look for .git. We allow at least
	// two extra levels beyond what we already saw (provider/org/repo style).
	maxDepth := 3
	if depthFromHome <= 1 {
		maxDepth = 4
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# ~/.cctl.yaml — generated by `cctl init`\n")
	b.WriteString("defaults:\n")
	fmt.Fprintf(&b, "  branch_prefix: %s          # worktree branch is <branch_prefix>/<session>\n", username)
	b.WriteString("  worktree_base: ~/worktrees    # local-side path; per-repo override allowed\n")
	b.WriteString("  claude_flags: [\"--dangerously-skip-permissions\"]\n")
	b.WriteString("  # codex_flags: []            # flags for `cctl codex` (OpenAI Codex CLI)\n")
	b.WriteString("  # agent: codex                # which agent the TUI launches (claude|codex); override per server/repo\n")
	b.WriteString("  mosh: true                    # ignored for local servers\n\n")
	b.WriteString("servers:\n")
	b.WriteString("  local:\n")
	b.WriteString("    local: true\n")
	b.WriteString("    repo_sources:\n")
	fmt.Fprintf(&b, "      - path: %s\n", source)
	fmt.Fprintf(&b, "        max_depth: %d\n", maxDepth)
	b.WriteString("        default_branch: main\n\n")
	b.WriteString("    # repos: optional explicit overrides — entries here win over discovery.\n")
	b.WriteString("    # repos:\n")
	b.WriteString("    #   custom:\n")
	b.WriteString("    #     path: ~/some/other/place\n")
	b.WriteString("    #     default_branch: develop\n\n")
	b.WriteString("  # Add remote servers below. Example:\n")
	b.WriteString("  # workspace:\n")
	b.WriteString("  #   host: 34.172.11.54\n")
	b.WriteString("  #   user: " + username + "\n")
	b.WriteString("  #   ssh_key: ~/.ssh/id_ed25519\n")
	b.WriteString("  #   repo_sources:\n")
	b.WriteString("  #     - path: ~/src\n")
	b.WriteString("  #       max_depth: 3\n")
	return b.String()
}
