// Package console is the screen a member sits in for the whole hackathon
// (plan 36): a live, tabbed view of one session, replacing the log scroll that
// used to follow the boot questions.
//
// It is READ-ONLY except for admission (plan 44 ticket 10): accept/decline a
// join request and flip the session's open/closed door on the users tab. No
// keypress elects, restarts, merges or ends anything else — every one of those
// already has a verb, and a verb that is wrong at 3am is recoverable in a way a
// stray keypress is not. Leaving still takes a confirm that names what stops.
//
// The transport rule it renders: structured facts travel, text stays where it
// is produced. Role state and the work feed come through the control plane, so
// every member sees them. Agent output stays on the elector's machine (§10) —
// that member's console streams it live, everyone else's names whose machine
// has it. Dev-server logs are the exception, because /logs already travels.
package console

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/nwylynko/slopball-cli/controlplane"
	"github.com/nwylynko/slopball-cli/devserver"
	"github.com/nwylynko/slopball-cli/reach"
)

// Options is everything the console needs that it cannot read from the session.
type Options struct {
	PIN      string
	Me       string // this member's name, for "is that sync mine?"
	MemberID string // persisted control-plane id; matches when Name differs (plan 42)
	Branch   string
	WorkPath string

	// Elector is true on the machine running the fleet. Only there do the agent
	// tabs have a live stream to show.
	Elector bool

	// Roles are the fleet roles that get a tab, in display order. Empty falls
	// back to the three the session always has.
	Roles []string

	// Quit is what leaving actually does — hand the leases off, stop the
	// daemon, leave the session. Called only after the confirm is accepted.
	Quit func(context.Context) error

	// Decide accepts or declines a pending join request (plan 44 ticket 10).
	// Called with no confirm — a person is blocked on the other end.
	Decide func(ctx context.Context, memberID, decision string) error

	// SetAccess flips the session door open or closed.
	SetAccess func(ctx context.Context, access string) error

	// Announce carries the facts above that the standup resolves only after this
	// screen is drawing — branch, member id, work tree. nil when the caller knows
	// them up front (a join, which has already cloned).
	Announce <-chan MemberMsg
}

// Message types the feeder pushes in. They are the whole input surface: the
// model never fetches anything itself, which is what makes it testable by
// rendering frames to a buffer.
type (
	// SessionMsg is a fresh control-plane snapshot.
	SessionMsg controlplane.Session
	// EventsMsg is new events, oldest first.
	EventsMsg []controlplane.Event
	// LogsMsg is new dev-server lines from the /logs cursor.
	LogsMsg []devserver.Line

	// LogsResetMsg says the dev tab is now following a different dev server —
	// the lease moved — so what is on screen came from a process that is no
	// longer the one being watched.
	LogsResetMsg struct{}
	// AgentOutputMsg is one role's live output — elector only, never sent
	// anywhere else.
	AgentOutputMsg struct {
		Role string
		Text string
	}
	// LogMsg is one line slopball itself wrote (a diverted logx write). A line
	// from a fleet role goes to that role's tab; everything else joins the
	// feed, because output that used to reach the terminal must not simply
	// vanish behind an alt-screen.
	LogMsg struct {
		Component string
		Text      string
	}
	// DevURLMsg is the dev server's address as something this machine can
	// actually open — resolved through controlplane.EndpointURL, because on the
	// session network the published endpoint is `slop://<pin>/dev/` and a URL
	// printed for a human to click IS a dial, just with a slower dialer.
	DevURLMsg string
	// ErrorMsg is one feed source's outcome, sent EVERY tick rather than only
	// on failure — the banner says what is wrong now, and a source that starts
	// working again has to be able to say so. A nil Err clears that source.
	//
	// Source keys it because the sources are independent: the behind count
	// recovering must not clear an unreachable /logs. Empty Source is a one-off
	// with nothing to recover from.
	ErrorMsg struct {
		Source string
		Err    error
	}
	// BoxMsg is what this machine can reach on the session's box (plan 42).
	BoxMsg struct {
		Git reach.Result
		Dev reach.Result
	}
	// BehindMsg is how many non-merge commits on main are not in this work tree.
	BehindMsg int
	// MemberMsg is who this member turned out to be — the branch it edits on and
	// the tree it edits in. The console draws before the standup that resolves
	// those has finished, so they arrive as news rather than as construction
	// arguments. Empty fields leave what is already known alone.
	//
	// PIN arrives the same way on a fresh create: the server mints the name, so
	// the first frame has none and the join line fills in one round trip later
	// (abuse-surface ticket 11).
	MemberMsg struct {
		PIN      string
		MemberID string
		Branch   string
		WorkPath string
	}
	// PendingMsg is the current join-request queue (oldest first). The session
	// document only carries admitted members; knocks ride this separate feed.
	PendingMsg []controlplane.Member
)

