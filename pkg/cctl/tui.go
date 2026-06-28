package cctl

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// runTUI loads the config and runs the interactive TUI. Returns when the user
// quits or an unrecoverable error occurs.
func runTUI() error {
	cfg, path, err := loadConfig()
	if err != nil {
		return err
	}
	log().Info("tui-start", "config", path, "servers", len(cfg.Servers))
	p := tea.NewProgram(newTUIModel(cfg), tea.WithAltScreen())
	final, err := p.Run()
	// Kill the notification-bridge ssh tails on every exit path so they
	// can't outlive the TUI.
	if fm, ok := final.(*tuiModel); ok {
		fm.stopNotifyWatchers()
	}
	if err != nil {
		log().Error("tui-error", "err", err.Error())
		return err
	}
	log().Info("tui-stop")
	if fm, ok := final.(*tuiModel); ok && fm.exitErr != nil {
		return fm.exitErr
	}
	return nil
}

// ---- model -----------------------------------------------------------------

type rowKind int

const (
	rowServer rowKind = iota
	rowRepo
	rowWorktree
	rowSession
	rowEmpty
	rowLoading
)

// treeRow is one logical row of the tree. The interpretation of fields varies
// by kind: server-rows have only `server`, repo-rows add `repo`,
// worktree-rows add `worktree`, session-rows fill all of `server/repo/
// worktree/session`. Empty/loading rows carry a `note` for display.
type treeRow struct {
	kind     rowKind
	server   string
	repo     string
	worktree string
	session  *SessionInfo
	wt       *Worktree // populated for rowWorktree (and inherited where useful)
	note     string
	depth    int
}

type tuiMode int

const (
	modeBrowse tuiMode = iota
	modeNewForm
	modeFilter
)

// serverState bundles all data fetched for one server. Worktrees and sessions
// load in parallel; rendering tolerates partial state ("loading...").
type serverState struct {
	repos           map[string]Repo
	worktrees       map[string][]Worktree // repoName -> worktrees
	repoStatus      map[string]RepoStatus // repoName -> OK/MISSING/NO_GIT (from listAllWorktrees)
	sessions        []SessionInfo
	sessionErr      error
	repoErr         error
	wtErr           error
	sessionsLoaded  bool
	reposLoaded     bool
	worktreesLoaded bool

	// Connection lifecycle for remote servers. Local servers skip the
	// probe and stay in connConnected from start.
	conn         connState
	connAttempts int   // number of probes issued (including current/last)
	connErr      error // last probe failure
	// pendingSync marks that this server (re)connected and, once it finishes
	// loading, should trigger a cmux↔cctl sync. Set on every probe success
	// (including a manual refresh), cleared when the sync is requested. This
	// is what makes "refresh a remote → sync runs" work post-startup.
	pendingSync bool
}

// ensureNotifyWatcher starts (or restarts after a transport drop) the
// claude→cmux notification bridge for a remote server. Local servers are
// covered natively by cmux's claude wrapper.
func (m *tuiModel) ensureNotifyWatcher(serverName string) {
	srv, ok := m.cfg.Servers[serverName]
	if !ok || srv.Local {
		return
	}
	if m.notifyWatchers == nil {
		m.notifyWatchers = map[string]*notifyWatcher{}
	}
	if w := m.notifyWatchers[serverName]; w != nil && !w.isDead() {
		return
	}
	m.notifyWatchers[serverName] = newNotifyWatcher(serverName, srv, m.cfg.betaRemoteClaudeStatus())
}

// stopNotifyWatchers kills every bridge transport; called when the TUI
// exits so no ssh tail outlives the process.
func (m *tuiModel) stopNotifyWatchers() {
	for _, w := range m.notifyWatchers {
		if w != nil && w.stop != nil {
			w.stop()
		}
	}
}

// normalizeSessionNames maps the tmux-sanitized repo/worktree components
// of loaded sessions back to their real names ("rxtx_dev" → "rxtx.dev")
// using the repo and worktree lists, once those are loaded. tmux replaces
// '.'/':' in session names, so the parsed-back components don't textually
// match repos/worktrees whose names contain dots; without this remap such
// sessions grouped under a ghost repo row that resolve() refused to act
// on. Exact matches always win — a repo literally named "rxtx_dev" keeps
// its sessions even if "rxtx.dev" also exists.
//
// Called whenever sessions, repos, or worktrees finish (re)loading, since
// the three fetches land in any order.
func (st *serverState) normalizeSessionNames() {
	if len(st.sessions) == 0 {
		return
	}
	repoBySafe := map[string]string{}
	for name := range st.repos {
		repoBySafe[tmuxSafeName(name)] = name
	}
	for i := range st.sessions {
		s := &st.sessions[i]
		if _, exact := st.repos[s.Repo]; !exact {
			if real, ok := repoBySafe[s.Repo]; ok {
				s.Repo = real
			}
		}
		wts := st.worktrees[s.Repo]
		exact := false
		for _, wt := range wts {
			if wt.Name == s.Worktree {
				exact = true
				break
			}
		}
		if !exact {
			for _, wt := range wts {
				if tmuxSafeName(wt.Name) == s.Worktree {
					s.Worktree = wt.Name
					break
				}
			}
		}
	}
}

// connState is the reachability of a server's transport (ssh/mosh). It's
// independent of repo/session health — those can fail (missing repo,
// permission, etc.) on a server that's perfectly reachable; we don't want
// to flip "disconnected" because of that.
type connState int

const (
	connConnecting connState = iota
	connConnected
	connDisconnected
)

// bgTask is one tracked background operation (a delete, a prepare+spawn).
// The TUI used to keep a single status slot + a single preparing-row key,
// which made concurrent actions flaky: a second dd would overwrite the
// first one's spinner and its outcome message. Now every async action is a
// task: the footer lists everything running (and recently finished), the
// targeted rows render a spinner, and conflicting actions on the same
// target are refused while one is in flight.
type bgTask struct {
	id      int
	key     string // rowKey of the target; "" for anonymous notices
	label   string
	detail  string // sub-line, usually the error text on failure
	started time.Time
	pending bool // queued but not started yet (shown with ○, no spinner)
	done    bool
	failed  bool
	clearAt time.Time // done only: when the entry drops from the footer
}

// spinnerFrames is a 10-step Braille spinner (industry standard, available
// in any monospace font). Frame index wraps mod len.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// tickMsg is the periodic beat that advances spinner animations and
// clears auto-expiring status states.
type tickMsg struct{}

// tickInterval is the spinner refresh rate. 100ms ≈ 10fps, smooth without
// burning CPU on terminals that re-render the whole frame.
const tickInterval = 100 * time.Millisecond

// needTick reports whether anything in the model is animating right now.
// Used to decide whether to schedule another tick after the current one.
func (m *tuiModel) needTick() bool {
	for _, t := range m.tasks {
		// A pending task doesn't animate by itself — it flips to running on a
		// state-change message, which re-renders anyway. Counting it here
		// would spin the tick loop forever while nothing moves.
		if !t.done && !t.pending {
			return true
		}
		if !t.clearAt.IsZero() && time.Now().Before(t.clearAt) {
			return true
		}
	}
	for _, st := range m.state {
		if st == nil {
			continue
		}
		if st.conn == connConnecting {
			return true
		}
		if st.conn == connConnected && (!st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded) {
			return true
		}
	}
	return false
}

// scheduleTick returns a tea.Cmd that ticks after tickInterval IF the
// model needs animation. Idempotent: if a tick is already pending the
// returned cmd is nil so we don't accumulate timers.
func (m *tuiModel) scheduleTick() tea.Cmd {
	if !m.needTick() {
		return nil
	}
	if m.tickPending {
		return nil
	}
	m.tickPending = true
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// startTask registers a running background task targeting the row with
// the given key and returns its id plus a tick cmd to start animating.
func (m *tuiModel) startTask(key, label string) (int, tea.Cmd) {
	m.nextTaskID++
	m.tasks = append(m.tasks, &bgTask{
		id:      m.nextTaskID,
		key:     key,
		label:   label,
		started: time.Now(),
	})
	log().Debug("tui-task-start", "id", m.nextTaskID, "key", key, "label", label)
	return m.nextTaskID, m.scheduleTick()
}

// startPendingTask registers a queued task that hasn't started running yet —
// shown in the queue with a ○ instead of a spinner. markTaskRunning flips it
// to active. Used for the startup "sync" step so the user sees it waiting in
// line behind the per-server connects.
func (m *tuiModel) startPendingTask(key, label string) int {
	m.nextTaskID++
	m.tasks = append(m.tasks, &bgTask{
		id:      m.nextTaskID,
		key:     key,
		label:   label,
		started: time.Now(),
		pending: true,
	})
	return m.nextTaskID
}

// markTaskRunning flips a pending task to running and (re)stamps its start
// time so the elapsed clock counts from when it actually began.
func (m *tuiModel) markTaskRunning(id int) {
	for _, t := range m.tasks {
		if t.id == id {
			t.pending = false
			t.started = time.Now()
			return
		}
	}
}

// finishTask marks the task done. A non-empty label replaces the running
// one (so "deleting X…" becomes "killed X"); err != nil renders the entry
// as a failure and keeps it on screen longer so the user can read it.
// Unknown ids are tolerated (the task may have been created by an older
// code path) — the outcome is shown as a fresh already-done entry.
func (m *tuiModel) finishTask(id int, label string, err error) tea.Cmd {
	var t *bgTask
	for _, c := range m.tasks {
		if c.id == id {
			t = c
			break
		}
	}
	if t == nil {
		m.nextTaskID++
		t = &bgTask{id: m.nextTaskID, started: time.Now()}
		m.tasks = append(m.tasks, t)
	}
	if label != "" {
		t.label = label
	}
	t.done = true
	t.failed = err != nil
	if err != nil {
		t.detail = err.Error()
		t.clearAt = time.Now().Add(10 * time.Second)
	} else {
		t.clearAt = time.Now().Add(4 * time.Second)
	}
	log().Debug("tui-task-done", "id", t.id, "label", t.label, "err", errString(err))
	// Surface outcomes the user may have walked away from as native cmux
	// notifications: every failure, and successes of long-running tasks
	// (claude upgrades, restores). Quick successes stay footer-only —
	// the user is still looking at them.
	if t.failed || time.Since(t.started) > 10*time.Second {
		title := "cctl: " + t.label
		detail := t.detail
		go notifyCmux(title, detail, "")
	}
	return m.scheduleTick()
}

// noteSuccess / noteFailure record an instant outcome (no running phase) —
// the replacement for the old one-slot status bar's flash messages.
func (m *tuiModel) noteSuccess(label string) tea.Cmd {
	m.nextTaskID++
	m.tasks = append(m.tasks, &bgTask{
		id: m.nextTaskID, label: label, started: time.Now(),
		done: true, clearAt: time.Now().Add(4 * time.Second),
	})
	return m.scheduleTick()
}

func (m *tuiModel) noteFailure(label, detail string) tea.Cmd {
	m.nextTaskID++
	m.tasks = append(m.tasks, &bgTask{
		id: m.nextTaskID, label: label, detail: detail, started: time.Now(),
		done: true, failed: true, clearAt: time.Now().Add(10 * time.Second),
	})
	return m.scheduleTick()
}

// taskKeyPath strips the kind prefix from a row key ("wt:s/r/w" → "s/r/w")
// so conflict checks compare tree paths regardless of row kind.
func taskKeyPath(k string) string {
	if i := strings.Index(k, ":"); i >= 0 {
		return k[i+1:]
	}
	return k
}

// taskKeysConflict reports whether two row keys target overlapping subtrees:
// the same row, an ancestor, or a descendant. A worktree delete therefore
// conflicts with (and blocks) actions on every session under that worktree,
// and vice versa. Anonymous keys ("") never conflict.
func taskKeysConflict(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	pa, pb := taskKeyPath(a), taskKeyPath(b)
	return pa == pb || strings.HasPrefix(pa, pb+"/") || strings.HasPrefix(pb, pa+"/")
}

// runningTaskFor returns the first running task whose target overlaps the
// given row key, or nil. Used both to block conflicting actions and to
// render the per-row spinner.
func (m *tuiModel) runningTaskFor(key string) *bgTask {
	for _, t := range m.tasks {
		if !t.done && !t.pending && taskKeysConflict(t.key, key) {
			return t
		}
	}
	return nil
}

// pruneTasks drops finished tasks whose display window has passed.
func (m *tuiModel) pruneTasks() {
	kept := m.tasks[:0]
	for _, t := range m.tasks {
		if t.done && !t.clearAt.IsZero() && time.Now().After(t.clearAt) {
			continue
		}
		kept = append(kept, t)
	}
	m.tasks = kept
}

type tuiModel struct {
	cfg         *Config
	serverNames []string
	state       map[string]*serverState

	// expanded[key]=true means the user explicitly expanded it.
	// expandedDefault returns the default for a kind when no entry is present.
	expanded map[string]bool
	rows     []treeRow
	cursor   int

	width, height int

	mode      tuiMode
	statusMsg string
	statusErr string

	// tasks is the background-task registry: every async action (delete,
	// prepare+spawn) plus instant outcome notices. Drives the footer's
	// task list, per-row spinners, and conflict blocking. Pruned on tick.
	tasks      []*bgTask
	nextTaskID int

	// Startup queue: one connect task per server ("<server>: connecting…")
	// plus a pending "sync cmux ↔ cctl" task. Each connect task finishes when
	// its server settles; once all have, the sync task flips from pending to
	// running. startupSynced guards that the sync fires exactly once.
	startupConnectTasks map[string]int // server -> task id (deleted once finished)
	startupSyncTaskID   int
	startupSynced       bool
	// syncPending debounces post-startup re-syncs: when one or more servers
	// reconnect+load in a burst (e.g. a manual refresh), they coalesce into a
	// single sync fired by runSyncMsg.
	syncPending bool

	// scrollOffset is the index of the first tree row drawn in the main
	// panel; the panel windows m.rows[scrollOffset:scrollOffset+visible] and
	// follows the cursor (k9s-style). Clamped each render in browseView.
	scrollOffset int
	// spinnerFrame ticks while anything in the model is animating (any
	// running task, or any server connecting / still loading). Shared so
	// every spinner in the UI advances in lockstep.
	spinnerFrame int
	tickPending  bool

	// new-session form
	formServer   string
	formRepo     string
	formWorktree string // if non-empty, worktree pre-filled (worktree-row flow)
	wtInput      textinput.Model
	nameInput    textinput.Model
	promptInput  textinput.Model
	formFocus    int // index into formFields (which depends on formWorktree presence)

	// k9s-style filter — when non-empty, only rows matching this substring
	// (or whose descendants match) are rendered.
	filter string

	// pendingD is true after the user pressed `d` once; the next key has
	// to also be `d` to confirm the delete (vim's dd flow). Any other key
	// cancels. pendingU is the same arming flow for `U` (upgrade claude +
	// restart that server's claude sessions).
	pendingD bool
	pendingU bool

	// notifyWatchers holds the per-remote-server claude→cmux notification
	// bridges (ssh tail of ~/.cctl/notify.jsonl). Started on probe
	// success, restarted on reconnect, killed when the TUI exits.
	notifyWatchers map[string]*notifyWatcher

	exitErr error
}

func newTUIModel(cfg *Config) *tuiModel {
	names := cfg.serverNames()
	sort.Strings(names)
	state := map[string]*serverState{}
	for _, n := range names {
		state[n] = &serverState{}
	}
	mkInput := func(placeholder string, charLimit int) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = placeholder
		ti.CharLimit = charLimit
		ti.Prompt = ""
		return ti
	}
	m := &tuiModel{
		cfg:            cfg,
		serverNames:    names,
		state:          state,
		expanded:       map[string]bool{},
		notifyWatchers: map[string]*notifyWatcher{},
		wtInput:        mkInput("worktree-name (e.g. audit)", 64),
		nameInput:      mkInput("session-name (e.g. quickcheck)", 64),
		promptInput:    mkInput("initial prompt (optional)", 800),
	}
	// Initial expansion state is derived from the data — see isExpanded.
	// Anything with a running session expands automatically once data
	// arrives; users can override with ←/→.
	m.rebuildRows()
	return m
}

