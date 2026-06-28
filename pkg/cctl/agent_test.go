package cctl

import (
	"strings"
	"testing"
)

func cfgWithAgents(defAgent, srvAgent, repoAgent string) *Config {
	return &Config{
		Defaults: Defaults{Agent: defAgent},
		Servers: map[string]Server{
			"s": {
				Local: true,
				Agent: srvAgent,
				Repos: map[string]Repo{
					"app": {Path: "/srv/app", DefaultBranch: "main", Agent: repoAgent},
				},
			},
		},
	}
}

func TestResolveAgentPrecedence(t *testing.T) {
	cases := []struct {
		name           string
		def, srv, repo string
		want           string
	}{
		{"default-claude-when-unset", "", "", "", agentClaude},
		{"defaults-wins", "codex", "", "", agentCodex},
		{"server-overrides-defaults", "claude", "codex", "", agentCodex},
		{"repo-overrides-server", "claude", "claude", "codex", agentCodex},
		{"repo-overrides-to-claude", "codex", "codex", "claude", agentClaude},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := cfgWithAgents(tc.def, tc.srv, tc.repo).resolve("s", "app")
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if r.Agent != tc.want {
				t.Errorf("agent = %q, want %q", r.Agent, tc.want)
			}
		})
	}
}

func TestResolveAgentRejectsUnknown(t *testing.T) {
	_, err := cfgWithAgents("gpt", "", "").resolve("s", "app")
	if err == nil || !strings.Contains(err.Error(), "invalid agent") {
		t.Fatalf("expected invalid-agent error, got %v", err)
	}
}

func TestAgentLaunchScript_DispatchesByAgent(t *testing.T) {
	base := &Resolved{
		ServerName:  "s",
		Server:      Server{Local: true},
		ClaudeFlags: []string{"--dangerously-skip-permissions"},
		CodexFlags:  []string{"--strict-config"},
	}

	claude := *base
	claude.Agent = agentClaude
	got := agentLaunchScript(&claude, "/wt", "", "cctl/app/main/x")
	if !strings.Contains(got, "claude --session-id") {
		t.Errorf("claude agent should launch claude --session-id; got:\n%s", got)
	}
	if strings.Contains(got, "codex") {
		t.Errorf("claude agent script leaked codex:\n%s", got)
	}

	codex := *base
	codex.Agent = agentCodex
	got = agentLaunchScript(&codex, "/wt", "", "cctl/app/main/x")
	if !strings.Contains(got, "codex resume --last --strict-config") {
		t.Errorf("codex agent should launch codex resume --last --strict-config; got:\n%s", got)
	}
	if strings.Contains(got, "claude") {
		t.Errorf("codex agent script leaked claude:\n%s", got)
	}
}

func TestAgentUpdateCmd(t *testing.T) {
	c := &Config{}
	if got := c.agentUpdateCmd(agentClaude); got != "claude update" {
		t.Errorf("default claude update = %q", got)
	}
	if got := c.agentUpdateCmd(agentCodex); got != "codex update" {
		t.Errorf("default codex update = %q", got)
	}
	c.Defaults.ClaudeUpdate = "npm i -g @anthropic-ai/claude-code"
	c.Defaults.CodexUpdate = "npm i -g @openai/codex"
	if got := c.agentUpdateCmd(agentClaude); got != "npm i -g @anthropic-ai/claude-code" {
		t.Errorf("override claude update = %q", got)
	}
	if got := c.agentUpdateCmd(agentCodex); got != "npm i -g @openai/codex" {
		t.Errorf("override codex update = %q", got)
	}
}

func TestAgentUpdateScript_LoginShellAndNvm(t *testing.T) {
	got := agentUpdateScript("codex update")
	for _, want := range []string{
		"exec bash -lc 'codex update'",              // login shell so PATH additions apply
		"$HOME/.local/bin",                          // well-known install dirs prepended
		`for d in "$HOME"/.nvm/versions/node/*/bin`, // nvm version bins added for nvm-managed codex
		"export PATH=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("agentUpdateScript missing %q; got:\n%s", want, got)
		}
	}
}
