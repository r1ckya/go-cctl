package cctl

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Spawner launches a wrapper script in a new tab/window of a terminal
// emulator (fire-and-forget). Implementations return nil on successful
// launch, an error otherwise; the TUI falls back to inline tea.ExecProcess
// when no Spawner succeeds.
//
// Each Spawner declares an Available() check so detection prefers
// terminals that are actually installed, and a Name() that matches the
// values accepted by `defaults.spawn` ("cmux" or "inline").
//
// `cwd` is a hint at the working directory the new tab/window should
// reflect. cmux uses it as the workspace path so the sidebar entry shows
// the worktree name. The inline spawner ignores it; the wrapper script
// does its own cd anyway.
//
// The mapping to cmux's model is one sidebar GROUP per repo, one
// workspace per WORKTREE, and one tab (surface) per SESSION. Spawning
// into a worktree that already has a workspace adds a tab there instead
// of creating another workspace; workspaces are added to their repo's
// group as they're created.
type Spawner interface {
	Name() string
	Available() bool
	Spawn(spec SpawnSpec) error
}

// SpawnSpec describes one open-this-session request to a Spawner.
type SpawnSpec struct {
	// Server, Repo, Worktree, Session identify the cctl session this spawn
	// represents. They drive the durable wrapper-script filename
	// (~/.cctl/spawn/<server>__<repo>__<worktree>__<session>.sh) and the
	// restore manifest entry. All four set => the spawn is tracked and
	// reboot-durable; any empty => it falls back to a disposable script and
	// isn't recorded. Repo/Worktree/Session are the REAL names (tmuxName
	// sanitizes them when building the tmux target).
	Server   string
	Repo     string
	Worktree string
	Session  string
	// Agent is the coding agent this session runs ("claude"/"codex"),
	// recorded in the manifest so revive/respawn relaunch the right one.
	Agent string
	// Script is the wrapper-script path; the only thing that must run.
	Script string
	// Cwd is the working-directory hint for a newly created workspace
	// (cmux uses it for the sidebar directory label + Files panel root
	// on local servers).
	Cwd string
	// WsTitle names the worktree's workspace: "repo/worktree".
	WsTitle string
	// TabTitle names the session's tab inside the workspace.
	TabTitle string
	// GroupTitle names the sidebar group the workspace belongs to — the
	// repo ("rxtx.dev" locally, "workspace/olympus" for remotes). Empty
	// disables grouping.
	GroupTitle string
	// GroupCwd seeds the group anchor's working directory when the group
	// is first created (the repo checkout path on local servers). cmux
	// keys per-group cmux.json customization on this path.
	GroupCwd string
	// FocusExisting says reaching the existing workspace is enough —
	// don't run the script at all. Only the attach-to-a-live-session
	// path sets it: there a tab in that workspace already holds the
	// attached tmux client, and running a second client would just
	// mirror it. Everywhere else (new session, detached resurrect) the
	// wrapper script MUST actually run.
	FocusExisting bool
	// Remote, when set, requests a cmux remote-SSH workspace instead of
	// a local one running the wrapper Script: the workspace is created
	// via `cmux ssh` and tabs run Remote.Command in remote shells. The
	// Script stays populated as the fallback when the cmux-ssh path
	// fails (older cmux, ssh auth quirks).
	Remote *RemoteSpawn
}

// RemoteSpawn describes the `cmux ssh` target for a remote-SSH workspace.
type RemoteSpawn struct {
	Destination string   // user@host
	Port        int      // 0 = default
	Identity    string   // expanded ssh key path, "" = default
	SSHOptions  []string // bare -o values ("IdentitiesOnly=yes", …)
	Command     string   // remote shell command (the idempotent tmux attach)
}

// ---- cmux ------------------------------------------------------------------

type cmuxSpawner struct{}

func (cmuxSpawner) Name() string { return "cmux" }

func (cmuxSpawner) Available() bool {
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/Applications/cmux.app", "/Applications/Cmux.app"} {
			if _, err := os.Stat(p); err == nil {
				return true
			}
		}
	}
	_, err := exec.LookPath("cmux")
	return err == nil
}