// feedItem is one line of the feed: a control-plane event or a diverted log
// line, kept in one list so the tab reads chronologically rather than as two
// interleaved streams a human has to merge in their head.
type feedItem struct {
	when time.Time
	text string
}

const (
	tabDashboard = "dashboard"
	tabDev       = "dev"
	tabFeed      = "feed"
	tabUsers     = "users"
)

// rosterRow is one line of the append-only users tab. Selection pins to ID,
// never to an index — a knock arriving mid-glance must not slide someone else
// under the cursor.
type rosterRow struct {
	ID, Name, Machine, State, Role string
}

var defaultRoles = []string{"merger", "error-watcher", "setup"}

// Model is the console. It is a bubbletea model, but every method is callable
// directly, which is how the tests drive it without a terminal.
type Model struct {
	opt   Options
	tabs  []string
	tab   int
	w, h  int
	ready bool

	sess    controlplane.Session
	events  []feedItem
	lastSeq int64
	logs    []devserver.Line
	agent   map[string][]string
	// errs is the live problem set, keyed by feed source and kept in first-seen
	// order so the banner does not reshuffle under a reader.
	errs     map[string]error
	errOrder []string
	confirm  bool
	quitting bool

	behind   int
	boxReach BoxMsg

	// seen is whether the control plane has ever answered for this PIN. Before
	// it has, the session is standing up behind this screen and a 404 is the
	// expected answer rather than a fault; after it has, the same 404 means the
	// session went away and belongs on screen.
	seen bool

	// roster is the append-only users-tab list. Admitted members from the
	// session, pending knocks, and people who left all stay here once seen.
	roster     []rosterRow
	selectedID string

	// devURL is the resolved, openable dev address (see DevURLMsg).
	devURL string
}

// New builds a console for one session.
func New(opt Options) *Model {
	roles := opt.Roles
	if len(roles) == 0 {
		roles = defaultRoles
	}
	tabs := append([]string{tabDashboard, tabDev, tabFeed, tabUsers}, roles...)
	return &Model{opt: opt, tabs: tabs, agent: map[string][]string{},
		errs: map[string]error{}, w: 100, h: 30}
}

// Quitting reports whether the console has accepted a quit.
func (m *Model) Quitting() bool { return m.quitting }

func (m *Model) Init() tea.Cmd { return nil }

// Update folds one message in. Keys are navigation and the quit confirm, and
// nothing else: the read-only rule lives here.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h, m.ready = msg.Width, msg.Height, true
	case SessionMsg:
		m.applySession(controlplane.Session(msg))
	case EventsMsg:
		m.applyEvents(msg)
	case DevURLMsg:
		m.devURL = string(msg)
	case LogsMsg:
		m.applyLogLines(msg)
	case LogsResetMsg:
		m.logs = nil
	case AgentOutputMsg:
		m.agent[msg.Role] = appendCapped(m.agent[msg.Role],
			strings.Split(strings.TrimSuffix(msg.Text, "\n"), "\n"), 2000)
	case LogMsg:
		m.applyLog(msg)
	case BoxMsg:
		m.boxReach = msg
	case BehindMsg:
		m.behind = int(msg)
	case MemberMsg:
		m.applyMember(msg)
	case PendingMsg:
		m.applyPending(msg)
	case ErrorMsg:
		m.applyError(msg)
	case tea.KeyMsg:
		return m, m.key(msg)
	}
	return m, nil
}