// isExpanded looks up an expansion key with kind-specific defaults. The
// guiding principle: if the subtree under a container has at least one
// session, the container expands by default — the user sees every running
// session at startup without manually drilling in. Explicit user toggles
// (left/right) override these defaults.
//
//   - filter active     → everything reads as expanded so the row-filter
//     pass can find matches; the visible tree is then
//     pruned to matches+ancestors.
//   - server            → expanded by default (top-level scaffolding).
//   - repo              → expanded if any session in any of its worktrees.
//   - worktree          → expanded if any session is running on it.
//
// Servers/repos/worktrees with no sessions stay collapsed (▶ or • glyph),
// matching the "show what matters first" k9s-ish vibe.
func (m *tuiModel) isExpanded(row treeRow) bool {
	if m.filter != "" {
		return true
	}
	if v, ok := m.expanded[keyForRow(row)]; ok {
		return v
	}
	switch row.kind {
	case rowServer:
		return true
	case rowRepo:
		st := m.state[row.server]
		if st == nil {
			return false
		}
		for _, sess := range st.sessions {
			if sess.Repo == row.repo {
				return true
			}
		}
		return false
	case rowWorktree:
		st := m.state[row.server]
		if st == nil {
			return false
		}
		for _, sess := range st.sessions {
			if sess.Repo == row.repo && sess.Worktree == row.worktree {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// keyForRow returns the canonical expansion key for a row.
func keyForRow(r treeRow) string {
	switch r.kind {
	case rowServer:
		return "server:" + r.server
	case rowRepo:
		return "repo:" + r.server + "/" + r.repo
	case rowWorktree:
		return "wt:" + r.server + "/" + r.repo + "/" + r.worktree
	}
	return ""
}

// rowKey is keyForRow extended to include sessions, used by the
// per-row preparing-spinner. (keyForRow stops at rowWorktree because
// it's used by m.expanded which only matters for container rows.)
func rowKey(r treeRow) string {
	if r.kind == rowSession {
		return "sess:" + r.server + "/" + r.repo + "/" + r.session.Worktree + "/" + r.session.Session
	}
	return keyForRow(r)
}

// ---- messages --------------------------------------------------------------

type loadedSessionsMsg struct {
	server   string
	sessions []SessionInfo
	err      error
}

type loadedReposMsg struct {
	server string
	repos  map[string]Repo
	err    error
}

type loadedWorktreesMsg struct {
	server    string
	worktrees map[string][]Worktree
	statuses  map[string]RepoStatus
	err       error
}

// probedMsg is the result of a reachability probe (cheap `true` over the
// transport). On success the model fans out the heavier sessions/repos/
// worktrees fetches. On failure the model marks the server disconnected
// and schedules a retry.
type probedMsg struct {
	server string
	err    error
}

// retryProbeMsg fires when the backoff timer elapses for a disconnected
// server. The handler re-issues loadServerCmd for that server only.
type retryProbeMsg struct {
	server string
}

// runSyncMsg fires (debounced) after one or more servers reconnect+load, to
// run the cmux↔cctl sync once for the burst. See requestSync.
type runSyncMsg struct{}

type prepareDoneMsg struct {
	server   Server
	useMosh  bool
	cmdStr   string
	cwd      string // suggested workspace cwd (used by cmuxSpawner)
	wsTitle  string // workspace label: "repo/worktree" (one workspace per worktree)
	tabTitle string // tab label inside the workspace: the session name
	// serverName/repo/worktree/session are the session's identity, carried
	// through to SpawnSpec for the durable wrapper path + restore manifest.
	serverName string
	repo       string
	worktree   string
	session    string
	group      string // sidebar group: the repo ("rxtx.dev" local, "server/repo" remote)
	groupCwd   string // anchor cwd when the group is first created (repo path)
	label      string
	refresh    string
	// focusExisting allows the spawner to focus a same-titled workspace
	// instead of creating one. Only safe when a live tmux client is known
	// to sit in that tab (attach-to-attached-session); see Spawner.
	focusExisting bool
	// taskID ties this result back to the bgTask started when the action
	// was triggered, so the right footer entry resolves.
	taskID int
	err    error
}

type actionDoneMsg struct {
	msg     string
	refresh string
	// refreshAll reloads every server after the action (used by restore,
	// which can bring sessions back across all of them).
	refreshAll bool
	taskID     int
	err        error
}

// refreshServerMsg triggers a one-server reload, used as a follow-up after
// spawning a session in a new terminal window (so the tree picks it up).
type refreshServerMsg struct {
	server string
}

// spawnInNewWindow launches an interactive cmd in a new tab (or window,
// depending on the terminal) using the configured/auto-detected Spawner.
// Fire-and-forget — TUI stays alive. Returns the provider name + an error
// if the spawn failed. The previous inline-fallback path was removed when
// cctl committed to "Enter always opens in a new tab".
//
// We always write a tiny wrapper script first so each Spawner only ever
// has to hand its terminal one filesystem path; this dodges all the
// arg-quoting/splitting pitfalls.
func spawnInNewWindow(cfg *Config, s Server, useMosh bool, cmdStr string, spec SpawnSpec) (string, error) {
	inner, err := interactiveCmd(s, useMosh, cmdStr)
	if err != nil {
		return "", fmt.Errorf("build interactive cmd: %w", err)
	}
	script, err := writeSpawnScript(inner, spec)
	if err != nil {
		return "", err
	}
	spec.Script = script
	// transport: cmux-ssh — request a remote-SSH workspace running the
	// raw remote command; the wrapper script above stays as the fallback
	// if the cmux ssh path fails.
	if s.useCmuxSSH() {
		spec.Remote = &RemoteSpawn{
			Destination: sshTarget(s),
			Port:        s.Port,
			Identity:    expandPath(s.SSHKey),
			SSHOptions:  sshOptionValues(s.SSHOpts),
			Command:     cmdStr,
		}
	}
	pref := ""
	if cfg != nil {
		pref = cfg.Defaults.Spawn
	}
	sp, reason := detectSpawner(pref)
	log().Debug("tui-spawn", "provider", sp.Name(), "reason", reason, "script", script,
		"cwd", spec.Cwd, "ws", spec.WsTitle, "tab", spec.TabTitle,
		"group", spec.GroupTitle, "focusExisting", spec.FocusExisting)
	if err := sp.Spawn(spec); err != nil {
		// Only clean up disposable ($TMPDIR) wrappers. A durable
		// ~/.cctl/spawn script may already back an existing cmux tab; a
		// transient spawn failure here mustn't yank it out from under a
		// later restore.
		if !spec.hasIdentity() {
			os.Remove(script)
		}
		return sp.Name(), err
	}
	// Record the session in the restore manifest so a reboot (which wipes
	// tmux, cctl's only other memory) can bring it back. Disposable spawns
	// without identity aren't tracked.
	if spec.hasIdentity() {
		manifestUpsert(spec)
	}
	return sp.Name(), nil
}

// writeSpawnScript materializes a small shell script that exec's the
// interactive command. The spawner just sees a single path, so there's no
// risk of arg-splitting going wrong.
//
// When the spec carries a full session identity the script gets a stable,
// reboot-durable path under ~/.cctl/spawn/ (overwritten in place on every
// respawn) — this is what cmux replays on restore, so it must outlive a
// reboot. Identity-less / ad-hoc spawns fall back to a disposable $TMPDIR
// temp file, the legacy behavior.
func writeSpawnScript(inner *exec.Cmd, spec SpawnSpec) (string, error) {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# cctl-spawn — autogenerated wrapper for a single cctl session.\n")
	b.WriteString("exec")
	b.WriteString(" " + shellQuote(inner.Path))
	for _, a := range inner.Args[1:] {
		b.WriteString(" " + shellQuote(a))
	}
	b.WriteString("\n")

	if spec.hasIdentity() {
		path, err := spawnScriptPath(spec)
		if err != nil {
			return "", err
		}
		if err := writeScriptAtomic(path, b.String()); err != nil {
			return "", err
		}
		return path, nil
	}

	f, err := os.CreateTemp("", "cctl-spawn-*.sh")
	if err != nil {
		return "", fmt.Errorf("create wrapper: %w", err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write wrapper: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("close wrapper: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("chmod wrapper: %w", err)
	}
	return f.Name(), nil
}

// ---- bubbletea lifecycle ---------------------------------------------------

func (m *tuiModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	for _, n := range m.serverNames {
		cmds = append(cmds, m.loadServerCmd(n)...)
	}
	// Servers start in connConnecting so the model is animating from
	// turn 0; schedule the first tick to drive their spinners.
	if c := m.scheduleTick(); c != nil {
		cmds = append(cmds, c)
	}
	// Show the startup work as a visible queue, done one by one: one
	// "<server>: connecting…" task per server (each finishes when that server
	// settles), then a "sync cmux ↔ cctl" step that waits in line (pending,
	// ○) until every server has settled — see maybeStartupSync. NOT a fixed
	// timer: the old 2s tick raced ahead of the remote fetches.
	m.startupConnectTasks = map[string]int{}
	for _, n := range m.serverNames {
		id, tick := m.startTask("startup:connect:"+n, n+": connecting…")
		m.startupConnectTasks[n] = id
		if tick != nil {
			cmds = append(cmds, tick)
		}
	}
	m.startupSyncTaskID = m.startPendingTask("startup:sync", "sync cmux ↔ cctl")
	// Pin + redden the workspace cctl is running in, so its tab stands out and
	// stays put. Fire-and-forget; no-op outside cmux.
	cmds = append(cmds, func() tea.Msg { markSelfCmuxWorkspace(); return nil })
	return tea.Batch(cmds...)
}

// startupProgress advances the queue after a server's state changed. During
// startup it finishes the server's connect entry and runs the one-shot sync
// once every server has settled. After startup, a server that just
// (re)connected and finished loading triggers a fresh (debounced) sync — this
// is the "refresh a remote → sync runs" path. tea.Batch tolerates nil cmds.
func (m *tuiModel) startupProgress(server string) tea.Cmd {
	settle := m.settleServerTask(server)
	if !m.startupSynced {
		return tea.Batch(settle, m.maybeStartupSync())
	}
	st := m.state[server]
	if st != nil && st.pendingSync && st.conn == connConnected &&
		st.sessionsLoaded && st.reposLoaded && st.worktreesLoaded {
		st.pendingSync = false
		return tea.Batch(settle, m.requestSync())
	}
	return settle
}

// requestSync schedules a single post-startup sync shortly after now,
// coalescing a burst of reconnects (e.g. a refresh of every server) into one.
func (m *tuiModel) requestSync() tea.Cmd {
	if m.syncPending {
		return nil
	}
	m.syncPending = true
	return tea.Tick(400*time.Millisecond, func(time.Time) tea.Msg { return runSyncMsg{} })
}

// settleServerTask finishes a server's startup "connecting…" queue entry once
// that server has settled — loaded (ready) or given up (unreachable). No-op
// until then, and only fires once per server.
func (m *tuiModel) settleServerTask(server string) tea.Cmd {
	id, tracked := m.startupConnectTasks[server]
	if !tracked {
		return nil
	}
	st := m.state[server]
	if st == nil {
		return nil
	}
	switch st.conn {
	case connConnected:
		if !st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded {
			return nil
		}
		delete(m.startupConnectTasks, server)
		return m.finishTask(id, fmt.Sprintf("%s: ready (%d session(s))", server, len(st.sessions)), nil)
	case connDisconnected:
		delete(m.startupConnectTasks, server)
		err := st.connErr
		if err == nil {
			err = fmt.Errorf("unreachable")
		}
		return m.finishTask(id, server+": unreachable", err)
	}
	return nil
}

// allServersSettled reports whether every server has finished its initial
// load — connected ones fully fetched, unreachable ones given up on (so a
// dead remote doesn't stall startup forever).
func (m *tuiModel) allServersSettled() bool {
	for _, n := range m.serverNames {
		st := m.state[n]
		if st == nil {
			return false
		}
		switch st.conn {
		case connConnecting:
			return false
		case connConnected:
			if !st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded {
				return false
			}
		}
	}
	return true
}

// maybeStartupSync fires the one-time cmux↔cctl reconcile the moment all
// servers have settled (sessions/worktrees fetched), then never again this
// run. Returns nil until then. This is the correct sequencing the fixed-
// timer version lacked: sync must see the real session lists.
func (m *tuiModel) maybeStartupSync() tea.Cmd {
	if m.startupSynced || !m.allServersSettled() {
		return nil
	}
	m.startupSynced = true
	sweepLegacySpawnScripts()
	// The startup sync covers every server, so clear their pendingSync flags
	// — otherwise the post-startup reconnect path would immediately re-sync.
	for _, st := range m.state {
		if st != nil {
			st.pendingSync = false
		}
	}
	// Flip the queued "sync" step to running and execute it.
	m.markTaskRunning(m.startupSyncTaskID)
	return tea.Batch(m.syncCmd(m.startupSyncTaskID, false), m.scheduleTick())
}

// loadServerCmd issues a reachability probe first, then (once connected)
// fans out the three heavy fetches: sessions, repos+discovery, and
// worktrees-for-all-repos. Local servers skip the probe because there's
// no transport that can fail.
//
// The model handles `probedMsg`: on success it triggers the fan-out; on
// failure it marks the server disconnected and schedules a retry.
func (m *tuiModel) loadServerCmd(serverName string) []tea.Cmd {
	srv := m.cfg.Servers[serverName]
	st := m.state[serverName]
	if st != nil {
		wasLoaded := st.conn == connConnected && st.sessionsLoaded && st.reposLoaded && st.worktreesLoaded
		st.conn = connConnecting
		st.connAttempts++
		// On a fresh or post-disconnect load we want rebuildRows to
		// show a "connecting…" placeholder instead of stale data, so
		// clear the loaded flags. But on a REFRESH of an already-
		// connected server, keep the data + flags so the user sees
		// the existing tree while we update; rows just appear dimmed
		// via the refreshing-render path until new data arrives.
		if !wasLoaded {
			st.sessionsLoaded = false
			st.reposLoaded = false
			st.worktreesLoaded = false
		}
	}
	if srv.Local {
		// Local servers can't transport-fail; emit synthetic success
		// + the heavy fetches directly.
		return append([]tea.Cmd{
			func() tea.Msg { return probedMsg{server: serverName, err: nil} },
		}, m.fetchServerCmd(serverName)...)
	}
	return []tea.Cmd{
		func() tea.Msg {
			start := time.Now()
			// Cheap reachability check. Don't reuse runRemote here so
			// stderr never echoes to the terminal — just want a yes/no.
			_, err := runRemote(srv, "true")
			log().Debug("tui-probe",
				"server", serverName, "dur", time.Since(start).String(),
				"err", errString(err))
			return probedMsg{server: serverName, err: err}
		},
	}
}

// fetchServerCmd performs the three heavy loads. Split out from
// loadServerCmd so probedMsg can trigger it once we know the box is up.
func (m *tuiModel) fetchServerCmd(serverName string) []tea.Cmd {
	srv := m.cfg.Servers[serverName]
	return []tea.Cmd{
		func() tea.Msg {
			start := time.Now()
			sessions, err := listSessions(serverName, srv)
			log().Debug("tui-load-sessions",
				"server", serverName, "count", len(sessions),
				"dur", time.Since(start).String(), "err", errString(err))
			return loadedSessionsMsg{server: serverName, sessions: sessions, err: err}
		},
		func() tea.Msg {
			start := time.Now()
			merged := mergeRepos(srv)
			discovered, derr := discoverRepos(srv)
			for k, v := range discovered {
				merged[k] = v
			}
			log().Debug("tui-load-repos",
				"server", serverName, "discovered", len(discovered),
				"explicit", len(srv.Repos), "total", len(merged),
				"dur", time.Since(start).String(), "err", errString(derr))
			return loadedReposMsg{server: serverName, repos: merged, err: derr}
		},
		func() tea.Msg {
			start := time.Now()
			// Re-merge here so worktrees pick up the same set as the repos
			// fetch; sequencing it via loadedReposMsg would force us to issue
			// two SSH round-trips in series.
			merged := mergeRepos(srv)
			discovered, _ := discoverRepos(srv)
			for k, v := range discovered {
				merged[k] = v
			}
			wts, statuses, err := listAllWorktrees(srv, merged)
			log().Debug("tui-load-worktrees",
				"server", serverName, "repos", len(merged),
				"dur", time.Since(start).String(), "err", errString(err),
				"statuses", statuses)
			return loadedWorktreesMsg{server: serverName, worktrees: wts, statuses: statuses, err: err}
		},
	}
}

// mergeRepos returns the explicit repos map (copy) so callers can layer
// discovery on top without mutating the original.
func mergeRepos(srv Server) map[string]Repo {
	out := map[string]Repo{}
	for k, v := range srv.Repos {
		out[k] = v
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// preparingPrefix returns an amber-bold spinner glyph + space when a
// running background task targets this row (or an ancestor/descendant of
// it — a worktree being removed spins its session rows too). Returns ""
// otherwise so the column doesn't shift.
func (m *tuiModel) preparingPrefix(r treeRow) string {
	if m.runningTaskFor(rowKey(r)) == nil {
		return ""
	}
	return warnStyle.Bold(true).Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
}

// canDelete reports whether dd is a valid action on the given row. Server
// and repo rows are never deletable (cctl's job is sessions + worktrees);
// the synthetic "main" worktree is also off-limits (it's the repo's
// primary checkout). Used by handleBrowseKey's "d" arm to skip the
// "press d again…" prompt when the second `d` would just refuse.
func canDelete(r treeRow) bool {
	switch r.kind {
	case rowSession:
		return true
	case rowWorktree:
		return r.worktree != "main"
	}
	return false
}

// Probe retry policy: a server gets at most one automatic retry after its
// first reachability probe fails, then it's left disconnected until the user
// refreshes (r). Auto-reconnecting forever is pointless here — the ssh
// ConnectTimeout already bounds each probe, and startup sync only waits for
// every server to *settle* (a given-up server counts as settled), so one
// retry is enough to ride out a transient blip without stalling.
const (
	maxProbeAttempts = 2 // initial probe + one retry
	probeRetryDelay  = 2 * time.Second
)

func (m *tuiModel) refreshAllCmd() tea.Cmd {
	var cmds []tea.Cmd
	for _, n := range m.serverNames {
		cmds = append(cmds, m.loadServerCmd(n)...)
	}
	return tea.Batch(cmds...)
}

func (m *tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case loadedSessionsMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.sessions = msg.sessions
		st.sessionErr = msg.err
		st.sessionsLoaded = true
		st.normalizeSessionNames()
		m.rebuildRows()
		return m, m.startupProgress(msg.server)

	case loadedReposMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.repos = msg.repos
		st.repoErr = msg.err
		st.reposLoaded = true
		st.normalizeSessionNames()
		m.rebuildRows()
		return m, m.startupProgress(msg.server)

	case loadedWorktreesMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		st.worktrees = msg.worktrees
		st.repoStatus = msg.statuses
		st.wtErr = msg.err
		st.worktreesLoaded = true
		st.normalizeSessionNames()
		m.rebuildRows()
		return m, m.startupProgress(msg.server)

	case probedMsg:
		st := m.state[msg.server]
		if st == nil {
			return m, nil
		}
		if msg.err == nil {
			st.conn = connConnected
			st.connErr = nil
			// Reset attempts on success so the next failure starts
			// backoff from scratch.
			st.connAttempts = 0
			// (Re)connected — once its data loads, trigger a sync. This is
			// what makes a manual refresh of a remote run the cmux sync.
			st.pendingSync = true
			m.ensureNotifyWatcher(msg.server)
			m.rebuildRows()
			return m, tea.Batch(m.fetchServerCmd(msg.server)...)
		}
		st.conn = connDisconnected
		st.connErr = msg.err
		log().Warn("tui-probe-fail",
			"server", msg.server, "attempt", st.connAttempts,
			"err", msg.err.Error())
		m.rebuildRows()
		// A server giving up counts as "settled" — advance the startup queue
		// so an all-remote-down launch still reconciles the local cmux.
		server := msg.server
		cmds := []tea.Cmd{m.startupProgress(server)}
		// Auto-retry at most once (see maxProbeAttempts); after that the host
		// stays disconnected until the user hits r.
		if st.connAttempts < maxProbeAttempts {
			cmds = append(cmds, tea.Tick(probeRetryDelay, func(time.Time) tea.Msg {
				return retryProbeMsg{server: server}
			}))
		}
		return m, tea.Batch(cmds...)

	case retryProbeMsg:
		st := m.state[msg.server]
		if st == nil || st.conn == connConnected {
			return m, nil
		}
		cmds := m.loadServerCmd(msg.server)
		if c := m.scheduleTick(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case tickMsg:
		m.tickPending = false
		// Advance the shared spinner frame so every spinner in the UI
		// (footer task list, server rows) draws the same braille glyph.
		m.spinnerFrame = (m.spinnerFrame + 1) % len(spinnerFrames)
		m.pruneTasks()
		return m, m.scheduleTick()

	case prepareDoneMsg:
		if msg.err != nil {
			return m, m.finishTask(msg.taskID, msg.label+": prepare failed", msg.err)
		}
		provider, err := spawnInNewWindow(m.cfg, msg.server, msg.useMosh, msg.cmdStr, SpawnSpec{
			Server:        msg.serverName,
			Repo:          msg.repo,
			Worktree:      msg.worktree,
			Session:       msg.session,
			Cwd:           msg.cwd,
			WsTitle:       msg.wsTitle,
			TabTitle:      msg.tabTitle,
			GroupTitle:    msg.group,
			GroupCwd:      msg.groupCwd,
			FocusExisting: msg.focusExisting,
		})
		if err != nil {
			log().Warn("tui-spawn-fail", "provider", provider, "err", err.Error())
			return m, m.finishTask(msg.taskID, provider+" spawn failed", err)
		}
		log().Info("tui-spawn-ok", "label", msg.label, "provider", provider)
		finish := m.finishTask(msg.taskID, fmt.Sprintf("opened %s (%s tab)", msg.label, provider), nil)
		refresh := msg.refresh
		return m, tea.Batch(finish,
			tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg {
				return refreshServerMsg{server: refresh}
			}),
		)

	case refreshServerMsg:
		var cmds []tea.Cmd
		if msg.server == "" {
			cmds = append(cmds, m.refreshAllCmd())
		} else {
			cmds = append(cmds, m.loadServerCmd(msg.server)...)
		}
		if c := m.scheduleTick(); c != nil {
			cmds = append(cmds, c)
		}
		return m, tea.Batch(cmds...)

	case runSyncMsg:
		// Debounced post-startup sync (a server reconnected/refreshed).
		m.syncPending = false
		id, tick := m.startTask("sync", "syncing cmux ↔ cctl…")
		return m, tea.Batch(m.syncCmd(id, false), tick)

	case actionDoneMsg:
		// Note: this no longer forces the mode back to browse — the user
		// may have opened the form or the filter while the action ran in
		// the background, and yanking them out of it was part of the old
		// flakiness.
		label := msg.msg
		if msg.err != nil && label == "" {
			label = "action failed"
		}
		statusCmd := m.finishTask(msg.taskID, label, msg.err)
		if msg.refreshAll {
			return m, tea.Batch(m.refreshAllCmd(), statusCmd)
		}
		if msg.refresh != "" {
			cmds := m.loadServerCmd(msg.refresh)
			cmds = append(cmds, statusCmd)
			return m, tea.Batch(cmds...)
		}
		return m, statusCmd

	case tea.KeyMsg:
		switch m.mode {
		case modeBrowse:
			return m.handleBrowseKey(msg)
		case modeNewForm:
			return m.handleFormKey(msg)
		case modeFilter:
			return m.handleFilterKey(msg)
		}
	}
	return m, nil
}

// ---- key handling ----------------------------------------------------------

func (m *tuiModel) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "down", "j":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "pgdown", "ctrl+f":
		if n := len(m.rows); n > 0 {
			m.cursor = min(m.cursor+m.pageStep(), n-1)
		}
	case "pgup", "ctrl+b":
		m.cursor = max(m.cursor-m.pageStep(), 0)
	case "ctrl+d":
		if n := len(m.rows); n > 0 {
			m.cursor = min(m.cursor+m.pageStep()/2, n-1)
		}
	case "ctrl+u":
		m.cursor = max(m.cursor-m.pageStep()/2, 0)
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}
	case "h", "left":
		m.clearPending()
		m.collapseCurrent()
	case "l", "right":
		m.clearPending()
		m.expandCurrent()
	case "enter", " ", "shift+enter", "ctrl+enter", "alt+enter":
		m.clearPending()
		// Enter (and any modifier variant) opens the row in a new cmux
		// tab. The inline / in-place mode was removed — cctl is built
		// around cmux's workspace UI and a separate path was just an
		// extra config knob people had to learn.
		return m.activateRow()
	case "n":
		m.clearPending()
		return m.startNewForm()
	case "t":
		m.clearPending()
		return m.openTerminal()
	case "d":
		// vim-style dd: first d arms (red footer prompt is the
		// confirmation). second d deletes outright — no extra modal.
		//
		// On a (s) session we keep the worktree (safer; the branch may
		// hold unpushed work). On a (w) worktree row we kill any
		// sessions on it and remove the worktree, same as before.
		if m.pendingD {
			m.clearPending()
			m.statusErr = ""
			m.statusMsg = ""
			return m.executeDelete()
		}
		m.clearPending()
		// If the row can't be deleted (server / repo / "main" worktree),
		// don't even arm the prompt — show the refusal immediately so the
		// user isn't led to believe a second `d` will do something.
		if m.cursor < len(m.rows) && !canDelete(m.rows[m.cursor]) {
			return m.executeDelete()
		}
		m.pendingD = true
		m.statusErr = "" // hide any earlier error so the red prompt below stands alone
		m.statusMsg = ""
		return m, nil
	case "S", "R":
		// Reconcile: converge cmux+tmux to the manifest (the same pass the
		// startup/reconnect sync runs). Revives tracked sessions that aren't
		// up, opens missing tabs, groups, and closes orphaned tabs. S and R
		// are the same action — kept as two keys for muscle memory.
		m.clearPending()
		return m.syncCmux()
	case "U":
		// UU, same arming flow as dd: upgrading claude restarts every
		// claude session on the target server (killing whatever they
		// were doing), so it deserves the explicit second keypress.
		if m.pendingU {
			m.clearPending()
			m.statusErr = ""
			m.statusMsg = ""
			return m.upgradeClaude()
		}
		m.clearPending()
		m.pendingU = true
		m.statusErr = ""
		m.statusMsg = ""
		return m, nil
	case "r":
		m.clearPending()
		m.statusErr = ""
		log().Info("tui-refresh-all")
		return m, m.refreshAllCmd()
	case "/":
		m.clearPending()
		m.mode = modeFilter
		m.statusErr = ""
		log().Debug("tui-filter-start")
		return m, nil
	case "esc":
		// in browse mode, esc clears any active filter or pending dd/UU
		m.clearPending()
		if m.filter != "" {
			log().Debug("tui-filter-clear")
			m.filter = ""
			m.rebuildRows()
		}
	case "?":
		m.statusMsg = "↑↓ move · PgUp/Dn ⌃d/⌃u scroll · g/G top/bottom · ←→ fold · ⏎ attach · n new · t terminal · dd delete · S/R reconcile (match cmux to cctl) · UU upgrade · / filter · r refresh · q quit"
	default:
		// any other key cancels a pending dd/UU so the next press starts fresh
		m.clearPending()
	}
	return m, nil
}

// clearPending resets the dd/UU arming states. Safe to call unconditionally.
func (m *tuiModel) clearPending() {
	m.pendingD = false
	m.pendingU = false
}

func (m *tuiModel) formFields() []*textinput.Model {
	if m.formWorktree == "" {
		// flow: pick worktree (or "main" / new), then session, then prompt
		return []*textinput.Model{&m.wtInput, &m.nameInput, &m.promptInput}
	}
	// flow: worktree pre-selected, only session + prompt
	return []*textinput.Model{&m.nameInput, &m.promptInput}
}

func (m *tuiModel) handleFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	fields := m.formFields()
	switch msg.String() {
	case "esc", "ctrl+c":
		m.mode = modeBrowse
		for _, f := range fields {
			f.Blur()
		}
		return m, nil
	case "tab":
		m.formFocus = (m.formFocus + 1) % len(fields)
		for i, f := range fields {
			if i == m.formFocus {
				f.Focus()
			} else {
				f.Blur()
			}
		}
		return m, nil
	case "shift+tab":
		m.formFocus = (m.formFocus - 1 + len(fields)) % len(fields)
		for i, f := range fields {
			if i == m.formFocus {
				f.Focus()
			} else {
				f.Blur()
			}
		}
		return m, nil
	case "enter", "shift+enter", "ctrl+enter", "alt+enter":
		wt := m.formWorktree
		if wt == "" {
			wt = strings.TrimSpace(m.wtInput.Value())
		}
		name := strings.TrimSpace(m.nameInput.Value())
		if wt == "" {
			log().Info("tui-form-submit-empty-worktree")
			m.statusErr = "worktree is required (use 'main' for the primary checkout)"
			return m, nil
		}
		if name == "" {
			log().Info("tui-form-submit-empty-name")
			m.statusErr = "session name is required"
			return m, nil
		}
		prompt := strings.TrimSpace(m.promptInput.Value())
		server, repo := m.formServer, m.formRepo
		log().Info("tui-form-submit",
			"server", server, "repo", repo, "wt", wt, "session", name,
			"prompt_len", len(prompt))
		m.mode = modeBrowse
		for _, f := range fields {
			f.Blur()
		}
		m.statusErr = ""
		// Track the prepare as a task keyed on the new session's row —
		// the worktree row above it spins too (ancestor overlap), and a
		// dd on that worktree is blocked until the prepare lands.
		key := "sess:" + server + "/" + repo + "/" + wt + "/" + name
		if t := m.runningTaskFor(key); t != nil {
			return m, m.noteFailure("target is busy: "+t.label,
				"wait for the running action to finish before creating this session")
		}
		id, tick := m.startTask(key, fmt.Sprintf("preparing %s/%s/%s/%s…", server, repo, wt, name))
		return m, tea.Batch(tick, m.newSessionPrepareCmd(server, repo, wt, name, prompt, id))
	}
	var cmd tea.Cmd
	*fields[m.formFocus], cmd = fields[m.formFocus].Update(msg)
	return m, cmd
}

// handleFilterKey edits m.filter live (each keystroke triggers rebuildRows so
// the tree updates as you type). Enter commits the filter (returns to browse
// with it still applied); Esc clears + returns; Ctrl-u also clears.
func (m *tuiModel) handleFilterKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.filter = ""
		m.mode = modeBrowse
		m.rebuildRows()
		return m, nil
	case "enter":
		m.mode = modeBrowse
		return m, nil
	case "ctrl+u":
		m.filter = ""
		m.rebuildRows()
		return m, nil
	case "ctrl+c":
		return m, tea.Quit
	case "backspace":
		if len(m.filter) > 0 {
			m.filter = m.filter[:len(m.filter)-1]
			m.rebuildRows()
		}
		return m, nil
	}
	// Treat any printable runes as filter input.
	if len(msg.Runes) > 0 {
		m.filter += string(msg.Runes)
		m.rebuildRows()
	}
	return m, nil
}

// ---- actions ---------------------------------------------------------------

// activateRow handles Enter (and modifier variants — all behave the same
// now). For repo/worktree rows it opens the new-session form. For session
// rows it goes straight to attach. Every path spawns in a new cmux tab.
func (m *tuiModel) activateRow() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	switch row.kind {
	case rowServer:
		m.toggleRow(row)
	case rowRepo, rowWorktree:
		return m.startNewForm()
	case rowSession:
		if t := m.runningTaskFor(rowKey(row)); t != nil {
			return m, m.noteFailure("row is busy: "+t.label,
				"wait for the running action to finish before opening this session")
		}
		id, tick := m.startTask(rowKey(row), fmt.Sprintf("opening %s/%s/%s/%s…",
			row.server, row.repo, row.session.Worktree, row.session.Session))
		return m, tea.Batch(tick, m.attachCmd(row.server, row.repo, *row.session, id))
	}
	return m, nil
}

