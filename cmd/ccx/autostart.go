package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// primeWindow is the length of the subscription usage window autostart keeps
// alive on each account. Anthropic resets the unified limit five hours after a
// window's first request.
const primeWindow = 5 * time.Hour

// autostartRetryGap bounds how often a single account is primed, so an account
// whose prime keeps failing is retried on a slow cadence instead of every tick.
const autostartRetryGap = 10 * time.Minute

// primeBody is a minimal Claude Code request. Subscription OAuth rejects a
// request whose first system block is not the Claude Code identifier, so the
// prime carries it verbatim and asks for a one-word reply.
const primeBody = `{"model":"claude-haiku-4-5","max_tokens":5,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}],"messages":[{"role":"user","content":"emit hi and stop"}]}`

// runAutostart keeps every account's usage window running and staggered. The
// active account anchors the schedule at its real window start, and each other
// account is offset by window/N so their resets spread evenly across the window.
func (m *manager) runAutostart(upstream *url.URL, beta string) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	m.autostartTick(upstream, beta)
	for range tick.C {
		m.autostartTick(upstream, beta)
	}
}

func (m *manager) autostartTick(upstream *url.URL, beta string) {
	profs, err := m.p.listProfiles()
	if err != nil || len(profs) == 0 {
		return
	}
	now := time.Now()
	m.followSoonest(now)
	order := m.autostartOrder(profs, now)
	offset := primeWindow / time.Duration(len(order))

	ref := m.referenceStart(order[0], now)
	pos := now.Sub(ref) % primeWindow
	for i, name := range order {
		phase := time.Duration(i) * offset
		if pos < phase || pos >= phase+offset {
			continue
		}
		if m.windowRunning(name, now) {
			continue
		}
		if !m.shouldPrime(name, now) {
			continue
		}
		if err := m.prime(upstream, beta, name); err != nil {
			log.Printf("prime %s: %v", name, err)
		}
	}
}

// referenceStart is the instant the schedule offsets from: the active account's
// current window start when one is running, otherwise now so the active account
// primes immediately and the rest fall in behind it.
func (m *manager) referenceStart(active string, now time.Time) time.Time {
	if reset, ok := m.windowReset(active, now); ok && reset.After(now) {
		return reset.Add(-primeWindow)
	}
	return now
}

// windowRunning reports whether the account's usage window is still open, read
// from the reset header its last response carried.
func (m *manager) windowRunning(name string, now time.Time) bool {
	reset, ok := m.windowReset(name, now)
	return ok && reset.After(now)
}

func (m *manager) windowReset(name string, now time.Time) (time.Time, bool) {
	snap, ok := m.usageSnapshot(name)
	if !ok {
		return time.Time{}, false
	}
	return parseReset(snap.Headers["anthropic-ratelimit-unified-5h-reset"], now)
}

// pickSoonest returns the account whose open 5h window resets soonest and is
// not already at its limit, so the proxy spends the budget nearest to expiring
// before it is lost. It returns "" when no open window qualifies.
func (m *manager) pickSoonest(now time.Time) string {
	profs, err := m.p.listProfiles()
	if err != nil {
		return ""
	}
	best := ""
	var bestReset time.Time
	for _, prof := range profs {
		snap, ok := m.usageSnapshot(prof.Name)
		if !ok {
			continue
		}
		if snap.Headers["anthropic-ratelimit-unified-5h-status"] == "rejected" {
			continue
		}
		reset, ok := parseReset(snap.Headers["anthropic-ratelimit-unified-5h-reset"], now)
		if !ok || !reset.After(now) {
			continue
		}
		if best == "" || reset.Before(bestReset) {
			best, bestReset = prof.Name, reset
		}
	}
	return best
}

// followSoonest keeps the selection on the open window nearest to reset, unless
// ccx use holds an account. It must run before autostart primes, or a closed
// selection would get a fresh window and stay selected while another account's
// budget expires unused
func (m *manager) followSoonest(now time.Time) {
	name := m.holding(now)
	if name == "" {
		name = m.pickSoonest(now)
	}
	if name == "" {
		return
	}
	active := m.activeName()
	if name == active {
		return
	}
	if err := m.switchTo(name); err != nil {
		log.Printf("follow soonest %s: %v", name, err)
		return
	}
	log.Printf("selected %s over %s", name, active)
}

// shouldPrime records an attempt and reports whether the retry gap has elapsed,
// so a failing prime does not repeat every tick.
func (m *manager) shouldPrime(name string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if last, ok := m.primeAttempt[name]; ok && now.Sub(last) < autostartRetryGap {
		return false
	}
	m.primeAttempt[name] = now
	return true
}

// prime sends one minimal request on the account to open its usage window,
// recording the rate-limit headers it comes back with.
func (m *manager) prime(upstream *url.URL, beta, name string) error {
	token, err := m.tokenFor(name)
	if err != nil {
		return err
	}
	body := []byte(primeBody)
	req, err := http.NewRequest(http.MethodPost, upstream.String()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultTransport.RoundTrip(buildUpstream(req, body, upstream, token, beta))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	m.recordUsage(name, requestModel(body), false, resp.Header)
	if resp.StatusCode == http.StatusTooManyRequests {
		m.markLimited(name, resetAfter(resp))
		return nil
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}
	return nil
}

// nextPrime is the instant autostart will next consider priming the account,
// the start of its stagger slot within the current window cycle. It reports
// false when autostart is off, so callers can tell "not scheduled" from "due".
func (m *manager) nextPrime(name string, now time.Time) (time.Time, bool) {
	if !m.autostart {
		return time.Time{}, false
	}
	profs, err := m.p.listProfiles()
	if err != nil || len(profs) == 0 {
		return time.Time{}, false
	}
	order := m.autostartOrder(profs, now)
	idx := indexOf(order, name)
	if idx < 0 {
		return time.Time{}, false
	}
	// An open window is never reprimed, so the soonest a prime can land is when
	// it closes; the slot time alone would show a prime the tick would skip
	if reset, ok := m.windowReset(name, now); ok && reset.After(now) {
		return reset, true
	}
	offset := primeWindow / time.Duration(len(order))
	ref := m.referenceStart(order[0], now)
	pos := now.Sub(ref) % primeWindow
	slot := time.Duration(idx) * offset

	switch {
	case pos < slot:
		return now.Add(slot - pos), true
	case pos < slot+offset:
		return now, true
	default:
		return now.Add(primeWindow - pos + slot), true
	}
}

func indexOf(names []string, name string) int {
	for i, n := range names {
		if n == name {
			return i
		}
	}
	return -1
}

// autostartOrder puts the active account first, then open windows by reset,
// then closed ones by name. The active account moves to whichever resets
// soonest, so slots must follow reset order: name order would hand an account
// that just closed a slot already past and leave it idle for most of a window
func (m *manager) autostartOrder(profs []Profile, now time.Time) []string {
	active := m.activeName()
	names := make([]string, 0, len(profs))
	if active != "" {
		names = append(names, active)
	}
	rest := len(names)
	resets := map[string]time.Time{}
	for _, prof := range profs {
		if prof.Name == active {
			continue
		}
		names = append(names, prof.Name)
		if reset, ok := m.windowReset(prof.Name, now); ok && reset.After(now) {
			resets[prof.Name] = reset
		}
	}
	slices.SortStableFunc(names[rest:], func(a, b string) int {
		ra, aOpen := resets[a]
		rb, bOpen := resets[b]
		if aOpen != bOpen {
			if aOpen {
				return -1
			}
			return 1
		}
		return ra.Compare(rb)
	})
	return names
}
