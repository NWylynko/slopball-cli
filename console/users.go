package console

import (
	"fmt"
	"strings"

	"github.com/nwylynko/slopball-cli/controlplane"
)

const (
	rosterActive   = "active"
	rosterOffline  = "offline"
	rosterLeft     = "left"
	rosterPending  = "pending"
	rosterDeclined = "declined"
)

func (m *Model) pendingCount() int {
	n := 0
	for _, r := range m.roster {
		if r.State == rosterPending {
			n++
		}
	}
	return n
}

// mergeAdmitted folds the present admitted list into the append-only roster.
// Anyone who was active/offline and is gone is left; order never reshuffles.
func (m *Model) mergeAdmitted(members []controlplane.Member) {
	present := map[string]controlplane.Member{}
	for _, mem := range members {
		if mem.Role == controlplane.RoleBox {
			continue
		}
		present[mem.ID] = mem
	}
	seen := map[string]bool{}
	for i := range m.roster {
		r := &m.roster[i]
		seen[r.ID] = true
		if mem, ok := present[r.ID]; ok {
			r.Name, r.Machine, r.Role = mem.Name, mem.Machine, mem.Role
			r.State = admittedState(mem)
			continue
		}
		if r.State == rosterActive || r.State == rosterOffline {
			r.State = rosterLeft
		}
	}
	for _, mem := range members {
		if mem.Role == controlplane.RoleBox || seen[mem.ID] {
			continue
		}
		m.roster = append(m.roster, rosterRow{
			ID: mem.ID, Name: mem.Name, Machine: mem.Machine, Role: mem.Role,
			State: admittedState(mem),
		})
	}
	m.ensureSelection()
}

func admittedState(mem controlplane.Member) string {
	if mem.State == controlplane.MemberLeft {
		return rosterLeft
	}
	if mem.Online {
		return rosterActive
	}
	return rosterOffline
}

// mergePending folds the knock queue. New requests append at the bottom;
// a pending id that vanished without becoming admitted is marked declined so
// the selection does not slide onto the next person.
func (m *Model) mergePending(list []controlplane.Member) {
	pending := map[string]controlplane.Member{}
	for _, mem := range list {
		pending[mem.ID] = mem
	}
	seen := map[string]bool{}
	for i := range m.roster {
		r := &m.roster[i]
		seen[r.ID] = true
		if mem, ok := pending[r.ID]; ok {
			r.Name, r.Machine = mem.Name, mem.Machine
			r.State = rosterPending
			continue
		}
		if r.State == rosterPending {
			r.State = rosterDeclined
		}
	}
	for _, mem := range list {
		if seen[mem.ID] {
			continue
		}
		m.roster = append(m.roster, rosterRow{
			ID: mem.ID, Name: mem.Name, Machine: mem.Machine,
			State: rosterPending,
		})
	}
	m.ensureSelection()
}

func (m *Model) ensureSelection() {
	if m.selectedID != "" {
		for _, r := range m.roster {
			if r.ID == m.selectedID {
				return
			}
		}
	}
	// Prefer a pending knock — that is who is blocked on a decision.
	for _, r := range m.roster {
		if r.State == rosterPending {
			m.selectedID = r.ID
			return
		}
	}
	if len(m.roster) > 0 {
		m.selectedID = m.roster[0].ID
		return
	}
	m.selectedID = ""
}

func (m *Model) moveSelection(delta int) {
	if len(m.roster) == 0 {
		return
	}
	idx := 0
	for i, r := range m.roster {
		if r.ID == m.selectedID {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(m.roster)) % len(m.roster)
	m.selectedID = m.roster[idx].ID
}

func (m *Model) users() string {
	var b strings.Builder
	access := m.sess.Access
	if access == "" {
		access = controlplane.AccessOpen
	}
	fmt.Fprintf(&b, "\n  access  %s   (c to flip open/closed)\n", access)
	fmt.Fprintf(&b, "  %s\n\n", styleDim.Render("↑↓ select · a accept · d decline — no confirm"))
	if len(m.roster) == 0 {
		b.WriteString("  nobody here yet.\n")
		return b.String()
	}
	for _, r := range m.roster {
		cursor := "  "
		if r.ID == m.selectedID {
			cursor = "> "
		}
		who := r.Name
		if r.Machine != "" {
			who += "@" + r.Machine
		}
		state := r.State
		if state == rosterPending {
			state = "pending — join request"
		}
		line := fmt.Sprintf("%s%-28s  %s", cursor, who, state)
		switch r.State {
		case rosterPending:
			b.WriteString(styleWorking.Render(line) + "\n")
		case rosterLeft, rosterDeclined, rosterOffline:
			b.WriteString(styleDim.Render(line) + "\n")
		default:
			b.WriteString(line + "\n")
		}
	}
	return b.String()
}