func (m *Model) key(msg tea.KeyMsg) tea.Cmd {
	s := msg.String()
	if m.confirm {
		switch s {
		case "y", "Y", "enter":
			m.confirm, m.quitting = false, true
			return m.leave()
		default:
			// Anything else declines. A confirm that only listens for "n" is a
			// confirm you can get stuck in.
			m.confirm = false
		}
		return nil
	}
	switch s {
	case "q", "ctrl+c", "esc":
		m.confirm = true
	case "tab", "right", "l":
		m.tab = (m.tab + 1) % len(m.tabs)
	case "shift+tab", "left", "h":
		m.tab = (m.tab - 1 + len(m.tabs)) % len(m.tabs)
	default:
		if m.onUsers() {
			if cmd := m.usersKey(s); cmd != nil {
				return cmd
			}
		}
		if n := int(s[0]) - '1'; len(s) == 1 && n >= 0 && n < len(m.tabs) {
			m.tab = n
		}
	}
	return nil
}

func (m *Model) onUsers() bool {
	return m.tab >= 0 && m.tab < len(m.tabs) && m.tabs[m.tab] == tabUsers
}

// usersKey is the only write path besides leave. Accept/decline have no confirm
// — a person is blocked on the other end.
func (m *Model) usersKey(s string) tea.Cmd {
	switch s {
	case "up", "k":
		m.moveSelection(-1)
	case "down", "j":
		m.moveSelection(1)
	case "a":
		return m.decide(controlplane.DecisionAccept)
	case "d":
		return m.decide(controlplane.DecisionDecline)
	case "c":
		return m.flipAccess()
	}
	return nil
}

func (m *Model) decide(decision string) tea.Cmd {
	id := m.selectedID
	if id == "" || m.opt.Decide == nil {
		return nil
	}
	fn := m.opt.Decide
	return func() tea.Msg {
		err := fn(context.Background(), id, decision)
		return ErrorMsg{Source: "admission", Err: err}
	}
}

func (m *Model) flipAccess() tea.Cmd {
	if m.opt.SetAccess == nil {
		return nil
	}
	cur := m.sess.Access
	if cur == "" {
		cur = controlplane.AccessOpen
	}
	next := controlplane.AccessClosed
	if cur == controlplane.AccessClosed {
		next = controlplane.AccessOpen
	}
	fn := m.opt.SetAccess
	return func() tea.Msg {
		err := fn(context.Background(), next)
		return ErrorMsg{Source: "admission", Err: err}
	}
}

// leave runs the caller's quit action and then ends the program. Everything the
// console does TO the session goes through this one path — it is the whole
// write surface, and it only runs after a confirm.
//
// Deliberately synchronous. Handing the leases off and leaving the session is
// the last thing this process does, so there is no frame after it worth keeping
// responsive, and doing it as a background command would race the program's own
// shutdown — the failure mode being a member who quit without handing anything
// over.
func (m *Model) leave() tea.Cmd {
	if m.opt.Quit != nil {
		if err := m.opt.Quit(context.Background()); err != nil {
			m.applyError(ErrorMsg{Source: "leave", Err: err})
		}
	}
	return tea.Quit
}

// applyError folds one source's outcome in. Every source reports every tick, so
// this is as much about clearing as about setting: a failure that stops
// happening must leave the screen, or a startup blip is on the dashboard for
// the rest of the session under a box that reads "git ok".
func (m *Model) applyError(msg ErrorMsg) {
	// A PIN the control plane has never heard of is this session standing up
	// behind the screen, not a fault. Painting the banner for it would make
	// every startup look broken for its first seconds — and the console now
	// draws before the standup that creates the session row has run.
	if !m.seen && errors.Is(msg.Err, controlplane.ErrNoSession) {
		msg.Err = nil
	}
	if _, known := m.errs[msg.Source]; !known {
		m.errOrder = append(m.errOrder, msg.Source)
	}
	m.errs[msg.Source] = msg.Err
}

