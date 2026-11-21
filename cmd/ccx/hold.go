package main

import (
	"log"
	"path/filepath"
	"time"
)

func (p paths) holdFile() string { return filepath.Join(p.claudeDir, "switcher", "hold.json") }

// sessionReset reads the account's upcoming 5h reset from a snapshot. A reset
// already in the past belongs to a closed window
func sessionReset(snap usageSnapshot, now time.Time) (time.Time, bool) {
	reset, ok := parseReset(snap.Headers["anthropic-ratelimit-unified-5h-reset"], now)
	if !ok || !reset.After(now) {
		return time.Time{}, false
	}
	return reset, true
}

// holding is the account ccx use chose, or "" once its 5h window has reset
func (m *manager) holding(now time.Time) string {
	h, ok := readPin(m.p.holdFile())
	if !ok || !h.live(now) {
		return ""
	}
	return h.Name
}

// pinHold fixes the hold's end at the 5h reset the account just reported. It
// pins once because the header rolls forward to the next window after a reset
func (m *manager) pinHold(account string, snap usageSnapshot) {
	h, ok := readPin(m.p.holdFile())
	if !ok || h.Name != account || !h.Until.IsZero() {
		return
	}
	reset, ok := sessionReset(snap, snap.ObservedAt)
	if !ok {
		return
	}
	h.Until = reset
	if err := writePin(m.p.holdFile(), h); err != nil {
		log.Printf("hold %s: %v", account, err)
		return
	}
	log.Printf("hold: %s until %s", account, reset.Format(time.RFC3339))
}