// Spawn maps cctl's tree onto cmux: one sidebar group per repo, one
// workspace per worktree (named `WsTitle`, "repo/worktree"), one tab per
// session (named `TabTitle`).
//
//   - workspace exists + FocusExisting → just select it (a tab in there
//     already holds the attached tmux client; running the script again
//     would only mirror the session).
//   - workspace exists → add a tab: new-surface, run the wrapper in it
//     (respawn-pane), rename the tab to the session name.
//   - no workspace → create one via `new-workspace --layout` with a single
//     terminal surface running the wrapper (bypasses cmux's default
//     template with its Files panel), then best-effort rename its tab.
//
// After either path the workspace is added to its repo's sidebar group
// (created on first use, anchored at the repo path). Every cmux call past
// the first is best-effort with logging: if adding a tab to an existing
// workspace fails (cmux version drift, surface-id parse failure, …) we
// fall back to creating a separate workspace so the session always opens
// SOMEWHERE, and a failed grouping never blocks the spawn.
//
// Auth: cmux's socket only accepts processes started inside cmux. When run
// from outside, the call errors and the TUI falls back to inline ExecProcess.
func (cmuxSpawner) Spawn(spec SpawnSpec) error {
	cli := cmuxCLIPath()
	if cli == "" {
		return fmt.Errorf("cmux CLI not found (install /Applications/cmux.app)")
	}
	cwd := spec.Cwd
	if cwd == "" {
		cwd, _ = os.UserHomeDir()
	}
	// The command a tab in this workspace should run: the local wrapper
	// script normally, the raw remote command in a cmux-ssh workspace
	// (the surface shell already lives on the remote there — a local
	// script path would mean nothing to it).
	tabCommand := spec.Script
	if spec.Remote != nil {
		tabCommand = spec.Remote.Command
	}
	if spec.WsTitle != "" {
		if id, ok := findCmuxWorkspaceByName(cli, spec.WsTitle); ok {
			// Workspaces created before grouping existed (or whose group
			// was dissolved) get healed into their repo's group here.
			ensureCmuxGroupMembership(cli, spec.GroupTitle, spec.GroupCwd, id)
			if spec.FocusExisting {
				out, err := exec.Command(cli, "select-workspace", "--workspace", id).CombinedOutput()
				if err == nil {
					log().Info("cmux-focus-existing", "id", id, "ws", spec.WsTitle)
					return nil
				}
				log().Debug("cmux-select-workspace-fail", "id", id, "ws", spec.WsTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
				// fall through to add-tab: running the wrapper is still
				// correct, just less elegant than focusing.
			}
			if err := addCmuxTab(cli, id, spec, tabCommand); err == nil {
				log().Info("cmux-add-tab", "id", id, "ws", spec.WsTitle, "tab", spec.TabTitle)
				return nil
			} else {
				log().Warn("cmux-add-tab-fail (falling back to new workspace)",
					"id", id, "ws", spec.WsTitle, "tab", spec.TabTitle, "err", err.Error())
			}
		}
	}
	if spec.Remote != nil {
		if err := spawnCmuxSSHWorkspace(cli, spec); err == nil {
			return nil
		} else if spec.Script == "" {
			return err
		} else {
			log().Warn("cmux-ssh-spawn-fail (falling back to local wrapper workspace)", "ws", spec.WsTitle, "err", err.Error())
		}
	}
	args, err := cmuxNewWorkspaceArgs(spec.Script, cwd, spec.WsTitle)
	if err != nil {
		return fmt.Errorf("cmux build args: %w", err)
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cmux new-workspace: %w: %s", err, strings.TrimSpace(string(out)))
	}
	finishCmuxWorkspace(cli, spec)
	return nil
}

// spawnCmuxSSHWorkspace creates a remote-SSH workspace via `cmux ssh`:
// cmux opens the transport, bootstraps its remote daemon (Files panel
// over ssh, browser proxying, CLI relay for notifications), and runs the
// remote command — our idempotent tmux attach — in the first tab.
func spawnCmuxSSHWorkspace(cli string, spec SpawnSpec) error {
	args := cmuxSSHArgs(spec)
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("cmux ssh: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log().Info("cmux-ssh-workspace", "ws", spec.WsTitle, "dest", spec.Remote.Destination)
	finishCmuxWorkspace(cli, spec)
	return nil
}

// cmuxSSHArgs builds the `cmux ssh` argv for a remote-SSH workspace.
// Split out for testability. The remote command goes through `sh -lc` so
// the user's login PATH applies (tmux may live in /usr/local/bin etc.).
func cmuxSSHArgs(spec SpawnSpec) []string {
	r := spec.Remote
	args := []string{"ssh", r.Destination}
	if spec.WsTitle != "" {
		args = append(args, "--name", spec.WsTitle)
	}
	if r.Port != 0 {
		args = append(args, "--port", fmt.Sprintf("%d", r.Port))
	}
	if r.Identity != "" {
		args = append(args, "--identity", r.Identity)
	}
	for _, opt := range r.SSHOptions {
		args = append(args, "--ssh-option", opt)
	}
	args = append(args, "--", "sh", "-lc", r.Command)
	return args
}

// finishCmuxWorkspace applies the post-create cosmetics shared by both
// workspace flavors: label the first tab with the session name and file
// the workspace under its repo group. Best-effort.
func finishCmuxWorkspace(cli string, spec SpawnSpec) {
	if spec.WsTitle == "" {
		return
	}
	// Retry the lookup — `cmux new-workspace` can return before the new
	// workspace is queryable via list-workspaces over the automation socket.
	// A single lookup here races: on a miss the whole post-create block
	// (tab-rename + grouping) is silently skipped.
	id, ok := findCmuxWorkspaceByNameRetry(cli, spec.WsTitle)
	if !ok {
		return
	}
	if spec.TabTitle != "" {
		// Rename by the surface's real id — the "tab:1" ref doesn't reliably
		// resolve right after new-workspace, which left first tabs showing the
		// raw wrapper-script path while later tabs (added via addCmuxTab,
		// renamed by surface UUID) showed correctly.
		tabRef := "tab:1"
		if sid := firstCmuxSurfaceID(cli, id); sid != "" {
			tabRef = sid
		}
		out, err := exec.Command(cli, "rename-tab", "--workspace", id, "--tab", tabRef, spec.TabTitle).CombinedOutput()
		if err != nil {
			log().Debug("cmux-rename-tab-fail", "id", id, "tab", spec.TabTitle, "ref", tabRef, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		}
	}
	ensureCmuxGroupMembership(cli, spec.GroupTitle, spec.GroupCwd, id)
}

// ensureCmuxGroupMembership files a workspace under the sidebar group
// named `group`, creating the group when it doesn't exist yet (anchored
// at groupCwd — the repo checkout). Adding a workspace that's already a
// member is harmless. Purely cosmetic, so failures only log.
//
// Note: `workspace-group create` defaults --from to the user's current
// sidebar selection, so we always pass the workspace id explicitly.
func ensureCmuxGroupMembership(cli, group, groupCwd, wsID string) error {
	if group == "" || wsID == "" {
		return nil
	}
	if gid, ok := findCmuxGroupByName(cli, group); ok {
		out, err := cmuxCmd(cli, "workspace-group", "add", "--group", gid, "--workspace", wsID).CombinedOutput()
		if err != nil {
			// Visible (Warn): a failed add is why a workspace stays ungrouped.
			log().Warn("cmux-group-add-fail", "group", group, "gid", gid, "ws", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			return fmt.Errorf("group-add %q: %w", group, err)
		}
		log().Debug("cmux-group-add", "group", group, "gid", gid, "ws", wsID)
		return nil
	}
	args := []string{"workspace-group", "create", "--name", group, "--from", wsID}
	if groupCwd != "" {
		args = append(args, "--cwd", groupCwd)
	}
	if out, err := cmuxCmd(cli, args...).CombinedOutput(); err != nil {
		log().Warn("cmux-group-create-fail", "group", group, "ws", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		return fmt.Errorf("group-create %q: %w", group, err)
	}
	log().Info("cmux-group-create", "group", group, "ws", wsID)
	return nil
}

// findCmuxGroupByName looks a sidebar group up by exact name via
// `workspace-group list --json` and returns its id. The JSON payload is
// {"groups":[{"id":…,"name":…,…}]} (see cmux's v2WorkspaceGroupPayload).
func findCmuxGroupByName(cli, name string) (string, bool) {
	out, err := cmuxCmd(cli, "workspace-group", "list", "--json").Output()
	if err != nil {
		return "", false
	}
	return parseCmuxGroupList(string(out), name)
}

// parseCmuxGroupList extracts the id of the group with the given name
// from `workspace-group list --json` output. Split from the exec call so
// the parsing is unit-testable. Falls back to the "ref" handle when "id"
// is absent — both are accepted by group-targeting commands.
func parseCmuxGroupList(raw, name string) (string, bool) {
	var payload struct {
		Groups []struct {
			ID   string `json:"id"`
			Ref  string `json:"ref"`
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		log().Debug("cmux-group-list-parse-fail", "err", err.Error())
		return "", false
	}
	for _, g := range payload.Groups {
		if g.Name != name {
			continue
		}
		if g.ID != "" {
			return g.ID, true
		}
		if g.Ref != "" {
			return g.Ref, true
		}
	}
	return "", false
}

// addCmuxTab makes the session's tab exist in an existing workspace and run
// `tabCommand`. If a same-titled tab is already there — the common case on
// restore, where cmux re-created a now-dead tab — it respawns that tab in
// place instead of stacking a duplicate; otherwise it opens a fresh surface.
// Returns an error if the surface can't be created or its id can't be
// determined — the caller falls back to a separate workspace in that case.
//
// No cmux "resume binding" is set: those accumulate in cmux.json and trigger
// cmux's restore prompts. cmux replays a surface's own command on restore,
// and the command here is cctl's durable ~/.cctl/spawn wrapper, so a reboot
// re-runs `tmux new-session -A` (→ claude --continue) without a binding.
func addCmuxTab(cli, wsID string, spec SpawnSpec, tabCommand string) error {
	// Reuse an existing same-titled tab when present: this is what makes
	// restore idempotent (no duplicate tabs).
	if spec.TabTitle != "" {
		if sid, ok := findCmuxSurfaceByTitle(cli, wsID, spec.TabTitle); ok {
			if out, err := exec.Command(cli, "respawn-pane", "--workspace", wsID, "--surface", sid, "--command", tabCommand).CombinedOutput(); err != nil {
				return fmt.Errorf("respawn-pane (heal existing tab): %w: %s", err, strings.TrimSpace(string(out)))
			}
			if out, err := exec.Command(cli, "select-workspace", "--workspace", wsID).CombinedOutput(); err != nil {
				log().Debug("cmux-select-workspace-fail", "id", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
			}
			log().Info("cmux-heal-tab", "ws", wsID, "surface", sid, "tab", spec.TabTitle)
			return nil
		}
	}
	out, err := exec.Command(cli, "new-surface", "--workspace", wsID, "--focus", "true").CombinedOutput()
	if err != nil {
		return fmt.Errorf("new-surface: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sid := parseCmuxSurfaceID(string(out))
	if sid == "" {
		// new-surface's output didn't include a recognizable id; ask for
		// the workspace's surfaces and take the newest (listed last).
		lout, lerr := exec.Command(cli, "--id-format", "uuids", "list-pane-surfaces", "--workspace", wsID).Output()
		if lerr == nil {
			sid = lastCmuxListedID(string(lout))
		}
	}
	if sid == "" {
		return fmt.Errorf("could not determine new surface id (new-surface said: %s)", strings.TrimSpace(string(out)))
	}
	// The fresh surface runs the user's default shell; respawn-pane sends
	// it the wrapper-script path to execute. When the wrapper exits the
	// shell survives, so the tab stays usable.
	if out, err := exec.Command(cli, "respawn-pane", "--workspace", wsID, "--surface", sid, "--command", tabCommand).CombinedOutput(); err != nil {
		return fmt.Errorf("respawn-pane: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if spec.TabTitle != "" {
		if out, err := exec.Command(cli, "rename-tab", "--workspace", wsID, "--tab", sid, spec.TabTitle).CombinedOutput(); err != nil {
			log().Debug("cmux-rename-tab-fail", "surface", sid, "tab", spec.TabTitle, "err", err.Error(), "out", strings.TrimSpace(string(out)))
		}
	}
	// Land the user on the workspace; the new surface was created with
	// --focus true so the right tab is already selected within it.
	if out, err := exec.Command(cli, "select-workspace", "--workspace", wsID).CombinedOutput(); err != nil {
		log().Debug("cmux-select-workspace-fail", "id", wsID, "err", err.Error(), "out", strings.TrimSpace(string(out)))
	}
	return nil
}

// cmuxUUIDRe / cmuxSurfaceRefRe match the two id shapes the cmux CLI
// prints depending on --id-format: full UUIDs and short "surface:N" refs.
var (
	cmuxUUIDRe       = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	cmuxSurfaceRefRe = regexp.MustCompile(`surface:\d+`)
)

// parseCmuxSurfaceID pulls a surface identifier out of `new-surface`
// output, accepting either a UUID or a "surface:N" short ref anywhere in
// the text. Returns "" when neither appears.
func parseCmuxSurfaceID(out string) string {
	if m := cmuxUUIDRe.FindString(out); m != "" {
		return m
	}
	if m := cmuxSurfaceRefRe.FindString(out); m != "" {
		return m
	}
	return ""
}

// lastCmuxListedID returns the first token of the last non-empty line —
// the id column of the newest entry in a cmux list-* output.
func lastCmuxListedID(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return strings.Fields(line)[0]
	}
	return ""
}

// findCmuxWorkspaceByName returns the UUID of the first workspace whose
// name matches the given string, or "" + false when none match (including
// the case where the cmux socket isn't reachable from this process).
//
// Output format from `cmux --id-format uuids list-workspaces` is one
// workspace per line; each line starts with the UUID and is followed by
// the workspace name and any extra metadata cmux chooses to include.
// We accept "uuid<sep>name" with whitespace separation and trim the
// match candidate before comparison.
// findCmuxWorkspaceByNameRetry is findCmuxWorkspaceByName with a short retry
// loop (~3s total). Used right after `cmux new-workspace`, which can return
// before the new workspace shows up in `list-workspaces` over the automation
// socket — a single lookup there races and skips tab-rename/grouping.
func findCmuxWorkspaceByNameRetry(cli, name string) (string, bool) {
	for i := 0; i < 20; i++ {
		if id, ok := findCmuxWorkspaceByName(cli, name); ok {
			return id, true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", false
}

// firstCmuxSurfaceID returns the id of a freshly created workspace's first
// (only) surface, retrying briefly because new-workspace can return before its
// surface is queryable over the automation socket. Returns "" if none shows
// up, leaving the caller to fall back to a tab ref.
func firstCmuxSurfaceID(cli, wsID string) string {
	for i := 0; i < 20; i++ {
		out, err := exec.Command(cli, "--id-format", "uuids", "list-pane-surfaces", "--workspace", wsID).Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
				if id := cmuxUUIDRe.FindString(line); id != "" {
					return id
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return ""
}

func findCmuxWorkspaceByName(cli, name string) (string, bool) {
	target := strings.TrimSpace(name)
	for _, ws := range listCmuxWorkspaces(cli) {
		if ws.name == target {
			return ws.id, true
		}
	}
	return "", false
}

// findCmuxWorkspaceNameBySafeTitle matches workspaces through tmux's
// name sanitization: events parsed from tmux session names carry
// sanitized components ("rxtx_dev/main") while workspace titles use the
// real names ("rxtx.dev/main"). Returns the real workspace name so
// callers can target it directly.
func findCmuxWorkspaceNameBySafeTitle(cli, safeTitle string) (string, bool) {
	target := strings.TrimSpace(safeTitle)
	for _, ws := range listCmuxWorkspaces(cli) {
		if tmuxSafeName(ws.name) == target {
			return ws.name, true
		}
	}
	return "", false
}

type cmuxWorkspace struct {
	id   string
	name string
}

func listCmuxWorkspaces(cli string) []cmuxWorkspace {
	out, err := exec.Command(cli, "--id-format", "uuids", "list-workspaces").Output()
	if err != nil {
		return nil
	}
	return parseCmuxWorkspaceList(string(out))
}

// parseCmuxWorkspaceList parses `--id-format uuids list-workspaces`: one
// workspace per line, UUID first, then the name (which may contain spaces).
// Split from the exec call so the parsing is unit-testable.
//
// cmux marks the SELECTED row with a leading "*" and a trailing "[selected]"
// status marker. Neither is part of the id or the name, so they must be
// stripped: otherwise the selected workspace parses with id "*" and a name
// that includes the UUID and "[selected]", which never matches a real name.
// findCmuxWorkspaceByName is then blind to it, so spawn's "does it already
// exist?" check (and reconcile's existing-set) miss it and create a DUPLICATE
// workspace for the same session. A freshly created workspace is auto-focused
// (`new-workspace --focus true`), i.e. exactly the selected row — so the very
// next lookup for it would otherwise always miss.
func parseCmuxWorkspaceList(raw string) []cmuxWorkspace {
	var result []cmuxWorkspace
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// Drop the leading "*" selected-row marker.
		if len(fields) > 0 && fields[0] == "*" {
			fields = fields[1:]
		}
		// Drop a trailing bracketed status marker (e.g. "[selected]").
		if n := len(fields); n > 0 && strings.HasPrefix(fields[n-1], "[") && strings.HasSuffix(fields[n-1], "]") {
			fields = fields[:n-1]
		}
		if len(fields) < 2 {
			continue
		}
		result = append(result, cmuxWorkspace{
			id:   fields[0],
			name: strings.TrimSpace(strings.Join(fields[1:], " ")),
		})
	}
	return result
}

// cmuxNewWorkspaceArgs builds the argv for `cmux new-workspace ...` with a
// single-terminal layout (no Files panel). Split out so the args are testable
// without spawning cmux.
func cmuxNewWorkspaceArgs(script, cwd, title string) ([]string, error) {
	layout, err := buildCmuxLayout(script)
	if err != nil {
		return nil, err
	}
	args := []string{"new-workspace"}
	if title != "" {
		args = append(args, "--name", title)
	}
	args = append(args,
		"--cwd", cwd,
		"--layout", layout,
		"--focus", "true",
	)
	return args, nil
}

// buildCmuxLayout returns a JSON layout describing a single pane containing
// one terminal surface running `command`. This matches the schema used by
// `cmux new-workspace --layout` and `cmux.json` layout definitions.
func buildCmuxLayout(command string) (string, error) {
	layout := map[string]any{
		"pane": map[string]any{
			"surfaces": []map[string]any{
				{"type": "terminal", "command": command},
			},
		},
	}
	b, err := json.Marshal(layout)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// notifyCmux posts a native cmux notification (sidebar badge +
// notification center) so task outcomes reach the user even when the
// cctl tab isn't focused. When wsTitle resolves to a workspace, the
// notification is attached to it (clicking jumps there). Fire-and-forget
// and best-effort: cctl works fine without a cmux socket.
func notifyCmux(title, body, wsTitle string) {
	cli := cmuxCLIPath()
	if cli == "" {
		return
	}
	args := []string{"notify", "--title", title}
	if body != "" {
		args = append(args, "--body", abbrev(body, 300))
	}
	if wsTitle != "" {
		if id, ok := findCmuxWorkspaceByName(cli, wsTitle); ok {
			args = append(args, "--workspace", id)
		}
	}
	if out, err := exec.Command(cli, args...).CombinedOutput(); err != nil {
		log().Debug("cmux-notify-fail", "title", title, "err", err.Error(), "out", strings.TrimSpace(string(out)))
	}
}

// cmuxCLIPath finds the cmux CLI binary. On macOS it's bundled inside the
// app at Contents/Resources/bin/cmux; users don't always have the symlink
// in $PATH (cmux ships a "Install CLI" action they may not have run).
func cmuxCLIPath() string {
	if runtime.GOOS == "darwin" {
		for _, p := range []string{
			"/Applications/cmux.app/Contents/Resources/bin/cmux",
			"/Applications/Cmux.app/Contents/Resources/bin/cmux",
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	if p, err := exec.LookPath("cmux"); err == nil {
		return p
	}
	return ""
}

// ---- registry + detection --------------------------------------------------

// allSpawners returns every concrete provider in a deterministic order.
// Today cctl ships only one — cmux — because "Enter always opens a new
// tab" is the design and cmux is the supported terminal. The interface
// is kept so future providers can plug in without touching call sites.
func allSpawners() []Spawner {
	return []Spawner{cmuxSpawner{}}
}

// detectSpawner returns the active Spawner. With only cmux supported,
// this is trivial — but kept as a function so the caller still gets a
// "reason" string for logging and we can plug new providers in later.
func detectSpawner(pref string) (Spawner, string) {
	pref = strings.ToLower(strings.TrimSpace(pref))
	if pref != "" && pref != "auto" {
		for _, s := range allSpawners() {
			if s.Name() == pref {
				return s, "config:" + pref
			}
		}
		log().Warn("unknown spawn provider in config, falling back to auto", "given", pref)
	}
	if looksLikeCmux() {
		return cmuxSpawner{}, "env=cmux(bundle)"
	}
	if strings.EqualFold(os.Getenv("TERM_PROGRAM"), "cmux") {
		return cmuxSpawner{}, "env=cmux"
	}
	return cmuxSpawner{}, "default"
}

// looksLikeCmux returns true when the current process appears to be running
// inside cmux even though $TERM_PROGRAM advertises "ghostty" (cmux is
// libghostty-based and inherits ghostty's TERM_PROGRAM). Heuristics:
//
//   - $__CFBundleIdentifier — macOS sets this for processes spawned by an
//     .app bundle, so cmux's "com.cmuxterm.app" identifies us.
//   - any CMUX_* environment variable — cmux sets workspace metadata as
//     env when it spawns shells.
func looksLikeCmux() bool {
	if id := strings.ToLower(os.Getenv("__CFBundleIdentifier")); strings.Contains(id, "cmux") {
		return true
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "CMUX_") {
			return true
		}
	}
	return false
}