func (m *Model) applySession(s controlplane.Session) {
	m.sess = s
	m.seen = true
	m.mergeAdmitted(s.Members)
}

func (m *Model) applyPending(list []controlplane.Member) {
	m.mergePending(list)
}

// applyMember folds in facts the standup resolved after this screen was already
// drawing. Empty fields leave what is known alone: a later message that only
// carries a branch must not blank the work tree.
func (m *Model) applyMember(msg MemberMsg) {
	if msg.PIN != "" {
		m.opt.PIN = msg.PIN
	}
	if msg.MemberID != "" {
		m.opt.MemberID = msg.MemberID
	}
	if msg.Branch != "" {
		m.opt.Branch = msg.Branch
	}
	if msg.WorkPath != "" {
		m.opt.WorkPath = msg.WorkPath
	}
}

func (m *Model) applyEvents(events []controlplane.Event) {
	for _, e := range events {
		if e.Seq <= m.lastSeq {
			continue
		}
		m.lastSeq = e.Seq
		// Local, because diverted log lines land on this same feed stamped with
		// local time.Now() — one instant appearing twice in two zones is worse
		// than either zone (plan 40).
		m.events = appendCapped(m.events, []feedItem{{when: e.CreatedAt.Local(), text: describe(e)}}, 500)
	}
}

// applyLog places a diverted log line. Fleet roles have their own tab; anything
// else — the host loop, the join daemon, git — lands in the feed, so the
// console never silently swallows a line that used to be on screen.
func (m *Model) applyLog(msg LogMsg) {
	role := msg.Component
	if role == "watcher" {
		role = "error-watcher"
	}
	for _, tab := range m.roleTabs() {
		if tab == role {
			m.agent[role] = appendCapped(m.agent[role], []string{msg.Text}, 2000)
			return
		}
	}
	m.events = appendCapped(m.events, []feedItem{{when: time.Now(), text: msg.Component + ": " + msg.Text}}, 500)
}

// myMember is this machine's row in the member list, and whether it is there at
// all — which is how the screen knows this machine's own join has landed. ID is
// authoritative when the box registered as "host" or the name on disk differs
// from $USER.
func (m *Model) myMember() (controlplane.Member, bool) {
	for _, mem := range m.sess.Members {
		if m.opt.MemberID != "" && mem.ID == m.opt.MemberID {
			return mem, true
		}
	}
	for _, mem := range m.sess.Members {
		if mem.Name == m.opt.Me {
			return mem, true
		}
	}
	return controlplane.Member{Name: m.opt.Me}, false
}

func (m *Model) View() string {
	var b strings.Builder
	b.WriteString(m.tabBar() + "\n")
	switch tab := m.tabs[m.tab]; tab {
	case tabDashboard:
		b.WriteString(m.dashboard())
	case tabDev:
		b.WriteString(m.logView())
	case tabFeed:
		b.WriteString(m.feed())
	case tabUsers:
		b.WriteString(m.users())
	default:
		b.WriteString(m.agentView(tab))
	}
	for _, source := range m.errOrder {
		if err := m.errs[source]; err != nil {
			b.WriteString("\n" + styleWarn.Render("! "+err.Error()))
		}
	}
	if m.confirm {
		b.WriteString("\n\n" + m.confirmBanner())
	} else {
		b.WriteString("\n\n" + styleDim.Render("tab/1-9 switch · q leave the session"))
		if m.onUsers() {
			b.WriteString("\n" + styleDim.Render("users: ↑↓ select · a accept · d decline · c access"))
		}
	}
	return b.String()
}

