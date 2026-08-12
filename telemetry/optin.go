package telemetry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nwylynko/slopball-cli/session"
)

// Telemetry is OFF by default on a machine somebody owns, and that is not
// negotiable (plan 46 ticket 12). A tool whose pitch includes "we are not your
// backup", and whose repo goes public specifically so people can verify it is
// not doing anything behind their back, cannot phone home silently.
//
// The setting follows who owns the MACHINE, not what the process is:
//
//   - a laptop records nothing until a human types `slopball telemetry on`;
//   - a MANAGED box always records — our container on our docker host, and the
//     one machine in a session no laptop's console fully sees (it holds
//     canonical, supervises the dev server and produces /logs), so defaulting
//     it off would silence the highest-value emitter in a box session;
//   - a BYO box inherits the setting of the laptop that ran `box add`, because
//     it is somebody else's hardware.
const (
	ModeOn  = "on"
	ModeOff = "off"

	// EnvMode is the provisioner→container wire: how a box is TOLD what its
	// owner decided. It is not operator configuration and not a flag — a
	// container has no human to ask, so the machine that provisioned it answers
	// on its behalf (AGENTS.md's third non-operator class).
	EnvMode = "SLOPBALL_TELEMETRY"

	// settingKey is where the decision lives in ~/.slopball/defaults.json.
	settingKey = "telemetry"
)

// Resolve answers whether this machine records, and why. The reason is the
// whole point of `status`, which exists for two questions at once: "why did
// this laptop produce no rows" and "prove to me the default is off".
//
// Order: the container wire first, then this machine's file. A box told what to
// do outranks any file in its image, because the file belongs to whoever owns
// the machine and a container has no owner present.
func Resolve() (on bool, why string) {
	if v := strings.TrimSpace(os.Getenv(EnvMode)); v != "" {
		switch strings.ToLower(v) {
		case ModeOn:
			return true, "on — this box was provisioned recording (" + EnvMode + "=on)"
		case ModeOff:
			return false, "off — this box was provisioned not recording (" + EnvMode + "=off)"
		default:
			return false, fmt.Sprintf("off — %s=%q is not on or off, so nothing is recorded", EnvMode, v)
		}
	}
	switch readSetting() {
	case ModeOn:
		return true, "on — you turned it on with `slopball telemetry on` (this machine)"
	case ModeOff:
		return false, "off — you turned it off with `slopball telemetry off` (this machine)"
	default:
		return false, "off — the default, on every machine, until somebody runs `slopball telemetry on`"
	}
}

// SetMode records this machine's decision. Off is WRITTEN rather than left
// absent: a human who turned it off has said something, and a later change of
// default must not silently re-enable them.
func SetMode(on bool) error {
	mode := ModeOff
	if on {
		mode = ModeOn
	}
	return writeSetting(mode)
}

// defaultsPath is ~/.slopball/defaults.json — the same file the wizard keeps
// this machine's prompt pre-fills in.
//
// It shares the file and NOT the ownership: the wizard's entries are
// suggestions for the next new session, while this one is a standing decision a
// running session reads. Both readers merge rather than overwrite (see
// writeSetting and firstrun.SaveDefaults), so neither can clobber the other.
func defaultsPath() string { return filepath.Join(session.Home(), "defaults.json") }

func readSetting() string {
	data, err := os.ReadFile(defaultsPath())
	if err != nil {
		return ""
	}
	var all map[string]any
	if json.Unmarshal(data, &all) != nil {
		return ""
	}
	s, _ := all[settingKey].(string)
	return strings.ToLower(strings.TrimSpace(s))
}

// writeSetting merges into whatever is already there, so the wizard's
// pre-fills survive somebody typing `slopball telemetry on`.
func writeSetting(mode string) error {
	if err := os.MkdirAll(session.Home(), 0o755); err != nil {
		return err
	}
	all := map[string]any{}
	if data, err := os.ReadFile(defaultsPath()); err == nil {
		_ = json.Unmarshal(data, &all)
	}
	all[settingKey] = mode
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(defaultsPath(), append(data, '\n'), 0o644)
}

// MemberConfig is what a member knows about where and how to record: the
// session it belongs to, the ingest that session advertised (ticket 11), and
// the telemetry ticket its member cycle minted (ticket 10).
type MemberConfig struct {
	PIN        string
	SessionUID string
	MemberID   string
	Advertised string
	Ticket     string
	// Version is this client's build. It rides the member config rather than
	// being read here because this package cannot import controlplane — that
	// package imports this one — and the member cycle, which owns the constant
	// the wire header carries, is the single caller.
	Version string
}

// ForMember builds the emitter a member uses, or a disabled one. Three
// independent reasons to record nothing, and each is silent-by-construction
// rather than a branch every caller has to remember:
//
//   - this machine has not opted in;
//   - the session advertises no ingest (reported once, by SessionIngest);
//   - there is no ticket yet, so nothing could be attributed anyway.
//
// A disabled emitter queues nothing and dials nothing, which is what makes
// "a client with telemetry off makes zero telemetry requests" a property of
// the code rather than a promise.
func ForMember(cfg MemberConfig) *Emitter {
	on, _ := Resolve()
	if !on {
		return New(Config{Service: "client", Version: cfg.Version})
	}
	url := SessionIngest(cfg.PIN, cfg.Advertised)
	if url == "" || strings.TrimSpace(cfg.Ticket) == "" {
		return New(Config{Service: "client", Version: cfg.Version})
	}
	return New(Config{URL: url, Bearer: cfg.Ticket, Service: "client", Version: cfg.Version})
}