func (m *tuiModel) toggleRow(row treeRow) {
	key := keyForRow(row)
	if key == "" {
		return
	}
	// Flip relative to the row's current effective expansion state; this
	// preserves the smart-default for worktrees (if it was implicitly
	// expanded, the first toggle collapses it explicitly, and vice versa).
	m.expanded[key] = !m.isExpanded(row)
	m.rebuildRows()
}

// collapseCurrent implements the "climbing" left-arrow behavior:
//
//   - on an expanded container row → collapse it in place;
//   - on a collapsed container OR a leaf row → climb to the parent and
//     collapse that, moving the cursor to the parent so further ← keeps
//     climbing.
//
// This matches the user's mental model of "press left until you reach
// something useful": one ← from a session takes you to its worktree (now
// collapsed), the next takes you to the repo, etc.
func (m *tuiModel) collapseCurrent() {
	if m.cursor >= len(m.rows) {
		return
	}
	row := m.rows[m.cursor]
	if isContainerKind(row.kind) && m.isExpanded(row) {
		m.expanded[keyForRow(row)] = false
		m.rebuildRows()
		return
	}
	parent, ok := m.parentRow(row)
	if !ok {
		return
	}
	if isContainerKind(parent.kind) {
		m.expanded[keyForRow(parent)] = false
	}
	m.rebuildRows()
	m.moveCursorTo(parent)
}