func (m *Model) tabBar() string {
	var parts []string
	pending := m.pendingCount()
	for i, t := range m.tabs {
		label := t
		if t == tabUsers && pending > 0 {
			label = fmt.Sprintf("users (%d)", pending)
		}
		text := fmt.Sprintf(" %d %s ", i+1, label)
		if i == m.tab {
			parts = append(parts, styleTabOn.Render(text))
			continue
		}
		parts = append(parts, styleTabOff.Render(text))
	}
	return strings.Join(parts, "")
}

func (m *Model) dashboard() string {
	var b strings.Builder
	join := "minting session…"
	if m.opt.PIN != "" {
		join = "slopball join " + m.opt.PIN
	}
	fmt.Fprintf(&b, "\n  %s\n\n", stylePIN.Render(join))

	if !m.seen {
		// The screen comes up first and the session stands up behind it, so say
		// that, rather than rendering a dashboard full of zeroes that pretends to
		// describe something. What is local and already true — where you edit —
		// still shows below.
		fmt.Fprintf(&b, "  %s\n", styleDim.Render("session   standing up…"))
	} else {
		online, names := m.onlineMembers()
		line := fmt.Sprintf("  members   %d online   %s", len(online), strings.Join(names, ", "))
		if n := m.pendingCount(); n > 0 {
			line += fmt.Sprintf(" · %d join request(s)", n)
		}
		fmt.Fprintf(&b, "%s\n", line)

		if line := m.boxLine(); line != "" {
			b.WriteString(line + "\n")
		}

		// A service nobody could START says so here, named. The session this
		// exists for showed "conductor off" for eleven minutes while one laptop
		// failed every five seconds and only its own stdout knew why.
		for _, line := range m.startFailureLines() {
			b.WriteString("  " + styleWarn.Render("! "+line) + "\n")
		}

		if m.behind > 0 {
			fmt.Fprintf(&b, "  main      %d commit(s) behind main — run sync to take them\n", m.behind)
		} else {
			fmt.Fprintf(&b, "  main      level with main\n")
		}
	}
	if m.opt.Branch != "" {
		fmt.Fprintf(&b, "  branch    %s\n", m.opt.Branch)
	}
	if m.opt.WorkPath != "" {
		fmt.Fprintf(&b, "  workspace %s\n", m.opt.WorkPath)
		fmt.Fprintf(&b, "            slopball open — new terminal\n")
	}
	if url := m.devURL; url != "" {
		fmt.Fprintf(&b, "  site      %s\n", url)
	} else {
		fmt.Fprintf(&b, "  site      no dev server up yet\n")
	}

	b.WriteString("\n")
	for _, role := range m.roleNames() {
		b.WriteString("  " + m.roleLine(role) + "\n")
	}
	return b.String()
}

// startFailureLines is one line per service a member took and could not start,
// in the session's own service order.
func (m *Model) startFailureLines() []string {
	var out []string
	for _, svc := range controlplane.Services {
		if f := m.sess.Leases[svc].StartFailure; f != nil {
			out = append(out, f.Line(svc))
		}
	}
	return out
}

// roleLine is one role's dot, its activity, and how long it has been at it.
// A role nobody has published recently is reported as stale rather than left
// showing whatever it was doing when the elector's laptop closed.
func (m *Model) roleLine(role string) string {
	r, ok := m.roles()[role]
	label := fmt.Sprintf("%-14s", role)
	harness := roleHarnessLabel(r, ok)
	switch {
	case !ok && m.fleetComingUp():
		// Coming up, not absent. The signal is a held conductor lease (or a
		// session this screen has not even seen yet), never elapsed time: a
		// member that holds the lease and has not sampled a role yet is the
		// window every startup passes through, and one that nobody holds is
		// genuinely nothing conducting.
		return styleDim.Render(label + harnessLabelPad(harness) + "○ starting")
	case !ok:
		return styleDim.Render(label + harnessLabelPad(harness) + "○ not running")
	case r.UpdatedAt.IsZero():
		// Published as part of the fleet but never sampled: it is coming up, not
		// going stale. Calling it stale (with a "?" where the duration goes) makes
		// every startup read as a broken session, which is what the console now
		// shows first rather than last.
		return styleDim.Render(label + harnessLabelPad(harness) + "○ starting")
	case r.Stale(time.Now()):
		return styleDim.Render(fmt.Sprintf("%s%s○ stale — nothing published for %s", label, harnessLabelPad(harness), since(r.UpdatedAt)))
	case r.Working(time.Now()):
		return fmt.Sprintf("%s%s%s %s %s", label, harnessLabelPad(harness), styleWorking.Render("●"), r.Activity, styleDim.Render("("+since(r.Since)+")"))
	default:
		return fmt.Sprintf("%s%s%s idle", label, harnessLabelPad(harness), styleIdle.Render("○"))
	}
}

