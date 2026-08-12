package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// RelayTicketsPath is the on-disk cache of Ed25519 session-network tickets
// (abuse-surface ticket 17). A cold `slopball box run` process has membership
// on disk but no in-memory Client cache — without this file it cannot connect
// to a ticket-requiring relay until a member cycle runs.
func (p Paths) RelayTickets() string { return filepath.Join(p.Root, "relay-tickets.json") }

// WriteRelayTickets persists service → ticket. Empty map is a no-op.
func WriteRelayTickets(pin string, tickets map[string]string) error {
	if len(tickets) == 0 {
		return nil
	}
	p := ForPin(pin)
	if err := os.MkdirAll(p.Root, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(tickets)
	if err != nil {
		return err
	}
	return os.WriteFile(p.RelayTickets(), b, 0o600)
}

// ReadRelayTickets returns the persisted map, or nil when absent.
func ReadRelayTickets(pin string) map[string]string {
	b, err := os.ReadFile(ForPin(pin).RelayTickets())
	if err != nil || len(b) == 0 {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
