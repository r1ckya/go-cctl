package cctl

import (
	"strings"
	"testing"
)

// indexOf returns the byte offset of sub in s, or -1. Used to assert the
// structural ordering of generated shell scripts (AGENTS.md: script builders
// are tested for presence AND ordering).
func indexOf(s, sub string) int { return strings.Index(s, sub) }

func TestCodexLaunchScript_ResumeAndFresh(t *testing.T) {
	got := codexLaunchScript("/home/u/wt", []string{"--strict-config"}, "fix the bug")

	wantLines := []string{
		`cd "/home/u/wt"`,                                   // cd into the worktree (shellPath double-quotes it)
		`if [ -s "$HOME/.nvm/nvm.sh" ]; then`,               // nvm sourced so codex resolves
		`export PATH="$HOME/.local/bin`,                     // PATH fixup like the claude launcher
		`sessdir="$HOME/.codex/sessions"`,                   // resume keyed on cwd
		`grep -rqsF -- "$(pwd -P)" "$sessdir"`,              // cwd probe
		"codex resume --last --strict-config 'fix the bug'", // resume branch carries flags + prompt
		"codex --strict-config 'fix the bug'",               // fresh branch carries flags + prompt
		"read _ || true",                                    // keep tmux session alive on exit
	}
	for _, w := range wantLines {
		if !strings.Contains(got, w) {
			t.Errorf("codexLaunchScript missing %q\n--- script ---\n%s", w, got)
		}
	}

	// Ordering: probe must come before the resume branch, and the resume
	// branch (then-clause) before the fresh branch (else-clause), or the
	// if/else is inverted.
	probe := indexOf(got, `grep -rqsF`)
	resume := indexOf(got, "\n  codex resume --last")  // the then-branch resume invocation
	fresh := indexOf(got, "\n  codex --strict-config") // the else-branch fresh invocation
	if !(probe < resume && resume < fresh) {
		t.Errorf("bad ordering: probe=%d resume=%d fresh=%d\n%s", probe, resume, fresh, got)
	}
}

func TestCodexLaunchScript_NoPromptOmitsPrompt(t *testing.T) {
	got := codexLaunchScript("/wt", nil, "")
	// With no flags and no prompt the invocations are bare.
	if !strings.Contains(got, "codex resume --last\n") {
		t.Errorf("expected bare `codex resume --last` resume line\n%s", got)
	}
	if !strings.Contains(got, "\n  codex\n") {
		t.Errorf("expected bare `codex` fresh line\n%s", got)
	}
	// A nil/empty prompt must never inject a stray quoted empty arg.
	if strings.Contains(got, "codex ''") || strings.Contains(got, "--last ''") {
		t.Errorf("empty prompt leaked an empty arg\n%s", got)
	}
}

func TestCodexLaunchScript_HasNoClaudeBridge(t *testing.T) {
	// Codex has no cmux wrapper / notify-bridge; make sure none of claude's
	// bridge machinery leaks into the codex launcher.
	got := codexLaunchScript("/wt", []string{"--strict-config"}, "")
	for _, bad := range []string{"claude", "--session-id", "CMUX_WORKSPACE_ID", ".claude/projects"} {
		if strings.Contains(got, bad) {
			t.Errorf("codex launcher unexpectedly contains %q\n%s", bad, got)
		}
	}
}

func TestResolve_ThreadsCodexFlags(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{
			ClaudeFlags: []string{"--dangerously-skip-permissions"},
			CodexFlags:  []string{"--strict-config"},
		},
		Servers: map[string]Server{
			"local": {
				Local: true,
				Repos: map[string]Repo{
					"app": {Path: "/srv/app", DefaultBranch: "main"},
				},
			},
		},
	}
	r, err := cfg.resolve("local", "app")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(r.CodexFlags) != 1 || r.CodexFlags[0] != "--strict-config" {
		t.Errorf("CodexFlags not threaded into Resolved: got %v", r.CodexFlags)
	}
}