// expandCurrent expands the current row if it's a container with children.
// Empty containers (the `•` glyph) are no-ops since there's nothing to show.
func (m *tuiModel) expandCurrent() {
	if m.cursor >= len(m.rows) {
		return
	}
	row := m.rows[m.cursor]
	if !isContainerKind(row.kind) {
		return
	}
	if !m.hasChildren(row) {
		return
	}
	m.expanded[keyForRow(row)] = true
	m.rebuildRows()
}

func isContainerKind(k rowKind) bool {
	return k == rowServer || k == rowRepo || k == rowWorktree
}

// hasChildren reports whether expanding a container would surface any rows.
// A repo with no worktrees still has the synthetic "main" entry, so repos
// always claim to have children; worktrees claim children only when at
// least one session exists under them; servers when at least one repo is
// known.
func (m *tuiModel) hasChildren(r treeRow) bool {
	st := m.state[r.server]
	if st == nil {
		return false
	}
	switch r.kind {
	case rowServer:
		// Disconnected servers have nothing to show beneath them.
		if st.conn == connDisconnected {
			return false
		}
		// While we haven't heard back from repo/session discovery yet,
		// assume there's something to expand so the user can drill in
		// without the glyph briefly flickering to •.
		if !st.reposLoaded && !st.sessionsLoaded {
			return true
		}
		if len(st.repos) > 0 {
			return true
		}
		for _, sess := range st.sessions {
			if sess.Repo != "" {
				return true
			}
		}
		return false
	case rowRepo:
		return true
	case rowWorktree:
		for _, sess := range st.sessions {
			if sess.Repo == r.repo && sess.Worktree == r.worktree {
				return true
			}
		}
		return false
	}
	return false
}

// parentRow returns the ancestor of `r` (one level up). For sessions it's
// the worktree, for worktrees the repo, for repos the server. Returns
// ok=false when there's nothing above (a server row, or a row with empty
// identifiers).
func (m *tuiModel) parentRow(r treeRow) (treeRow, bool) {
	switch r.kind {
	case rowSession, rowEmpty, rowLoading:
		if r.worktree != "" {
			return treeRow{kind: rowWorktree, server: r.server, repo: r.repo, worktree: r.worktree}, true
		}
		if r.repo != "" {
			return treeRow{kind: rowRepo, server: r.server, repo: r.repo}, true
		}
		if r.server != "" {
			return treeRow{kind: rowServer, server: r.server}, true
		}
	case rowWorktree:
		return treeRow{kind: rowRepo, server: r.server, repo: r.repo}, true
	case rowRepo:
		return treeRow{kind: rowServer, server: r.server}, true
	}
	return treeRow{}, false
}

// moveCursorTo points the cursor at the row matching target's identifiers
// (kind + server/repo/worktree). No-op if not found.
func (m *tuiModel) moveCursorTo(target treeRow) {
	for i, r := range m.rows {
		if r.kind == target.kind &&
			r.server == target.server &&
			r.repo == target.repo &&
			r.worktree == target.worktree {
			m.cursor = i
			return
		}
	}
}

func (m *tuiModel) startNewForm() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	server, repo, worktree := row.server, row.repo, row.worktree
	if server == "" {
		m.statusErr = "select a server, repo, worktree, or session row"
		return m, nil
	}
	if repo == "" {
		st := m.state[server]
		if st != nil && len(st.repos) == 1 {
			for n := range st.repos {
				repo = n
			}
		}
		if repo == "" {
			m.statusErr = "expand a repo (and a worktree if you have one) and try again"
			return m, nil
		}
	}
	// rowSession's worktree is set; rowEmpty/loading under a worktree have it
	// too. rowRepo doesn't — leave worktree empty so the form asks for one.
	m.formServer = server
	m.formRepo = repo
	m.formWorktree = worktree
	m.wtInput.SetValue("")
	m.nameInput.SetValue("")
	m.promptInput.SetValue("")
	m.formFocus = 0
	fields := m.formFields()
	for i, f := range fields {
		if i == 0 {
			f.Focus()
		} else {
			f.Blur()
		}
	}
	m.mode = modeNewForm
	m.statusErr = ""
	return m, nil
}

// executeDelete is the vim-dd action: fires the kill straight away with no
// extra modal — the red "press d again" prompt was the confirmation. On a
// session row we keep the worktree (the branch is the user's work); on a
// worktree row we kill any sessions on it and `git worktree remove`. The
// synthetic "main" worktree is refused.
//
// The delete runs as a tracked background task: the row keeps a spinner,
// the footer lists the operation, and a second action targeting the same
// subtree is refused until it finishes.
func (m *tuiModel) executeDelete() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	if t := m.runningTaskFor(rowKey(row)); t != nil {
		return m, m.noteFailure("row is busy: "+t.label,
			"wait for the running action to finish before starting another on the same target")
	}
	switch row.kind {
	case rowSession:
		log().Info("tui-dd-session",
			"server", row.server, "repo", row.repo,
			"wt", row.session.Worktree, "session", row.session.Session)
		id, tick := m.startTask(rowKey(row),
			fmt.Sprintf("deleting session %s/%s/%s", row.repo, row.session.Worktree, row.session.Session))
		return m, tea.Batch(tick, m.killCmd(row.server, row.repo, row.session.Name, row.session.Worktree, row.session.Session, false, id))
	case rowRepo:
		// Repos are deliberately not deletable from cctl. The post-incident
		// rule is: cctl can only delete things INSIDE the worktree_base.
		// Repos live outside it (they're the user's source of truth). Show
		// an explicit, visible refusal instead of silently doing nothing.
		log().Info("tui-dd-repo-refused", "server", row.server, "repo", row.repo)
		return m, m.noteFailure("can't delete a repo from cctl",
			"cctl only deletes worktrees and sessions; the repo dir is yours to manage (rm or git clone manually)")
	case rowServer:
		log().Info("tui-dd-server-refused", "server", row.server)
		return m, m.noteFailure("can't delete a server",
			"servers are config entries; edit ~/.cctl.yaml to remove one")
	case rowWorktree:
		if row.worktree == "main" {
			return m, m.noteFailure("can't delete the main worktree",
				"that's the repo's primary checkout; cctl will never remove it")
		}
		// Pass the *actual* on-disk path from `git worktree list` (not the
		// cctl-convention path) so we delete the worktree the user can see
		// in the DETAIL column, even if it lives outside ~/worktrees/<repo>/.
		var wtPath string
		if row.wt != nil {
			wtPath = row.wt.Path
		}
		// Snapshot the sessions running on this worktree NOW, on the UI
		// goroutine — the kill closure runs concurrently and must not read
		// m.state (the loaders mutate it).
		var victims []string
		if st := m.state[row.server]; st != nil {
			for _, s := range st.sessions {
				if s.Repo == row.repo && s.Worktree == row.worktree {
					victims = append(victims, s.Name)
				}
			}
		}
		log().Info("tui-dd-worktree",
			"server", row.server, "repo", row.repo, "wt", row.worktree, "path", wtPath)
		id, tick := m.startTask(rowKey(row),
			fmt.Sprintf("removing worktree %s/%s (%d session(s))", row.repo, row.worktree, len(victims)))
		return m, tea.Batch(tick, m.killWorktreeCmd(row.server, row.repo, row.worktree, wtPath, victims, id))
	default:
		return m, m.noteFailure("nothing to delete on this row", "select a session or worktree")
	}
}

