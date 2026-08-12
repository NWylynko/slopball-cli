// Package firstrun is slopball's guided first run (plan 29): the questions, the
// detected defaults behind them, and this machine's prompt pre-fills — kept in
// one vertical because a question, its default and its detection belong
// together.
//
// It is deliberately a pure function of (flags, detection, keystrokes) → Plan.
// Nothing here writes session state: the brief goes to canonical, the run
// commands go to canonical, the agents go to the control plane, and all of that
// is the caller's job (MASTERPLAN §9.3 — one smart tap, not a wizard; the
// answers are session state, not wizard memory).
package firstrun

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/slopball-cli/devserver"
	sbGit "github.com/nwylynko/slopball-cli/git"
	"github.com/nwylynko/slopball-cli/harness"
	"github.com/nwylynko/slopball-cli/session"
)

// Agent is one role's intelligence: which CLI, and optionally which model.
// A blank model means the CLI's own default, which is always legal.
type Agent struct {
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

func (a Agent) String() string {
	if a.Harness == "" {
		return "none"
	}
	if a.Model == "" {
		return a.Harness
	}
	return a.Harness + "/" + a.Model
}

// Plan is the fully-resolved answer set. Every field is also expressible as a
// flag, so the wizard's output is one scriptable command line.
type Plan struct {
	Merger, Watcher, Setup Agent
	Brief                  string
	// Box is the MANAGED answer (plan 37): yes/no, no target. The control
	// plane provisions it, so the creator names no machine and needs neither
	// docker nor an ssh key.
	Box bool
	// BoxTarget is the BYO answer — `--box-ssh <user@host>`, provisioning onto
	// a machine you own. It survives as the power-user tier §11 always wanted;
	// it is simply no longer what "do you want a box?" means.
	BoxTarget string
	Install   []string // empty = detect after the scaffold
	Dev       []string // empty = detect after the scaffold
	MirrorURL string   // durability snapshot target; token never travels
	Seeded    bool     // --seed/--seed-url given: question 2 becomes "adapt"
}

// Agents renders the plan as the per-role map hoststart takes.
func (p Plan) Agents() map[string]Agent {
	return map[string]Agent{
		"merger":        p.Merger,
		"error-watcher": p.Watcher,
		"setup":         p.Setup,
	}
}

// SetAllAgents assigns one agent to every role — the default shape.
func (p *Plan) SetAllAgents(a Agent) { p.Merger, p.Watcher, p.Setup = a, a, a }

// Question keys, used by callers to mark what a flag already answered.
const (
	QAgents  = "agents"
	QBrief   = "brief"
	QBox     = "box"
	QInstall = "install"
	QDev     = "dev"
	QMirror  = "mirror"
)

// Detect builds the pre-filled plan from this machine: the first harness CLI
// actually on PATH runs every role, and a seed repo's own origin pre-fills the
// mirror. Nothing slopball can detect is ever asked (§9.3).
func Detect(seedDir string) Plan {
	var p Plan
	for _, a := range harness.Available() {
		if a.Present {
			p.SetAllAgents(Agent{Harness: string(a.Name)})
			break
		}
	}
	p.Seeded = seedDir != ""
	if seedDir != "" {
		p.MirrorURL = originOf(seedDir)
	}
	return p
}

// originOf reads a brought repo's own push target — the mirror it already has.
func originOf(dir string) string {
	out, err := sbGit.Output(context.Background(), dir, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Ask fills the gaps in base by prompting, and returns the resolved plan.
// `asked` names the questions a flag already answered, which are skipped
// entirely rather than shown with a pre-fill. `remembered` is this machine's
// last session, offered up front as question zero.
//
// Deliberately no TUI: bufio over the reader, numbered menus, `[default]` in
// the prompt, Enter accepts. No raw mode, no arrow keys, no new dependency.
// (The session console this hands over to is a full TUI — plan 36 §3 — but a
// linear question flow genuinely does not need one.)
func Ask(in io.Reader, out io.Writer, base Plan, asked map[string]bool, remembered Remembered) (Plan, error) {
	p := base
	s := bufio.NewScanner(in)
	// A brief can be a long sentence; the default 64KiB token limit is plenty,
	// but be explicit that lines are the unit.
	s.Buffer(make([]byte, 0, 4096), 1<<20)
	q := &prompter{in: s, out: out}

	// Question zero. Everything this machine answered last time, shown as a
	// block and accepted with one Enter — which leaves only the brief, the one
	// answer that is genuinely about *this* session.
	if remembered.Exists {
		p = remembered.apply(p)
		fmt.Fprint(out, remembered.render())
		if q.yesNo("use these?", true) {
			asked = withAll(asked, QAgents, QBox, QMirror)
		} else {
			// Declining drops back to the individual prompts, where a remembered
			// target is a pre-fill and never an implied yes.
			p.Box = remembered.UsedBox
			p.BoxTarget = remembered.Plan.BoxTarget
		}
	}

	avail := harness.Available()
	installed := make([]harness.Name, 0, len(avail))
	var names []string
	for _, a := range avail {
		mark := "✗"
		if a.Present {
			mark = "✓"
			installed = append(installed, a.Name)
		}
		names = append(names, fmt.Sprintf("%s %s", a.Name, mark))
	}

	if !asked[QAgents] {
		fmt.Fprintf(out, "\n  agents on PATH: %s\n\n", strings.Join(names, "  "))
		if len(installed) == 0 {
			fmt.Fprintf(out, "  no agent CLI installed — the fleet will merge mechanically and cannot scaffold.\n"+
				"  install Claude Code / Codex / Cursor and restart to get the full fleet.\n\n")
		} else if q.yesNo("use one agent for the whole fleet?", true) {
			a := q.agent("agent", installed, p.Merger)
			p.SetAllAgents(a)
		} else {
			p.Merger = q.agent("merger", installed, p.Merger)
			p.Watcher = q.agent("error-watcher", installed, p.Watcher)
			p.Setup = q.agent("setup", installed, p.Setup)
		}
	}

	if !asked[QBrief] {
		if p.Seeded {
			// A brief against a repo the human brought puts plan 28's setup role
			// into adapt mode — an agent editing their code. Name it first.
			fmt.Fprintf(out, "\n  what should we turn this repo into?  (blank = leave it as-is)\n"+
				"  a non-blank answer lets the setup agent make ONE commit against the repo you seeded.\n")
		} else {
			fmt.Fprintf(out, "\n  what are we building?  (blank to skip)\n")
		}
		// Under --seed p.Brief is blank by construction (Remembered.apply refuses
		// to pre-fill it), so Enter here leaves the brought repo alone.
		p.Brief = q.line(fmt.Sprintf("  >%s ", defaultHint(p.Brief)), p.Brief)
	}

	if !asked[QBox] {
		// The question lost its target (plan 37). Nothing follows a yes: the
		// control plane provisions the box, and this machine never learns where
		// it runs. `--box-ssh <user@host>` is still there for a machine you own.
		fmt.Fprintf(out, "\n  run this session on a box?  (we host it; your laptop can close)\n"+
			"  we are not your backup — the box is for liveness. `--mirror` snapshots to GitHub.\n")
		p.Box = q.yesNo("  box?", p.Box)
	}

	// Install and dev are deliberately NOT asked (plan 36 §1). Their default is
	// already "detect after the scaffold", which is right nearly always, and a
	// question whose best answer is Enter is the wizard-ness §9.3 argues
	// against. `--install` / `--dev` still answer them for scripting, and
	// recordRunCommands still *reports* what detection found — it stopped being
	// a prompt, not a fact.

	if !asked[QMirror] {
		p.MirrorURL = q.line(fmt.Sprintf("  github snapshot   (blank = off)%s > ", defaultHint(p.MirrorURL)), p.MirrorURL)
		if p.MirrorURL != "" {
			if name, tok := MirrorCredential(); tok != "" {
				fmt.Fprintf(out, "    ✓ %s found — mirroring from this machine (the token never leaves it)\n", name)
			} else {
				// Accept the URL anyway: a safety net configured now and
				// credentialed in a minute beats one never configured. But say it.
				fmt.Fprintf(out, "    ! no GitHub token in this environment (GITHUB_TOKEN / GH_TOKEN) —\n"+
					"      the URL is recorded, but mirroring stays paused until one is set here\n")
			}
		}
	}

	if err := s.Err(); err != nil {
		return p, err
	}
	return p, nil
}

// MirrorCredential reports which env var holds the durability token on this
// machine, if any. The token is host-only and never published (§4.4).
func MirrorCredential() (string, string) {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(name); v != "" {
			return name, v
		}
	}
	return "", ""
}

type prompter struct {
	in  *bufio.Scanner
	out io.Writer
}

// line prints the prompt and returns the typed answer, or def on a bare Enter.
// EOF behaves like Enter, so a closed stdin resolves to the defaults instead of
// spinning.
func (q *prompter) line(prompt, def string) string {
	fmt.Fprint(q.out, prompt)
	if !q.in.Scan() {
		fmt.Fprintln(q.out)
		return def
	}
	txt := strings.TrimSpace(q.in.Text())
	if txt == "" {
		return def
	}
	return txt
}

func (q *prompter) yesNo(question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		switch strings.ToLower(q.line(fmt.Sprintf("  %s %s > ", question, hint), "")) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(q.out, "  please answer y or n")
	}
}

// agent asks which CLI runs one role, plus its model. Picking a CLI that is not
// installed re-asks with the reason — never a silent downgrade to mechanical
// merging, because the human explicitly asked for an agent.
func (q *prompter) agent(role string, installed []harness.Name, def Agent) Agent {
	if len(installed) == 0 {
		return Agent{}
	}
	var menu []string
	for i, n := range installed {
		menu = append(menu, fmt.Sprintf("[%d] %s", i+1, n))
	}
	pick := def
	for {
		ans := q.line(fmt.Sprintf("    %-14s %s%s > ", role, strings.Join(menu, " "), defaultHint(def.Harness)), def.Harness)
		chosen := ""
		for i, n := range installed {
			if ans == fmt.Sprint(i+1) || strings.EqualFold(ans, string(n)) {
				chosen = string(n)
				break
			}
		}
		if chosen != "" {
			pick.Harness = chosen
			break
		}
		fmt.Fprintf(q.out, "    %q is not installed on this machine — pick one of: %s\n",
			ans, strings.Join(menu, " "))
	}
	hints := harness.SuggestedModels(harness.Name(pick.Harness))
	label := fmt.Sprintf("[%s default]", pick.Harness)
	if len(hints) > 0 {
		label = fmt.Sprintf("[%s default; e.g. %s]", pick.Harness, strings.Join(hints, ", "))
	}
	model := def.Model
	if def.Harness != pick.Harness {
		model = "" // a different CLI's model means nothing here
	}
	pick.Model = q.line(fmt.Sprintf("      model     %s%s > ", label, defaultHint(model)), model)
	return pick
}

func defaultHint(def string) string {
	if def == "" {
		return ""
	}
	return "  [" + def + "]"
}

func splitCmd(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

// --- machine defaults (prompt pre-fill only) --------------------------------

// defaultsFile is this machine's last session, as prompt pre-fill. It is a
// pre-fill for the prompts of the *next new session* and nothing else — never
// authoritative, never read by a running session.
//
// It now remembers the brief and the mirror too (plan 36 §1). That does not
// contradict the rule it used to state: session-scoped answers still live in
// canonical, which stays the authority every machine reads. What is here is a
// suggestion for a prompt on a machine that has not created the session yet —
// it is overwritten by the first thing canonical says, and a session in flight
// never opens this file. The run commands stay out because they are *detected*,
// not answered.
func defaultsFile() string { return filepath.Join(session.Home(), "defaults.json") }

type machineDefaults struct {
	Merger    Agent  `json:"merger,omitempty"`
	Watcher   Agent  `json:"watcher,omitempty"`
	Setup     Agent  `json:"setup,omitempty"`
	Brief     string `json:"brief,omitempty"`
	BoxTarget string `json:"boxTarget,omitempty"`
	MirrorURL string `json:"mirrorUrl,omitempty"`
	// Telemetry is NOT the wizard's to answer — it is this machine's standing
	// decision, owned by `slopball telemetry on|off` (plan 46 ticket 12). It is
	// carried here only so SaveDefaults writes it back unchanged: two writers
	// share this file, and neither may clobber the other's keys.
	Telemetry string `json:"telemetry,omitempty"`
	// UsedBox records whether the last session actually ran on a box, which is
	// what lets question zero say "box nick@remote-box" and mean it. Without it
	// a remembered target could only ever be a pre-fill, and "use these?" would
	// be lying about one of its lines.
	UsedBox bool `json:"usedBox,omitempty"`
}

// Remembered is this machine's last session, offered as question zero. Zero
// value = nothing remembered, so the wizard walks every question.
type Remembered struct {
	Plan    Plan
	UsedBox bool
	Exists  bool
}

// Recall loads this machine's remembered block. Missing or unreadable → not
// exists, which just means the full walk.
func Recall() Remembered {
	d, err := loadDefaults()
	if err != nil {
		return Remembered{}
	}
	return Remembered{
		Exists:  true,
		UsedBox: d.UsedBox,
		Plan: Plan{
			Merger: d.Merger, Watcher: d.Watcher, Setup: d.Setup,
			Brief: d.Brief, BoxTarget: d.BoxTarget, MirrorURL: d.MirrorURL,
		},
	}
}

// apply lays the remembered answers over p as pre-fills, ready for either an
// accepting Enter or the walk that follows a "no".
func (r Remembered) apply(p Plan) Plan {
	p = ApplyDefaults(p, r.Plan)
	// UsedBox IS the remembered box answer now (plan 37 §7). It used to be
	// display-only beside a target; with the question reduced to yes/no there is
	// nothing else it could mean.
	p.Box = r.UsedBox
	if !r.UsedBox {
		p.BoxTarget = ""
	}
	return p
}

// render is the block a human reads before accepting it. Every line that will
// take effect is named — a confirmed box target is applied, so it has to be
// visible first.
func (r Remembered) render() string {
	var b strings.Builder
	b.WriteString("\n  last time on this machine:\n")
	agents := r.Plan.Merger.String()
	if r.Plan.Watcher != r.Plan.Merger || r.Plan.Setup != r.Plan.Merger {
		agents = fmt.Sprintf("merger %s  error-watcher %s  setup %s",
			r.Plan.Merger, r.Plan.Watcher, r.Plan.Setup)
	}
	fmt.Fprintf(&b, "    agents  %s\n", agents)
	switch {
	case r.UsedBox && r.Plan.BoxTarget != "":
		fmt.Fprintf(&b, "    box     %s (your machine)\n", r.Plan.BoxTarget)
	case r.UsedBox:
		b.WriteString("    box     yes (we provision it)\n")
	default:
		b.WriteString("    box     none\n")
	}
	if r.Plan.MirrorURL != "" {
		fmt.Fprintf(&b, "    mirror  %s\n", r.Plan.MirrorURL)
	}
	b.WriteString("\n")
	return b.String()
}

func withAll(asked map[string]bool, keys ...string) map[string]bool {
	out := map[string]bool{}
	for k, v := range asked {
		out[k] = v
	}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

func loadDefaults() (machineDefaults, error) {
	var d machineDefaults
	data, err := os.ReadFile(defaultsFile())
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, err
	}
	return d, nil
}

// Defaults loads this machine's remembered pre-fills. Missing or unreadable →
// a zero Plan, which just means "no pre-fill".
func Defaults() Plan { return Recall().Plan }

// SaveDefaults remembers what this machine answered, for the next new session's
// prompts. The run commands are never saved: they are detected, not answered.
func SaveDefaults(p Plan) error {
	if err := os.MkdirAll(session.Home(), 0o755); err != nil {
		return err
	}
	d := machineDefaults{
		Merger: p.Merger, Watcher: p.Watcher, Setup: p.Setup,
		Brief: p.Brief, BoxTarget: p.BoxTarget, MirrorURL: p.MirrorURL,
		UsedBox: p.Box || p.BoxTarget != "",
	}
	// Preserve what this machine decided about telemetry. The wizard never asks
	// and never changes it; losing it here would silently opt somebody out.
	if prev, err := loadDefaults(); err == nil {
		d.Telemetry = prev.Telemetry
	}
	if !d.UsedBox {
		// A boxless session keeps the last known target as a *prompt* pre-fill
		// while recording that it was not used — so question zero says "box
		// none" truthfully and the ssh prompt still saves you the typing.
		if prev, err := loadDefaults(); err == nil {
			d.BoxTarget = prev.BoxTarget
		}
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsFile(), append(data, '\n'), 0o644)
}

// ApplyDefaults layers this machine's remembered choices over detection, for
// fields the caller has not already resolved from a flag. Precedence is
// explicit flag → machine defaults → detection.
func ApplyDefaults(base, d Plan) Plan {
	if d.Merger.Harness != "" && harness.IsAvailable(d.Merger.Harness) {
		base.Merger = d.Merger
	}
	if d.Watcher.Harness != "" && harness.IsAvailable(d.Watcher.Harness) {
		base.Watcher = d.Watcher
	}
	if d.Setup.Harness != "" && harness.IsAvailable(d.Setup.Harness) {
		base.Setup = d.Setup
	}
	// BoxTarget is deliberately NOT layered in. It used to pre-fill the "ssh
	// target" prompt; that prompt is gone (plan 37 §7), so carrying it forward
	// would provision onto a remembered machine that nobody named this time.
	// --box-ssh is how you ask for that, every time.
	if d.MirrorURL != "" && base.MirrorURL == "" {
		base.MirrorURL = d.MirrorURL
	}
	// Never under --seed: a non-blank brief there puts the setup role into adapt
	// mode and makes a commit against a repo the human brought. A bare Enter
	// must not do that.
	if d.Brief != "" && base.Brief == "" && !base.Seeded {
		base.Brief = d.Brief
	}
	return base
}

// ResolveCommands fills blank install/dev answers from the scaffolded tree —
// the plan's "detect after the project exists" default, which is what makes
// asking everything up front compatible with correct answers.
func ResolveCommands(p Plan, workDir string) Plan {
	if len(p.Install) == 0 {
		p.Install = devserver.DetectInstall(workDir)
	}
	if len(p.Dev) == 0 {
		p.Dev = devserver.DetectDev(workDir)
	}
	return p
}