// fleetComingUp reports whether a role with no published state is on its way
// rather than missing.
func (m *Model) fleetComingUp() bool {
	if !m.seen {
		return true // the session itself is still standing up behind this screen
	}
	if _, joined := m.myMember(); !joined {
		return true // this machine's own join has not landed yet
	}
	return m.sess.Leases[controlplane.ServiceConductor].Live(time.Now())
}

func roleHarnessLabel(r controlplane.RoleAgent, ok bool) string {
	if !ok {
		return ""
	}
	var s string
	if r.Harness != "" {
		s = r.Harness
		if r.Model != "" {
			s += "/" + r.Model
		}
	}
	if r.Mechanical {
		if s != "" {
			s += " (mechanical here)"
		} else {
			s = "(mechanical here)"
		}
	}
	return s
}

// harnessLabelPad keeps the state column aligned. The width is a minimum, never
// a maximum: a model name is unbounded ("cursor/composer-2.5-fast" is 24), and a
// fixed %-18s collapsed to nothing and fused the state glyph onto the model.
func harnessLabelPad(harness string) string {
	if harness == "" {
		return ""
	}
	if len(harness) >= 18 {
		return harness + " "
	}
	return fmt.Sprintf("%-18s", harness)
}

func (m *Model) boxLine() string {
	if m.sess.Box == nil {
		return ""
	}
	box := *m.sess.Box
	if box.Pending() {
		return "  box       provisioning…"
	}
	if box.State == controlplane.BoxFailed {
		err := strings.TrimSpace(box.Error)
		if i := strings.Index(err, "\n"); i >= 0 {
			err = err[:i]
		}
		if err == "" {
			err = "provisioning failed"
		}
		return "  box       failed — " + err
	}
	prov := box.Provider
	if prov == "" {
		prov = "box"
	}
	// Only claim something about a service the session has actually published.
	// Before that it is coming up, and "dev unreachable from here" for a dev
	// server nobody has started yet is a fault report at the moment nothing is
	// wrong — which is the whole difference between a session starting and a
	// session broken.
	parts := []string{"ready", prov}
	for _, svc := range []struct {
		name string
		r    reach.Result
	}{{"git", m.boxReach.Git}, {"dev", m.boxReach.Dev}} {
		switch {
		case !svc.r.Published:
			continue
		case svc.r.Reachable:
			parts = append(parts, svc.name+" ok")
		default:
			parts = append(parts, svc.name+" unreachable from here")
		}
	}
	if len(parts) == 2 {
		parts = append(parts, "services coming up…")
	}
	// Which path git took, not where the relay lives: plan 38 wants the CHOICE
	// visible (a dual path you cannot see is the hour lost on stage), and the
	// address is a debugging fact that belongs in monitor — and the thing that
	// ran this line off the side of the terminal.
	if via := pathKind(m.boxReach.Git.Via); via != "" {
		parts = append(parts, "via "+via)
	}
	return "  box       " + strings.Join(parts, " · ")
}

// pathKind keeps "direct" or "relay" from sessionnet's "<how> <addr>".
func pathKind(path string) string {
	kind, _, _ := strings.Cut(path, " ")
	return kind
}