// openTerminal handles `t`: a plain-shell tmux session on the selected
// worktree, opened as a tab in the worktree's cmux workspace — the same
// transport as a claude session (tmux + mosh/ssh/local), just without
// claude. A repo row targets its "main" checkout; a session row targets
// that session's worktree. Names auto-increment (term, term2, …) per
// worktree so repeated presses give fresh shells; the sessions appear in
// the tree like any other and can be attached/deleted there.
func (m *tuiModel) openTerminal() (tea.Model, tea.Cmd) {
	if m.cursor >= len(m.rows) {
		return m, nil
	}
	row := m.rows[m.cursor]
	var server, repo, wt string
	switch row.kind {
	case rowWorktree:
		server, repo, wt = row.server, row.repo, row.worktree
	case rowRepo:
		server, repo, wt = row.server, row.repo, "main"
	case rowSession:
		server, repo, wt = row.server, row.repo, row.session.Worktree
	default:
		return m, m.noteFailure("terminal needs a repo, worktree, or session row",
			"select where the shell should live and press t again")
	}
	name := m.freeTerminalName(server, repo, wt)
	key := "sess:" + server + "/" + repo + "/" + wt + "/" + name
	id, tick := m.startTask(key, fmt.Sprintf("opening terminal %s/%s/%s…", repo, wt, name))
	return m, tea.Batch(tick, m.terminalPrepareCmd(server, repo, wt, name, id))
}

// freeTerminalName picks the first unused term/term2/term3… name on the
// given worktree, based on the model's last-loaded session list.
func (m *tuiModel) freeTerminalName(server, repo, wt string) string {
	used := map[string]bool{}
	if st := m.state[server]; st != nil {
		for _, s := range st.sessions {
			if s.Repo == repo && s.Worktree == wt {
				used[s.Session] = true
			}
		}
	}
	if !used["term"] {
		return "term"
	}
	for i := 2; ; i++ {
		if name := fmt.Sprintf("term%d", i); !used[name] {
			return name
		}
	}
}

func (m *tuiModel) terminalPrepareCmd(serverName, repoName, worktree, name string, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		cwd := r.Repo.Path
		if worktree != "" && worktree != "main" {
			cwd = worktreePath(r.WorktreeBase, r.RepoName, worktree)
		}
		group, groupCwd := repoGroup(r)
		return prepareDoneMsg{
			server:     r.Server,
			useMosh:    r.UseMosh,
			cmdStr:     terminalCmd(tmuxName(r.RepoName, worktree, name), cwd),
			cwd:        workspaceCwd(r, worktree),
			wsTitle:    cmuxWsTitle(repoName, worktree, name),
			tabTitle:   name,
			serverName: serverName,
			repo:       repoName,
			worktree:   worktree,
			session:    name,
			group:      group,
			groupCwd:   groupCwd,
			label:      fmt.Sprintf("%s/%s/%s/%s", serverName, repoName, worktree, name),
			refresh:    serverName,
			taskID:     taskID,
		}
	}
}

// syncCmux is the S/R keys and the automatic startup/reconnect pass: ONE
// reconcile that converges cmux+tmux to the manifest (desired state) — adopt
// live sessions, revive tracked ones that died (reboot/kill → claude
// --continue), open missing tabs, group, and close orphaned (untracked) tabs.
// dd is the only way to drop a session. See syncCmuxState for the algorithm
// and safety rails.
func (m *tuiModel) syncCmux() (tea.Model, tea.Cmd) {
	id, tick := m.startTask("", "reconciling cmux ↔ cctl…")
	return m, tea.Batch(tick, m.syncCmd(id, false))
}

// syncCmd runs the reconcile off the UI goroutine. `quiet` suppresses the
// "already in sync" note (used by the automatic startup pass, which
// shouldn't chatter when there's nothing to do).
func (m *tuiModel) syncCmd(taskID int, quiet bool) tea.Cmd {
	return func() tea.Msg {
		res := syncCmuxState(m.cfg)
		log().Info("tui-sync", "restored", res.restored, "closed", res.closed,
			"renamed", res.migrated, "grouped", res.grouped, "adopted", res.adopted,
			"errs", res.errs)
		msg := res.summary()
		if !res.touched() {
			if quiet {
				msg = ""
			} else {
				msg = "cmux already matches cctl"
			}
		}
		// No tree refresh: reconcile only touches cmux workspaces + the
		// manifest. A revive does create a tmux session, but the tree catches
		// up on the next reconnect/`r`; refreshing here would re-probe every
		// server and loop back into another reconcile.
		return actionDoneMsg{msg: msg, taskID: taskID}
	}
}

// upgradeTarget resolves what a `UU` press would act on: the selected
// row's server and how many claude sessions (term* shells excluded) would
// be restarted there. Used by both the armed footer prompt and the action.
func (m *tuiModel) upgradeTarget() (string, []string) {
	if m.cursor >= len(m.rows) {
		return "", nil
	}
	server := m.rows[m.cursor].server
	var names []string
	if st := m.state[server]; st != nil {
		for _, s := range st.sessions {
			if !isTerminalSession(s.Session) {
				names = append(names, s.Name)
			}
		}
	}
	return server, names
}

// upgradeClaude handles the second `U`: run the configured update command
// on the selected row's server, then restart each claude session there
// via `tmux respawn-window -k` — the window re-runs its stored launch
// script, so claude relaunches on the new binary and `--continue` picks
// the conversation back up. Attached clients (cmux tabs) stay attached
// through the respawn. Plain-terminal sessions are left alone.
func (m *tuiModel) upgradeClaude() (tea.Model, tea.Cmd) {
	server, names := m.upgradeTarget()
	if server == "" {
		return m, m.noteFailure("select a row first", "U targets the server of the selected row")
	}
	if t := m.runningTaskFor("server:" + server); t != nil {
		return m, m.noteFailure("server is busy: "+t.label,
			"wait for the running action to finish before upgrading claude")
	}
	srv := m.cfg.Servers[server]
	log().Info("tui-upgrade-agents", "server", server, "sessions", len(names))
	// Keyed on the server row so everything underneath it is blocked (and
	// spins) while the agent(s) upgrade and the sessions bounce.
	id, tick := m.startTask("server:"+server,
		fmt.Sprintf("upgrading agents on %s (then restarting %d session(s))…", server, len(names)))
	return m, tea.Batch(tick, m.upgradeClaudeCmd(server, srv, names, id))
}

func (m *tuiModel) upgradeClaudeCmd(serverName string, srv Server, sessions []string, taskID int) tea.Cmd {
	return func() tea.Msg {
		// Map sanitized tmux repo names back to real ones so resolve() finds
		// the worktree (e.g. "rxtx_dev" → "rxtx.dev").
		bySafe := map[string]string{}
		if allRepos, rerr := m.cfg.repos(serverName); rerr == nil {
			for n := range allRepos {
				bySafe[tmuxSafeName(n)] = n
			}
		}
		// Sessions on one server can resolve to different agents (different
		// repos), so the self-update is keyed on the resolved agent and run at
		// most once per agent. If an agent's update fails we skip respawning
		// that agent's sessions — same conservative intent as the original
		// "update failed → don't bounce sessions", scoped per agent.
		updated := map[string]bool{}
		updateErrs := map[string]string{}
		ensureUpdate := func(agent string) bool {
			if updated[agent] {
				return updateErrs[agent] == ""
			}
			updated[agent] = true
			cmd := m.cfg.agentUpdateCmd(agent)
			if out, err := runRemote(srv, agentUpdateScript(cmd)); err != nil {
				updateErrs[agent] = fmt.Sprintf("%s: %v — %s", cmd, err, abbrev(strings.TrimSpace(out), 200))
				log().Warn("tui-agent-update-fail", "server", serverName, "agent", agent, "err", err.Error())
				return false
			}
			return true
		}

		restarted, failed := 0, 0
		for _, name := range sessions {
			// Respawn with the CURRENT launch script (not the window's stale
			// original), so the restart picks up the new agent binary AND the
			// current flags/hooks. claude --continue / codex resume --last
			// resumes the conversation, so the session survives. Falls back to
			// re-running the original command when we can't resolve the
			// session's worktree.
			cmd := fmt.Sprintf("tmux respawn-window -k -t %s", shellQuote(name))
			if repo, wt, _, ok := parseTmuxName(name); ok {
				if real, found := bySafe[repo]; found {
					repo = real
				}
				if r, rerr := m.cfg.resolve(serverName, repo); rerr == nil {
					if !ensureUpdate(r.Agent) {
						// Update for this agent failed; leave the session as-is.
						failed++
						continue
					}
					cwd := r.Repo.Path
					if wt != "" && wt != "main" {
						cwd = worktreePath(r.WorktreeBase, r.RepoName, wt)
					}
					launch := agentLaunchScript(r, cwd, "", name)
					cmd = fmt.Sprintf("tmux respawn-window -k -t %s %s", shellQuote(name), shellQuote(launch))
				}
			}
			if _, err := runRemote(srv, cmd); err != nil {
				log().Warn("tui-respawn-fail", "server", serverName, "session", name, "err", err.Error())
				failed++
				continue
			}
			restarted++
		}
		msg := fmt.Sprintf("agents upgraded on %s; relaunched %d session(s)", serverName, restarted)
		if len(updateErrs) > 0 {
			parts := make([]string, 0, len(updateErrs))
			for agent, e := range updateErrs {
				parts = append(parts, fmt.Sprintf("%s (%s)", agent, e))
			}
			return actionDoneMsg{
				msg:     msg,
				err:     fmt.Errorf("agent update failed: %s", strings.Join(parts, "; ")),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		if failed > 0 {
			return actionDoneMsg{
				msg:     msg,
				err:     fmt.Errorf("%d session(s) failed to restart — see ~/.cctl.log", failed),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		return actionDoneMsg{msg: msg, refresh: serverName, taskID: taskID}
	}
}

func (m *tuiModel) attachCmd(serverName, repoName string, sess SessionInfo, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		// attachOrRespawn (not bare `tmux attach`): cmux re-runs the wrapper
		// script when restoring the workspace, possibly long after the
		// session died — see the helper's doc comment.
		cwd := r.Repo.Path
		if sess.Worktree != "" && sess.Worktree != "main" {
			cwd = worktreePath(r.WorktreeBase, r.RepoName, sess.Worktree)
		}
		group, groupCwd := repoGroup(r)
		return prepareDoneMsg{
			server:     r.Server,
			useMosh:    r.UseMosh,
			cmdStr:     agentAttachOrRespawn(r, sess.Name, cwd),
			cwd:        workspaceCwd(r, sess.Worktree),
			wsTitle:    cmuxWsTitle(repoName, sess.Worktree, sess.Session),
			tabTitle:   sess.Session,
			serverName: serverName,
			repo:       repoName,
			worktree:   sess.Worktree,
			session:    sess.Session,
			group:      group,
			groupCwd:   groupCwd,
			label:      fmt.Sprintf("%s/%s/%s", serverName, repoName, sess.Session),
			refresh:    serverName,
			// Reuse the existing cmux tab only when a tmux client is
			// already attached — that's the tab holding it. A detached
			// session's old tab (if any) is a dead shell; the wrapper
			// must run, so force a fresh workspace.
			focusExisting: sess.Attached,
			taskID:        taskID,
		}
	}
}

func (m *tuiModel) newSessionPrepareCmd(serverName, repoName, worktree, sessionName, prompt string, taskID int) tea.Cmd {
	return func() tea.Msg {
		start := time.Now()
		log().Info("tui-new-session-prepare-start",
			"server", serverName, "repo", repoName, "wt", worktree, "session", sessionName)
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			log().Error("tui-new-session-resolve-fail",
				"server", serverName, "repo", repoName, "err", err.Error())
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		cmdStr, err := prepareAgent(r, worktree, sessionName, "", prompt, false, false)
		log().Info("tui-new-session-prepare-done",
			"server", serverName, "session", sessionName,
			"dur", time.Since(start).String(), "err", errString(err))
		if err != nil {
			return prepareDoneMsg{err: err, taskID: taskID}
		}
		group, groupCwd := repoGroup(r)
		return prepareDoneMsg{
			server:     r.Server,
			useMosh:    r.UseMosh,
			cmdStr:     cmdStr,
			cwd:        workspaceCwd(r, worktree),
			wsTitle:    cmuxWsTitle(repoName, worktree, sessionName),
			tabTitle:   sessionName,
			serverName: serverName,
			repo:       repoName,
			worktree:   worktree,
			session:    sessionName,
			group:      group,
			groupCwd:   groupCwd,
			label:      fmt.Sprintf("%s/%s/%s/%s", serverName, repoName, worktree, sessionName),
			refresh:    serverName,
			taskID:     taskID,
		}
	}
}

// repoGroup returns the sidebar-group identity for a repo: the bare repo
// name on the local server ("rxtx.dev"), server-qualified elsewhere
// ("workspace/olympus") so same-named repos on different hosts don't
// merge into one group. The cwd anchors the group's header workspace —
// the repo checkout locally, the stand-in project dir for remotes.
func repoGroup(r *Resolved) (name, cwd string) {
	if r.Server.Local {
		return r.RepoName, expandPath(r.Repo.Path)
	}
	return r.ServerName + "/" + r.RepoName, standInRepoDir(r)
}

// standInRepoDir returns (and lazily creates) a local placeholder
// directory representing a REMOTE repo: ~/.cctl/remotes/<server>/<repo>,
// marked with an empty .git dir. cmux derives a workspace's "project"
// by walking its LOCAL working directory up to the nearest .git — remote
// paths don't exist here, so without a stand-in every remote workspace
// lands in the sidebar's "No Folder" bucket. With it, all of a remote
// repo's worktree workspaces group under the repo name.
func standInRepoDir(r *Resolved) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".cctl", "remotes", r.ServerName, r.RepoName)
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		log().Debug("standin-mkdir-fail", "dir", dir, "err", err.Error())
		return ""
	}
	return dir
}

