// Package cctl implements `cctl` — a TUI + CLI for managing
// mosh+tmux+claude sessions across local and remote git checkouts.
//
// Quick start:
//
//	cctl claude <server> <session> [--repo R] [--no-worktree] [--branch B] [-- <prompt...>]
//	cctl ls [server]
//	cctl rm <server> <session>
//	cctl rma <server>
//
// See the repository README for the full config schema.
package cctl

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is the semver from version.txt (managed by `mise run release`),
// injected at build time via -ldflags (see the cctl:install mise task).
// Defaults to "dev" for a plain `go build`; versionString() then falls back
// to the module build info.
var Version = "dev"

var configPath string

var rootCmd = &cobra.Command{
	Use:           "cctl",
	Short:         "Manage mosh+tmux+claude sessions across remote servers",
	SilenceUsage:  true,
	SilenceErrors: true,
	Args:          cobra.NoArgs,
	Long: `cctl manages create/resume/list/teardown of tmux sessions running
Claude Code on remote servers, with optional git-worktree branches.

Running cctl with no arguments opens the interactive TUI. The subcommands
below are for scripting and direct CLI use.

Examples:
  cctl                                  # open the TUI
  cctl claude workspace audit -- investigate the auth bug
  cctl claude local/my-app hotfix --no-worktree
  cctl ls
  cctl rm local/my-app hotfix
  cctl rma workspace --yes
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runTUI()
	},
}

func init() {
	rootCmd.Version = versionString()
	rootCmd.PersistentFlags().StringVar(&configPath, "config", "", "path to cctl config (default: $CCTL_CONFIG or ~/.cctl.yaml)")

	rootCmd.AddCommand(newClaudeCmd())
	rootCmd.AddCommand(newCodexCmd())
	rootCmd.AddCommand(newLsCmd())
	rootCmd.AddCommand(newRmCmd())
	rootCmd.AddCommand(newRmaCmd())
	rootCmd.AddCommand(newServersCmd())
	rootCmd.AddCommand(newReposCmd())
	rootCmd.AddCommand(newSSHCmd())
	rootCmd.AddCommand(newConfigCmd())
	rootCmd.AddCommand(newInitCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newVersionCmd())
}

// Run is the package's CLI entry point. It wires up logging, executes the
// cobra root command, and exits the process with a non-zero status on
// failure. Intended to be called from a thin top-level main.go.
func Run() {
	initLogger()
	log().Debug("argv", "args", os.Args)
	if err := rootCmd.Execute(); err != nil {
		log().Error("execute-failed", "err", err.Error())
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
