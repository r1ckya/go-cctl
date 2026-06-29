package cctl

import "testing"

func TestOtherAgent(t *testing.T) {
	if got := otherAgent(agentCodex); got != agentClaude {
		t.Errorf("otherAgent(codex) = %q, want claude", got)
	}
	if got := otherAgent(agentClaude); got != agentCodex {
		t.Errorf("otherAgent(claude) = %q, want codex", got)
	}
	// Toggling from an unset/unknown value lands on codex (so the first
	// ←/→ from a default that somehow wasn't set still does something useful).
	if got := otherAgent(""); got != agentCodex {
		t.Errorf("otherAgent(\"\") = %q, want codex", got)
	}
}

// TestManifestPersistsAgent pins the per-session agent round-trip: a session
// created as codex is recorded and read back as codex, so revive/respawn
// relaunch the right agent. HOME is redirected to a temp dir so the test never
// touches the real ~/.cctl manifest.
func TestManifestPersistsAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	manifestUpsert(SpawnSpec{
		Server: "vm", Repo: "olympus", Worktree: "bench", Session: "bench",
		Agent: agentCodex,
	})

	if got := manifestAgent("vm", "olympus", "bench", "bench"); got != agentCodex {
		t.Errorf("manifestAgent(created codex) = %q, want %q", got, agentCodex)
	}
	// An untracked session returns "" so callers fall back to the config agent.
	if got := manifestAgent("vm", "olympus", "bench", "missing"); got != "" {
		t.Errorf("manifestAgent(untracked) = %q, want empty", got)
	}
}