// workspaceCwd is the directory hint we pass to Spawner.Spawn — for cmux
// this becomes the new workspace's working directory (and the sidebar
// label). For other spawners it's purely cosmetic since the wrapper
// script handles its own cd. "main" worktree maps to the repo's path,
// any other to <worktree_base>/<repo>/<worktree>.
//
// Note: paths returned here can still start with "~" because they're
// derived from the user's config; cmuxCLIPath spawns the CLI locally so
// only the local shell will tilde-expand. If it doesn't, cmux will
// usually fall back to $HOME — not the end of the world.
func workspaceCwd(r *Resolved, worktreeName string) string {
	// For remote servers, the worktree path only exists on the remote
	// host. cmux/wezterm/etc. spawn locally and would error with "Path
	// does not exist". Use a per-worktree subdir of the local stand-in
	// project dir (see standInRepoDir) so cmux's project-root walk
	// groups the workspace under its repo instead of "No Folder"; fall
	// back to $HOME when the stand-in can't be created. The wrapper
	// script's mosh/ssh handles the real remote cd.
	if !r.Server.Local {
		if repoDir := standInRepoDir(r); repoDir != "" {
			wt := worktreeName
			if wt == "" {
				wt = "main"
			}
			dir := filepath.Join(repoDir, wt)
			if err := os.MkdirAll(dir, 0o755); err == nil {
				return dir
			}
		}
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return ""
	}
	if worktreeName == "" || worktreeName == "main" {
		return expandPath(r.Repo.Path)
	}
	return expandPath(worktreePath(r.WorktreeBase, r.RepoName, worktreeName))
}

func (m *tuiModel) killCmd(serverName, repoName, tmuxFullName, worktreeName, session string, removeWorktree bool, taskID int) tea.Cmd {
	return func() tea.Msg {
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err, taskID: taskID}
		}
		if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(tmuxFullName))); err != nil {
			return actionDoneMsg{err: fmt.Errorf("kill %s: %w", tmuxFullName, err), taskID: taskID}
		}
		// dd removes the session everywhere: forget it in the restore
		// manifest (so sync won't bring it back) and close its cmux workspace
		// so the window matches cctl. Best-effort — a missing one is fine.
		manifestRemove(serverName, repoName, worktreeName, session)
		closeCmuxWorkspaceByTitle(cmuxWsTitle(repoName, worktreeName, session))
		// "main" worktree is the original checkout — never delete it.
		if removeWorktree && worktreeName != "main" {
			wt := worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
			if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
				return actionDoneMsg{
					msg:     fmt.Sprintf("killed %s (worktree removal failed)", tmuxFullName),
					err:     fmt.Errorf("remove worktree %s: %w", wt, err),
					refresh: serverName,
					taskID:  taskID,
				}
			}
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %s + removed worktree", tmuxFullName),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		return actionDoneMsg{
			msg:     fmt.Sprintf("killed %s (worktree kept)", tmuxFullName),
			refresh: serverName,
			taskID:  taskID,
		}
	}
}

// killWorktreeCmd removes a whole worktree: kills every cctl-managed tmux
// session running on it, then runs `git worktree remove` (with --force +
// rm -rf fallbacks). The branch is intentionally kept (matching `cctl rm`
// behavior) so unpushed work isn't lost. Refuses to touch "main".
//
// `wtPath` is the actual on-disk path from `git worktree list`, when
// known (TUI flow). If empty we fall back to the cctl-convention path
// `<worktree_base>/<repo>/<worktree_name>` — the legacy behavior, which
// silently no-ops for worktrees that live outside that convention.
//
// `victims` is the list of tmux session names to kill first, snapshotted
// by the caller on the UI goroutine — this closure runs concurrently with
// the model and must not read m.state.
func (m *tuiModel) killWorktreeCmd(serverName, repoName, worktreeName, wtPath string, victims []string, taskID int) tea.Cmd {
	return func() tea.Msg {
		if worktreeName == "main" {
			return actionDoneMsg{err: fmt.Errorf("cannot remove the main worktree"), taskID: taskID}
		}
		r, err := m.cfg.resolve(serverName, repoName)
		if err != nil {
			return actionDoneMsg{err: err, taskID: taskID}
		}
		// Belt-and-braces: even if the row was misclassified by a
		// samePath regression, refuse to act when the on-disk path
		// equals the configured repo path. removeWorktreeScript also
		// guards this — three layers because the impact of getting
		// it wrong is "user loses their repo".
		if wtPath != "" && samePath(wtPath, r.Repo.Path) {
			log().Error("tui-kill-worktree-refused-main",
				"server", serverName, "repo", repoName, "wt", worktreeName,
				"path", wtPath, "repo_path", r.Repo.Path)
			return actionDoneMsg{err: fmt.Errorf(
				"refused: wtPath %s is the main checkout (repo.Path=%s) — the row was likely mis-classified",
				wtPath, r.Repo.Path,
			), taskID: taskID}
		}
		wt := wtPath
		if wt == "" {
			wt = worktreePath(r.WorktreeBase, r.RepoName, worktreeName)
		}
		log().Info("tui-kill-worktree-start",
			"server", serverName, "repo", repoName, "wt", worktreeName,
			"path", wt, "sessions", len(victims))
		for _, name := range victims {
			if _, err := runRemote(r.Server, fmt.Sprintf("tmux kill-session -t %s", shellQuote(name))); err != nil {
				log().Warn("tui-kill-worktree-session-fail", "name", name, "err", err.Error())
			}
		}
		// The worktree's sessions are dead now, so their per-session cmux
		// workspaces are stale shells: forget the worktree in the manifest and
		// close every "repo/worktree/*" workspace so cmux matches cctl (done
		// before the git removal so it happens even if that fails).
		manifestRemoveWorktree(serverName, repoName, worktreeName)
		closeCmuxWorkspacesByPrefix(repoName + "/" + worktreeName + "/")
		if _, err := runRemote(r.Server, removeWorktreeScript(r.Repo.Path, wt, r.WorktreeBase)); err != nil {
			return actionDoneMsg{
				msg:     fmt.Sprintf("killed %d session(s); worktree removal failed", len(victims)),
				err:     fmt.Errorf("remove worktree %s: %w", wt, err),
				refresh: serverName,
				taskID:  taskID,
			}
		}
		log().Info("tui-kill-worktree-ok", "wt", wt, "sessions_killed", len(victims))
		return actionDoneMsg{
			msg:     fmt.Sprintf("removed %s (path=%s, killed %d session(s); branch kept)", worktreeName, wt, len(victims)),
			refresh: serverName,
			taskID:  taskID,
		}
	}
}

// ---- tree building ---------------------------------------------------------

func (m *tuiModel) rebuildRows() {
	var rows []treeRow
	for _, server := range m.serverNames {
		serverRow := treeRow{kind: rowServer, server: server, depth: 0}
		rows = append(rows, serverRow)
		st := m.state[server]
		// Disconnected: don't list any children — the state under a
		// down server is stale and would mislead the user (the very
		// kind of "ghost tree" we just fixed for missing repos).
		if st != nil && st.conn == connDisconnected {
			continue
		}
		// Still connecting on first probe → show a placeholder, no
		// (stale) children.
		if st != nil && st.conn == connConnecting && !st.sessionsLoaded && !st.reposLoaded {
			if m.isExpanded(serverRow) {
				note := "connecting…"
				if st.connAttempts > 1 {
					note = fmt.Sprintf("reconnecting… (attempt %d)", st.connAttempts)
				}
				rows = append(rows, treeRow{kind: rowLoading, server: server, depth: 1, note: note})
			}
			continue
		}
		if !m.isExpanded(serverRow) {
			continue
		}
		// Collect repo names from both discovered/explicit map and observed
		// sessions (in case a session is on a repo not surfaced by discovery).
		repoSet := map[string]struct{}{}
		for r := range st.repos {
			repoSet[r] = struct{}{}
		}
		for _, sess := range st.sessions {
			repoSet[sess.Repo] = struct{}{}
		}
		// Loading state: nothing loaded yet → show placeholder.
		if !st.sessionsLoaded && !st.reposLoaded && len(repoSet) == 0 {
			rows = append(rows, treeRow{kind: rowLoading, server: server, depth: 1, note: "loading..."})
			continue
		}
		if len(repoSet) == 0 {
			rows = append(rows, treeRow{kind: rowEmpty, server: server, depth: 1, note: "no repos"})
			continue
		}
		repos := make([]string, 0, len(repoSet))
		for r := range repoSet {
			repos = append(repos, r)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			repoRow := treeRow{kind: rowRepo, server: server, repo: repo, depth: 1}
			rows = append(rows, repoRow)
			if !m.isExpanded(repoRow) {
				continue
			}
			// Collect worktree names: from listAllWorktrees + from sessions.
			wts := st.worktrees[repo]
			wtSet := map[string]struct{}{}
			wtByName := map[string]*Worktree{}
			for i := range wts {
				wtSet[wts[i].Name] = struct{}{}
				wtByName[wts[i].Name] = &wts[i]
			}
			for _, sess := range st.sessions {
				if sess.Repo == repo {
					wtSet[sess.Worktree] = struct{}{}
				}
			}
			// Always show "main" so users can create a session against it
			// even when no worktrees yet exist (or worktrees haven't loaded).
			wtSet["main"] = struct{}{}
			if !st.worktreesLoaded && len(wts) == 0 {
				rows = append(rows, treeRow{kind: rowLoading, server: server, repo: repo, depth: 2, note: "loading worktrees..."})
				continue
			}
			wtNames := make([]string, 0, len(wtSet))
			for n := range wtSet {
				wtNames = append(wtNames, n)
			}
			sort.Slice(wtNames, func(i, j int) bool {
				// "main" sorts first.
				if wtNames[i] == "main" {
					return true
				}
				if wtNames[j] == "main" {
					return false
				}
				return wtNames[i] < wtNames[j]
			})
			for _, wtName := range wtNames {
				wtRow := treeRow{
					kind:     rowWorktree,
					server:   server,
					repo:     repo,
					worktree: wtName,
					wt:       wtByName[wtName],
					depth:    2,
				}
				rows = append(rows, wtRow)
				if !m.isExpanded(wtRow) {
					continue
				}
				var sessions []SessionInfo
				for _, sess := range st.sessions {
					if sess.Repo == repo && sess.Worktree == wtName {
						sessions = append(sessions, sess)
					}
				}
				sort.Slice(sessions, func(i, j int) bool { return sessions[i].Session < sessions[j].Session })
				if len(sessions) == 0 {
					rows = append(rows, treeRow{
						kind: rowEmpty, server: server, repo: repo, worktree: wtName,
						depth: 3, note: "no sessions — press n to create",
					})
					continue
				}
				for i := range sessions {
					rows = append(rows, treeRow{
						kind: rowSession, server: server, repo: repo, worktree: wtName,
						session: &sessions[i], depth: 3,
					})
				}
			}
		}
	}
	if m.filter != "" {
		rows = applyFilter(rows, m.filter)
	}
	var prev *treeRow
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		r := m.rows[m.cursor]
		prev = &r
	}
	m.rows = rows
	m.restoreCursor(prev)
}