func (m *Model) feed() string {
	if len(m.events) == 0 {
		return "\n  nothing has happened yet.\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	start := 0
	if n := m.h - 8; n > 0 && len(m.events) > n {
		start = len(m.events) - n
	}
	for _, it := range m.events[start:] {
		fmt.Fprintf(&b, "  %s  %s\n", styleDim.Render(it.when.Format("15:04:05")), it.text)
	}
	return b.String()
}

// describe turns one event into the line a human reads. Kinds it does not know
// are still shown — a feed that silently drops what it does not recognise is
// worse than one with an ugly line in it.
func describe(e controlplane.Event) string {
	switch e.Kind {
	case "member.joined":
		return payloadString(e, "name") + " joined"
	case "member.left":
		return payloadString(e, "name") + " left"
	case "member.knock":
		who := payloadString(e, "name")
		if mach := payloadString(e, "machine"); mach != "" {
			who += "@" + mach
		}
		return who + " — join request"
	case "member.declined":
		return payloadString(e, "name") + " — join request declined"
	case controlplane.EventSyncPushed:
		return fmt.Sprintf("%s synced %s — %s",
			payloadString(e, "member"), payloadString(e, "branch"), payloadString(e, "intent"))
	case controlplane.EventMergeApplied:
		line := fmt.Sprintf("merge landed from %s — %s",
			payloadString(e, "branch"), payloadString(e, "intent"))
		if n := payloadInt(e, "conflicts"); n > 0 {
			line += fmt.Sprintf(" (%d conflict(s) resolved)", n)
		}
		return line
	case "error.detected":
		return styleWarn.Render("dev server error detected")
	case "error.resolved":
		return "dev server error resolved"
	case "main.advanced":
		return "main advanced"
	case "endpoint.changed":
		return fmt.Sprintf("%s endpoint moved to %s", payloadString(e, "kind"), payloadString(e, "url"))
	case "conductor.elected":
		line := payloadString(e, "elector") + " is conducting"
		if h := payloadString(e, "harness"); h != "" {
			line += " with " + h
		}
		return line
	case "role.working":
		line := payloadString(e, "role") + " working"
		if a := payloadString(e, "activity"); a != "" {
			line += " — " + a
		}
		return line
	case "role.idle":
		return payloadString(e, "role") + " idle"
	case controlplane.EventPlacementFailed:
		who := payloadString(e, "member")
		if machine := payloadString(e, "machine"); machine != "" {
			who += "@" + machine
		}
		return styleWarn.Render(fmt.Sprintf("%s: %s can't start it — %s",
			payloadString(e, "service"), who, payloadString(e, "reason")))
	default:
		return e.Kind
	}
}

func (m *Model) logView() string {
	if len(m.logs) == 0 {
		return "\n  no dev-server output yet.\n"
	}
	var b strings.Builder
	b.WriteString("\n")
	start := 0
	if n := m.h - 8; n > 0 && len(m.logs) > n {
		start = len(m.logs) - n
	}
	for _, l := range m.logs[start:] {
		if l.Stream == devserver.StreamStderr {
			fmt.Fprintf(&b, "  %s\n", styleWarn.Render(l.Text))
			continue
		}
		fmt.Fprintf(&b, "  %s\n", l.Text)
	}
	return b.String()
}

// agentView is the §10 split made visible: the elector streams, everybody else
// gets the published state and is told whose machine has the rest. Agent output
// is bulk text produced under one person's harness login, and shipping it
// through the control plane would make the addressing spine a log bus.
func (m *Model) agentView(role string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  %s\n\n", m.roleLine(role))
	if m.opt.Elector {
		lines := m.agent[role]
		if len(lines) == 0 {
			b.WriteString("  waiting for " + role + " to do something.\n")
			return b.String()
		}
		start := 0
		if n := m.h - 10; n > 0 && len(lines) > n {
			start = len(lines) - n
		}
		for _, l := range lines[start:] {
			fmt.Fprintf(&b, "  %s\n", l)
		}
		return b.String()
	}
	elector := "the elector"
	if m.sess.Conductor != nil && m.sess.Conductor.Elector != "" {
		elector = m.sess.Conductor.Elector
	}
	fmt.Fprintf(&b, "  the full %s stream is on %s's machine — it runs under that\n"+
		"  harness login and stays there. What travels is the state above.\n", role, elector)
	return b.String()
}

func (m *Model) confirmBanner() string {
	online, _ := m.onlineMembers()
	what := fmt.Sprintf("leave session %s", m.opt.PIN)
	if m.opt.Elector {
		what = fmt.Sprintf("leave session %s and hand off what this machine is running", m.opt.PIN)
	}
	rest := ""
	if others := len(online) - 1; others > 0 {
		rest = fmt.Sprintf(" %d other member(s) stay in the session.", others)
	}
	return styleWarn.Render(fmt.Sprintf("  %s?%s  [y/N]", what, rest))
}

func (m *Model) onlineMembers() (out []controlplane.Member, names []string) {
	for _, mem := range m.sess.Members {
		if mem.Role == controlplane.RoleBox {
			continue
		}
		if mem.State == controlplane.MemberLeft || mem.State == controlplane.MemberPending {
			continue
		}
		if mem.Online {
			out = append(out, mem)
			names = append(names, mem.Name)
		}
	}
	sort.Strings(names)
	return out, names
}

// roleTabs returns the fleet-role tab names (everything after the fixed tabs).
func (m *Model) roleTabs() []string {
	for i, t := range m.tabs {
		if t == tabUsers {
			if i+1 < len(m.tabs) {
				return m.tabs[i+1:]
			}
			return nil
		}
	}
	return nil
}

func (m *Model) roles() map[string]controlplane.RoleAgent {
	if m.sess.Conductor == nil {
		return nil
	}
	return m.sess.Conductor.Roles
}

// roleNames is the display order: the tabs' roles first, then anything the
// session published that we did not expect.
func (m *Model) roleNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range m.roleTabs() {
		seen[t] = true
		out = append(out, t)
	}
	var extra []string
	for role := range m.roles() {
		if !seen[role] {
			extra = append(extra, role)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// raw endpoint ok: rendered on the dashboard for a human to read and open.
// The one endpoint the console DIALS — logs — goes through EndpointURL in
// cli.logsEndpoint.
func (m *Model) endpoint(kind string) string {
	if ep, ok := m.sess.Endpoints[kind]; ok {
		return ep.URL
	}
	return ""
}

func payloadString(e controlplane.Event, key string) string {
	s, _ := e.Payload[key].(string)
	return s
}

func payloadInt(e controlplane.Event, key string) int {
	switch v := e.Payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func since(t time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := time.Since(t).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

func appendCapped[T any](dst []T, add []T, max int) []T {
	dst = append(dst, add...)
	if len(dst) > max {
		dst = dst[len(dst)-max:]
	}
	return dst
}

var (
	stylePIN     = lipgloss.NewStyle().Bold(true)
	styleTabOn   = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleTabOff  = lipgloss.NewStyle().Faint(true)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleWorking = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleIdle    = lipgloss.NewStyle().Faint(true)
)

// applyLogLines folds a page in, replacing by Seq rather than appending: a line
// still being written is re-sent complete under the same Seq, and appending
// blindly shows a dying dev server's last line twice — which is usually the
// line that says why it died.
func (m *Model) applyLogLines(lines []devserver.Line) {
	for _, l := range lines {
		replaced := false
		for i := len(m.logs) - 1; i >= 0 && i >= len(m.logs)-64; i-- {
			if m.logs[i].Seq == l.Seq && l.Seq != 0 {
				m.logs[i] = l
				replaced = true
				break
			}
		}
		if !replaced {
			m.logs = appendCapped(m.logs, []devserver.Line{l}, 2000)
		}
	}
}