// restoreCursor keeps the highlight on the same logical row across a
// rebuild. The cursor used to be a bare index, so any refresh that
// changed the tree's shape (a dd landing, sessions appearing, filter
// keystrokes) silently moved the highlight onto a stranger. Exact row
// first; if it's gone (deleted, collapsed away), the nearest surviving
// ancestor — after dd'ing a session that's its worktree row; after
// dd'ing a worktree, its repo. Falls back to index-clamping when
// nothing identifiable survives.
func (m *tuiModel) restoreCursor(prev *treeRow) {
	if prev != nil {
		// Placeholder rows (empty/loading) have no rowKey; match them on
		// full identity so a background refresh doesn't bump the cursor
		// up to their parent.
		if rowKey(*prev) == "" {
			for i := range m.rows {
				r := m.rows[i]
				if r.kind == prev.kind && r.server == prev.server &&
					r.repo == prev.repo && r.worktree == prev.worktree {
					m.cursor = i
					return
				}
			}
		}
		// Candidate keys, most specific first: the row itself, then its
		// ancestor chain derived from the captured identity fields.
		cands := []string{rowKey(*prev)}
		switch {
		case prev.kind == rowSession && prev.session != nil:
			cands = append(cands, "wt:"+prev.server+"/"+prev.repo+"/"+prev.session.Worktree)
		case prev.worktree != "":
			cands = append(cands, "wt:"+prev.server+"/"+prev.repo+"/"+prev.worktree)
		}
		if prev.repo != "" {
			cands = append(cands, "repo:"+prev.server+"/"+prev.repo)
		}
		if prev.server != "" {
			cands = append(cands, "server:"+prev.server)
		}
		for _, k := range cands {
			if k == "" {
				continue
			}
			for i := range m.rows {
				if rowKey(m.rows[i]) == k {
					m.cursor = i
					return
				}
			}
		}
	}
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

// applyFilter keeps every row that matches the filter on its OWN primary
// identifier (server/repo/worktree/session name), plus every ancestor of
// such a match so the user still sees the tree position. Descendants of a
// matching repo are NOT pulled in unless they themselves match — searching
// a repo name must not surface unrelated sessions just because they
// happen to live under that repo.
//
// Placeholder rows (rowEmpty, rowLoading) are always dropped when filter is
// active — they're noise, not data.
func applyFilter(rows []treeRow, filter string) []treeRow {
	if filter == "" {
		return rows
	}
	needle := strings.ToLower(filter)

	// First pass: mark every row index that matches in its own right, and
	// record the ancestor identifiers we need to keep so the matches stay
	// in their tree positions.
	matchIdx := make(map[int]bool)
	keepAncestor := make(map[string]bool)
	for i, r := range rows {
		if r.kind == rowEmpty || r.kind == rowLoading {
			continue
		}
		if !rowMatches(r, needle) {
			continue
		}
		matchIdx[i] = true
		if r.server != "" {
			keepAncestor["server:"+r.server] = true
		}
		if r.repo != "" {
			keepAncestor["repo:"+r.server+"/"+r.repo] = true
		}
		if r.worktree != "" {
			keepAncestor["wt:"+r.server+"/"+r.repo+"/"+r.worktree] = true
		}
	}

	// Second pass: emit matches + ancestors (only if at least one descendant
	// matched) preserving original depths so the tree is intact.
	var out []treeRow
	for i, r := range rows {
		if matchIdx[i] {
			out = append(out, r)
			continue
		}
		key := keyForRow(r)
		if key != "" && keepAncestor[key] {
			out = append(out, r)
		}
	}
	return out
}

// rowMatches checks ONLY the row's own primary identifier — not the names
// of its ancestors. A session named X does not match Y just because it
// sits inside a repo named Y; the user wants matches, not lineage.
func rowMatches(r treeRow, needle string) bool {
	check := func(s string) bool {
		return s != "" && strings.Contains(strings.ToLower(s), needle)
	}
	switch r.kind {
	case rowServer:
		return check(r.server)
	case rowRepo:
		return check(r.repo)
	case rowWorktree:
		branch := ""
		if r.wt != nil {
			branch = r.wt.Branch
		}
		return check(r.worktree) || check(branch)
	case rowSession:
		return check(r.session.Session)
	}
	return false
}

// ---- view ------------------------------------------------------------------

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	dimStyle     = lipgloss.NewStyle().Faint(true)
	errorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	cursorStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Underline(true).Foreground(lipgloss.Color("7"))
	warnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11")) // amber/yellow for "connecting"
	darkRedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("88")) // ANSI 256 dark red — for (detached) etc.
)

// Column header labels — also used as minimum widths.
const (
	hdrName = "NAME"
	hdrInfo = "DETAIL"
	hdrAge  = "AGE"
)

func (m *tuiModel) View() string {
	switch m.mode {
	case modeNewForm:
		return m.formView()
	default:
		return m.browseView()
	}
}

// browseView is the main screen: a title bar, a scrollable tree panel on the
// left, an ACTIVITY (task queue) + KEYS sidebar on the right, and a full-width
// status line at the bottom. On very small / not-yet-sized terminals it falls
// back to a simple linear render.
func (m *tuiModel) browseView() string {
	if m.width < 40 || m.height < 10 {
		return m.linearView()
	}

	title := m.renderTitleBar()
	status := m.bottomStatus()
	// Key hints live at the bottom now (not a right sidebar) so the tree —
	// the NAME column especially — gets the full terminal width.
	hints := m.renderKeyHints(m.width)

	// The activity queue sits in its own strip just above the hints — the
	// "things the system is doing", checked off one by one. Present only while
	// there's work (startup connects + sync, deletes, restores…).
	queue := m.renderQueueStrip(m.width)
	queueH := 0
	if queue != "" {
		queueH = lipgloss.Height(queue) + 1 // +1 for the blank spacer line
	}

	bodyH := m.height - lipgloss.Height(title) - lipgloss.Height(hints) - lipgloss.Height(status) - queueH
	if bodyH < 3 {
		bodyH = 3
	}

	body := lipgloss.NewStyle().Width(m.width).Height(bodyH).MaxHeight(bodyH).
		Render(m.renderMainPanel(m.width, bodyH))

	out := title + "\n" + body
	if queue != "" {
		out += "\n\n" + queue
	}
	return out + "\n" + hints + "\n" + status
}

// renderKeyHints is the compact, dim shortcut legend shown along the bottom.
// It's one " · "-joined string wrapped to the terminal width — horizontal, so
// it costs one or two lines instead of a full-height right column.
func (m *tuiModel) renderKeyHints(w int) string {
	if w < 1 {
		w = 1
	}
	return dimStyle.Width(w).Render(strings.Join(keyHints, "  ·  "))
}

// renderQueueStrip draws the activity queue as a bordered-top strip: a faint
// rule + "ACTIVITY", then each task (running spinner / pending ○ / done ✓✗),
// one per line. Empty string when nothing is queued. Width-truncated.
func (m *tuiModel) renderQueueStrip(w int) string {
	tasks := m.renderTaskLines()
	if len(tasks) == 0 {
		return ""
	}
	maxW := w
	if maxW < 1 {
		maxW = 1
	}
	trunc := lipgloss.NewStyle().MaxWidth(maxW)
	lines := []string{dimStyle.Render("ACTIVITY")}
	for _, t := range tasks {
		lines = append(lines, trunc.Render(t))
	}
	return strings.Join(lines, "\n")
}

// linearView is the small-terminal / pre-size fallback: title, the tree, then
// the task queue + legend below (the pre-redesign layout).
func (m *tuiModel) linearView() string {
	var b strings.Builder
	b.WriteString(m.renderTitleBar() + "\n\n")
	avail := m.width
	if avail <= 0 {
		avail = 100
	}
	nameW, infoW, ageW := m.measureColumns(avail)
	b.WriteString(m.renderHeader(nameW, infoW, ageW) + "\n")
	if len(m.rows) == 0 {
		hint := "(no servers configured — run `cctl init`)"
		if m.filter != "" {
			hint = "(no rows match filter — esc to clear)"
		}
		b.WriteString(dimStyle.Render(hint) + "\n")
	}
	for i, r := range m.rows {
		b.WriteString(m.renderRow(r, i == m.cursor, nameW, infoW, ageW) + "\n")
	}
	b.WriteString("\n" + m.footer())
	return b.String()
}

// renderTitleBar is the top line: app name + version on the left, the detected
// spawn provider on the right (cmux inherits TERM_PROGRAM=ghostty, so what
// cctl picks isn't always obvious — surfacing it helps).
func (m *tuiModel) renderTitleBar() string {
	pref := ""
	if m.cfg != nil {
		pref = m.cfg.Defaults.Spawn
	}
	sp, _ := detectSpawner(pref)
	left := titleStyle.Render("cctl") + dimStyle.Render(" "+Version+" — claude sessions")
	right := dimStyle.Render("spawn=" + sp.Name())
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 || m.width <= 0 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// renderMainPanel draws the column header plus the visible window of tree
// rows, scrolled so the cursor stays on screen.
func (m *tuiModel) renderMainPanel(w, h int) string {
	nameW, infoW, ageW := m.measureColumns(w)
	var b strings.Builder
	b.WriteString(m.renderHeader(nameW, infoW, ageW))

	visible := h - 1 // column header takes one line
	if visible < 1 {
		visible = 1
	}
	m.ensureCursorVisible(visible)

	if len(m.rows) == 0 {
		hint := "(no servers configured — run `cctl init`)"
		if m.filter != "" {
			hint = "(no rows match filter — esc to clear)"
		}
		b.WriteString("\n" + dimStyle.Render(hint))
		return b.String()
	}
	end := m.scrollOffset + visible
	if end > len(m.rows) {
		end = len(m.rows)
	}
	for i := m.scrollOffset; i < end; i++ {
		b.WriteString("\n" + m.renderRow(m.rows[i], i == m.cursor, nameW, infoW, ageW))
	}
	return b.String()
}

// pageStep is the cursor jump for PageUp/PageDown (roughly one screen of
// rows); ctrl+d/ctrl+u use half of it. Derived from the terminal height
// minus the title/header/status chrome.
func (m *tuiModel) pageStep() int {
	s := m.height - 6
	if s < 1 {
		s = 1
	}
	return s
}

// ensureCursorVisible clamps scrollOffset so the cursor row sits within the
// visible window (and the window never runs past the end of the list).
func (m *tuiModel) ensureCursorVisible(visible int) {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor < m.scrollOffset {
		m.scrollOffset = m.cursor
	}
	if m.cursor >= m.scrollOffset+visible {
		m.scrollOffset = m.cursor - visible + 1
	}
	maxOff := len(m.rows) - visible
	if maxOff < 0 {
		maxOff = 0
	}
	if m.scrollOffset > maxOff {
		m.scrollOffset = maxOff
	}
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
}

// bottomStatus is the full-width line under the body: armed-action prompts and
// errors (loud), the filter editor, a transient status message, or the idle
// hint with the scroll position.
func (m *tuiModel) bottomStatus() string {
	switch {
	case m.pendingD:
		return errorStyle.Bold(true).Render("press d again to delete — any other key cancels")
	case m.pendingU:
		server, names := m.upgradeTarget()
		return errorStyle.Bold(true).Render(fmt.Sprintf(
			"press U again to upgrade claude on %s and restart %d claude session(s) — any other key cancels",
			server, len(names)))
	case m.statusErr != "":
		return errorStyle.Render("error: " + m.statusErr)
	case m.mode == modeFilter || m.filter != "":
		fstyle := lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
		if m.mode != modeFilter {
			fstyle = dimStyle
		}
		line := dimStyle.Render("/") + fstyle.Render(m.filter)
		if m.mode == modeFilter {
			line += cursorStyle.Render("▎")
		}
		return line + dimStyle.Render(fmt.Sprintf("   (%d rows)", len(m.rows)))
	case m.statusMsg != "":
		return dimStyle.Render(m.statusMsg)
	default:
		// Just the scroll position — the key hints line above already covers
		// help/quit, so don't repeat them here.
		if len(m.rows) > 0 {
			return dimStyle.Render(fmt.Sprintf("row %d/%d", m.cursor+1, len(m.rows)))
		}
		return ""
	}
}

// keyHints is the compact one-liner legend along the bottom (renderKeyHints
// joins + wraps it). The full reference is `?` (legendLines).
var keyHints = []string{
	"↑↓ move",
	"←→ fold",
	"⏎ open",
	"n new",
	"t term",
	"dd delete",
	"S/R reconcile",
	"UU upgrade",
	"/ filter",
	"r refresh",
	"? help",
	"q quit",
}

// measureColumns returns the natural width of each column based on actual row
// contents (and the header labels as minimums). NAME is the important column
// (the session tree), so when the row overflows the terminal the squeeze falls
// on DETAIL first, then AGE, and only touches NAME as a last resort.
func (m *tuiModel) measureColumns(avail int) (nameW, infoW, ageW int) {
	nameW = lipgloss.Width(hdrName)
	infoW = lipgloss.Width(hdrInfo)
	ageW = lipgloss.Width(hdrAge)
	for _, r := range m.rows {
		n, i, a := m.rowCells(r, false)
		if w := lipgloss.Width(n); w > nameW {
			nameW = w
		}
		if w := lipgloss.Width(i); w > infoW {
			infoW = w
		}
		if w := lipgloss.Width(a); w > ageW {
			ageW = w
		}
	}
	// 2 leading mark + 2 single-space gaps between columns = 4 cells.
	if avail <= 0 {
		return nameW, infoW, ageW
	}
	over := nameW + infoW + ageW + 4 - avail
	if over <= 0 {
		return nameW, infoW, ageW
	}
	// Shrink DETAIL first, down to its header label.
	if floor := lipgloss.Width(hdrInfo); infoW > floor {
		cut := min(over, infoW-floor)
		infoW -= cut
		over -= cut
	}
	// Then AGE.
	if over > 0 {
		if floor := lipgloss.Width(hdrAge); ageW > floor {
			cut := min(over, ageW-floor)
			ageW -= cut
			over -= cut
		}
	}
	// Only then NAME (so it stays full whenever the line can possibly fit it).
	if over > 0 {
		if nameW-over >= lipgloss.Width(hdrName) {
			nameW -= over
		} else {
			nameW = lipgloss.Width(hdrName)
		}
	}
	return nameW, infoW, ageW
}

func (m *tuiModel) renderHeader(nameW, infoW, ageW int) string {
	name := padOrTrunc(hdrName, nameW)
	info := padOrTrunc(hdrInfo, infoW)
	age := padOrTrunc(hdrAge, ageW)
	return "  " + headerStyle.Render(name) + " " + headerStyle.Render(info) + " " + headerStyle.Render(age)
}

// rowCells produces the three column strings for a row — separated from
// rendering so we can measure widths in a pre-pass.
//
// Conventions for the leading glyph:
//
//	▼  expanded (has children, currently showing them)
//	▶  collapsed (has children, hidden)
//	•  no children to show (an empty worktree, etc.)
//	●  session — attached
//	○  session — detached
//
// Each non-server row carries a small "(r)"/"(w)"/"(s)" tag so the row kind
// is unambiguous regardless of indent.
func (m *tuiModel) rowCells(r treeRow, selected bool) (name, info, age string) {
	indent := strings.Repeat("  ", r.depth)
	tagStyle := dimStyle
	switch r.kind {
	case rowServer:
		st := m.state[r.server]
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.server, m.filter)
		nameStyle := lipgloss.NewStyle().Bold(true)
		if selected {
			nameStyle = cursorStyle
		}
		styled := nm
		if m.filter == "" {
			styled = nameStyle.Render(r.server)
		}
		// Spinner sits to the LEFT of the name (between the tree
		// glyph and the server label) when probing or still loading
		// post-probe. Empty when fully loaded so the column doesn't
		// jitter as servers come online.
		spinPrefix := ""
		if st != nil {
			switch {
			case st.conn == connConnecting:
				spinPrefix = warnStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
			case st.conn == connConnected && (!st.sessionsLoaded || !st.reposLoaded || !st.worktreesLoaded):
				spinPrefix = dimStyle.Render(spinnerFrames[m.spinnerFrame%len(spinnerFrames)]) + " "
			}
		}
		name = indent + glyph + " " + spinPrefix + styled + childCountSuffix(len(st.sessions))
		srv := m.cfg.Servers[r.server]
		// Connection state takes precedence: an unreachable host has no
		// meaningful session/repo state to show.
		switch {
		case st.conn == connDisconnected:
			hint := "disconnected"
			if st.connErr != nil {
				hint = "disconnected: " + abbrev(st.connErr.Error(), 70)
			}
			if st.connAttempts < maxProbeAttempts {
				hint += fmt.Sprintf(" (retry in %s)", probeRetryDelay)
			} else {
				hint += " (press r to retry)"
			}
			info = errorStyle.Render(hint)
		case st.conn == connConnecting && st.sessionsLoaded && st.reposLoaded:
			// Refresh of already-loaded data: keep the host string
			// visible so the user can see what's being refreshed, and
			// append a dim "refreshing…" cue (the spinner prefix on
			// the name already shows motion).
			base := srv.Host
			if srv.Local {
				base = "(local)"
			}
			info = dimStyle.Render(base + " · refreshing…")
		case st.conn == connConnecting:
			if st.connAttempts > 1 {
				info = warnStyle.Render(fmt.Sprintf("reconnecting… (attempt %d)", st.connAttempts))
			} else {
				info = warnStyle.Render("connecting…")
			}
		case !st.sessionsLoaded && !st.reposLoaded:
			info = dimStyle.Render("loading…")
		case st.sessionErr != nil:
			info = errorStyle.Render("error: " + abbrev(st.sessionErr.Error(), 50))
		case srv.Local:
			info = dimStyle.Render("(local)")
		case srv.Host != "":
			info = dimStyle.Render(srv.Host)
		}

	case rowRepo:
		st := m.state[r.server]
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.repo, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(r.repo)
		}
		count := 0
		if st.worktreesLoaded {
			count = len(st.worktrees[r.repo])
		}
		name = indent + glyph + " " + tagStyle.Render("(r)") + " " + m.preparingPrefix(r) + nm + childCountSuffix(count)
		status := RepoStatusOK
		if st.repoStatus != nil {
			if s, ok := st.repoStatus[r.repo]; ok {
				status = s
			}
		}
		if repo, ok := st.repos[r.repo]; ok {
			switch status {
			case RepoStatusMissing:
				info = errorStyle.Render(repo.Path + " — MISSING on this host")
			case RepoStatusNoGit:
				info = errorStyle.Render(repo.Path + " — NOT a git repo (no .git)")
			default:
				info = dimStyle.Render(repo.Path)
			}
		}

	case rowWorktree:
		st := m.state[r.server]
		var sessCount int
		for _, s := range st.sessions {
			if s.Repo == r.repo && s.Worktree == r.worktree {
				sessCount++
			}
		}
		glyph := containerGlyph(m.isExpanded(r), m.hasChildren(r))
		nm := highlightMatch(r.worktree, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(r.worktree)
		}
		name = indent + glyph + " " + tagStyle.Render("(w)") + " " + m.preparingPrefix(r) + nm + childCountSuffix(sessCount)
		parentStatus := RepoStatusOK
		if st.repoStatus != nil {
			if s, ok := st.repoStatus[r.repo]; ok {
				parentStatus = s
			}
		}
		switch {
		case r.wt != nil && r.wt.Branch != "":
			info = dimStyle.Render(r.wt.Branch)
		case r.wt != nil:
			// Detached HEAD = worktree exists but not on a branch. Worth
			// calling out in dark red so the user notices their changes
			// are at risk if they switch worktrees.
			info = darkRedStyle.Render("(detached)")
		case r.worktree == "main" && parentStatus == RepoStatusOK:
			info = dimStyle.Render("(primary checkout)")
		case parentStatus == RepoStatusMissing:
			// Session-only ghost row hanging off a repo that's gone from
			// disk — tell the truth instead of "(no worktree on disk)".
			info = errorStyle.Render("(repo gone — session is orphaned)")
		case parentStatus == RepoStatusNoGit:
			info = errorStyle.Render("(repo path exists but has no .git)")
		case sessCount > 0:
			// The row exists only because of a tmux session — there's no
			// matching worktree dir on disk. dd is safe (it kills the
			// orphan session and the worktree-remove step is a no-op),
			// so call the state out specifically rather than the generic
			// "no worktree on disk" that made users worry about data
			// loss after the past delete-repo incident.
			info = warnStyle.Render("(orphan session — worktree dir gone; dd is safe)")
		default:
			info = dimStyle.Render("(no worktree on disk)")
		}

	case rowSession:
		sess := r.session
		marker := "○"
		mStyle := dimStyle
		if sess.Attached {
			marker = "●"
			mStyle = okStyle
		}
		nm := highlightMatch(sess.Session, m.filter)
		if m.filter == "" && selected {
			nm = cursorStyle.Render(sess.Session)
		}
		name = indent + mStyle.Render(marker) + " " + tagStyle.Render("(s)") + " " + m.preparingPrefix(r) + nm
		if sess.Attached {
			info = okStyle.Render("attached")
		} else {
			info = dimStyle.Render("detached")
		}
		age = dimStyle.Render(humanAge(sess.Age))

	case rowEmpty, rowLoading:
		name = indent + "  " + dimStyle.Italic(true).Render(r.note)
	}
	return name, info, age
}

func (m *tuiModel) renderRow(r treeRow, selected bool, nameW, infoW, ageW int) string {
	mark := "  "
	if selected {
		mark = cursorStyle.Render("▸ ")
	}
	name, info, age := m.rowCells(r, selected)
	return mark +
		padOrTruncStyled(name, nameW) + " " +
		padOrTruncStyled(info, infoW) + " " +
		padOrTruncStyled(age, ageW)
}

// childCountSuffix returns a "  (N)" gray suffix for non-zero counts, or
// empty when the count is 0 — keeps the tree visually tight by hiding zeros.
func childCountSuffix(n int) string {
	if n <= 0 {
		return ""
	}
	return " " + dimStyle.Render(fmt.Sprintf("(%d)", n))
}

// containerGlyph returns the leading glyph for a container row (server,
// repo, worktree). When the row has children we show an expand/collapse
// indicator; when it doesn't, a small bullet so the user knows nothing
// will appear if they try to expand.
func containerGlyph(expanded, hasChildren bool) string {
	if !hasChildren {
		return "•"
	}
	if expanded {
		return "▼"
	}
	return "▶"
}

// highlightMatch wraps every (case-insensitive) occurrence of needle inside
// name with a bold yellow style, so the matched letters pop in a filtered
// list — k9s/fzf style. Returns name unchanged when needle is empty or
// absent.
func highlightMatch(name, needle string) string {
	if needle == "" {
		return name
	}
	lower := strings.ToLower(name)
	low := strings.ToLower(needle)
	yellow := lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	var b strings.Builder
	for {
		idx := strings.Index(lower, low)
		if idx < 0 {
			b.WriteString(name)
			return b.String()
		}
		b.WriteString(name[:idx])
		b.WriteString(yellow.Render(name[idx : idx+len(needle)]))
		name = name[idx+len(needle):]
		lower = lower[idx+len(needle):]
	}
}

func (m *tuiModel) footer() string {
	var b strings.Builder
	// The task list lives above the legend, separated by a blank line:
	// one line per running background action (spinner + elapsed) and per
	// recently finished one (✓/✗, auto-expiring). Unlike the old single
	// status slot, concurrent deletes/spawns each keep their own line.
	var lines []string
	switch {
	case m.pendingD:
		lines = append(lines, errorStyle.Bold(true).Render("press d again to delete — any other key cancels"))
	case m.pendingU:
		server, names := m.upgradeTarget()
		lines = append(lines, errorStyle.Bold(true).Render(fmt.Sprintf(
			"press U again to upgrade claude on %s and restart %d claude session(s) — any other key cancels",
			server, len(names))))
	case m.statusErr != "":
		lines = append(lines, errorStyle.Width(m.errorWidth()).Render("error: "+m.statusErr))
	default:
		lines = m.renderTaskLines()
		if len(lines) == 0 && m.statusMsg != "" {
			lines = append(lines, dimStyle.Render(m.statusMsg))
		}
	}
	if len(lines) > 0 {
		b.WriteString(strings.Join(lines, "\n") + "\n\n")
	}
	// Legend: one shortcut per line. Was a 130-column single line that
	// truncated awkwardly on narrow terminals.
	for _, line := range legendLines {
		b.WriteString(dimStyle.Render(line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// maxTaskLines caps the footer's task list so a burst of actions doesn't
// push the tree off screen. Running tasks get priority over finished ones.
const maxTaskLines = 6

// renderTaskLines draws one line per visible background task: running
// first (spinner + label + elapsed), then recently finished (✓ / ✗ +
// label, with the error detail on failures).
func (m *tuiModel) renderTaskLines() []string {
	var running, pending, finished []*bgTask
	for _, t := range m.tasks {
		switch {
		case t.done:
			finished = append(finished, t)
		case t.pending:
			pending = append(pending, t)
		default:
			running = append(running, t)
		}
	}
	var lines []string
	spin := spinnerFrames[m.spinnerFrame%len(spinnerFrames)]
	for _, t := range running {
		if len(lines) >= maxTaskLines {
			break
		}
		line := warnStyle.Bold(true).Render(spin + " " + t.label)
		if elapsed := time.Since(t.started).Round(time.Second); elapsed >= time.Second {
			line += dimStyle.Render(fmt.Sprintf(" (%s)", elapsed))
		}
		lines = append(lines, line)
	}
	for _, t := range pending {
		if len(lines) >= maxTaskLines {
			break
		}
		lines = append(lines, dimStyle.Render("○ "+t.label))
	}
	for _, t := range finished {
		if len(lines) >= maxTaskLines {
			break
		}
		if t.failed {
			line := errorStyle.Bold(true).Render("✗ " + t.label)
			if t.detail != "" {
				line += dimStyle.Render(" — " + abbrev(t.detail, 200))
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, okStyle.Bold(true).Render("✓ "+t.label))
		}
	}
	if hidden := len(running) + len(pending) + len(finished) - len(lines); hidden > 0 {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("… and %d more", hidden)))
	}
	return lines
}

// legendLines is the keybinding reference shown at the bottom of the TUI.
// One shortcut per line so the footer doesn't depend on terminal width.
// Aligned with a soft tab so the keys line up visually.
var legendLines = []string{
	"↑↓        move   ·  PgUp/PgDn, ⌃d/⌃u scroll  ·  g/G top/bottom",
	"←→        collapse / expand",
	"⏎         open in new cmux tab (r/w → form, s → attach)",
	"n         new session on selected row",
	"t         plain terminal on selected worktree (no claude)",
	"dd        delete (worktree or session — never the repo) + close its tab",
	"S / R     reconcile: make cmux match cctl — revive tracked sessions,",
	"          open missing tabs, close orphans (also runs on launch)",
	"UU        upgrade claude on selected server + restart its sessions",
	"/         filter (k9s-style)",
	"r         refresh",
	"q         quit",
}

// errorWidth returns the column width to use for wrapping error/status text.
// Falls back to 100 when the terminal hasn't reported its size yet.
func (m *tuiModel) errorWidth() int {
	if m.width > 0 {
		return m.width
	}
	return 100
}

func (m *tuiModel) formView() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("new session"))
	target := fmt.Sprintf("  — %s / %s", m.formServer, m.formRepo)
	if m.formWorktree != "" {
		target += " / " + m.formWorktree
	}
	b.WriteString(dimStyle.Render(target))
	b.WriteString("\n\n")

	type field struct {
		label string
		input *textinput.Model
	}
	var fields []field
	if m.formWorktree == "" {
		fields = []field{
			{"worktree:", &m.wtInput},
			{"session: ", &m.nameInput},
			{"prompt:  ", &m.promptInput},
		}
	} else {
		fields = []field{
			{"session: ", &m.nameInput},
			{"prompt:  ", &m.promptInput},
		}
	}
	for i, f := range fields {
		marker := "  "
		if i == m.formFocus {
			marker = cursorStyle.Render("▸ ")
		}
		b.WriteString(marker + f.label + " " + f.input.View() + "\n")
	}
	b.WriteString("\n")
	if m.statusErr != "" {
		b.WriteString(errorStyle.Width(m.errorWidth()).Render("error: "+m.statusErr) + "\n\n")
	}
	if m.formWorktree == "" {
		b.WriteString(dimStyle.Render("worktree = `main` reuses the primary checkout; any other name creates a new worktree on branch <prefix>/<name>.") + "\n")
	}
	b.WriteString(dimStyle.Render("⏎ submit · tab switch · esc cancel"))
	return b.String()
}

// padOrTrunc pads or trims an unstyled string to exactly w printable columns.
func padOrTrunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if len(s) > w {
		if w <= 1 {
			return s[:w]
		}
		return s[:w-1] + "…"
	}
	return s + strings.Repeat(" ", w-len(s))
}

// padOrTruncStyled pads/trims a string that may contain ANSI escapes to w
// printable columns. Truncation only happens if the *visible* width exceeds
// w; otherwise we right-pad with spaces.
func padOrTruncStyled(s string, w int) string {
	if w <= 0 {
		return ""
	}
	vis := lipgloss.Width(s)
	switch {
	case vis == w:
		return s
	case vis < w:
		return s + strings.Repeat(" ", w-vis)
	default:
		// Naive truncation: cut bytes while watching visible width. This loses
		// styling on the cut tail; acceptable since we mostly avoid styles in
		// the same string as the truncate boundary.
		return truncStyled(s, w)
	}
}

func truncStyled(s string, w int) string {
	if w <= 1 {
		return s[:0]
	}
	// Build prefix one rune at a time until we'd exceed w-1 cells, then append "…".
	out := make([]rune, 0, len(s))
	inEsc := false
	used := 0
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			out = append(out, r)
			continue
		}
		if inEsc {
			out = append(out, r)
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		if used >= w-1 {
			out = append(out, '…')
			break
		}
		out = append(out, r)
		used++
	}
	return string(out)
}
