// Package pace spaces and retries outbound model calls.
//
// Motivating incident (2026-09-04, in aimem): a batch job fired dozens of
// requests back-to-back through a provider chain whose far end sits
// behind Cloudflare; the burst tripped its bot and rate rules and every
// batch died for the hour, while interactive agents on the same chain,
// one polite request at a time, never saw an error. The cure is to make
// batch traffic look like everyone else's: space the calls (Interval),
// retry transient blocks with backoff instead of abandoning the batch
// (Retries), and, when a rate-block IS detected, widen the spacing
// adaptively (the penalty doubles per block, is capped, and halves
// again on each clean success).
//
// A Pacer is meant to be shared by every caller in a process. With a
// state directory, the penalty and block counters persist across
// processes, so a new run inherits the spacing the previous one earned.
// A nil *Pacer is valid and means "unpaced, no retries".
package pace

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultInterval and DefaultRetries are the values aimem uses; the
	// zero Options mean no base spacing and no retries.
	DefaultInterval = 2 * time.Second
	DefaultRetries  = 3

	// StateFile is the file name of the persisted state in StateDir.
	StateFile = "llmrate.json"

	maxPenalty  = 2 * time.Minute
	penaltyStep = 5 * time.Second
)

// Clock is the pacer's time source; tests inject a fake one.
type Clock interface {
	Now() time.Time
	// Sleep waits d or until ctx ends, returning ctx's error then.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Options configure a Pacer. Every field is the caller's choice; the
// package reads no environment.
type Options struct {
	Interval time.Duration // base spacing between calls; <= 0 means none
	Retries  int           // retry attempts a blocked call gets; < 0 means 0
	StateDir string        // where StateFile lives; "" keeps state in memory
	Clock    Clock         // nil = the real clock
	// Logf receives the pacer's operator messages (a detected block, the
	// return to baseline, a scheduled retry). nil discards them.
	Logf func(format string, args ...any)
}

// Pacer is the shared call spacer. The zero value is not usable; use New.
type Pacer struct {
	interval time.Duration
	retries  int
	stateDir string
	clock    Clock
	logf     func(string, ...any)

	mu      sync.Mutex
	last    time.Time     // newest RESERVED slot (may be in the future)
	penGen  int           // bumped by Penalize; sleeping waiters re-queue on change
	penalty time.Duration // adaptive add-on to the base interval
	blocks  int           // rate-blocks seen (persisted, for stats)
	lastHit string        // RFC3339 of the newest block
}

// New returns a Pacer, loading any state persisted in opts.StateDir.
func New(opts Options) *Pacer {
	p := &Pacer{
		interval: max(opts.Interval, 0),
		retries:  max(opts.Retries, 0),
		stateDir: opts.StateDir,
		clock:    opts.Clock,
		logf:     opts.Logf,
	}
	if p.clock == nil {
		p.clock = realClock{}
	}
	if p.logf == nil {
		p.logf = func(string, ...any) {}
	}
	p.loadLocked()
	return p
}

type state struct {
	PenaltyMS int64  `json:"penalty_ms"`
	Blocks    int    `json:"blocks"`
	LastBlock string `json:"last_block,omitempty"`
	Updated   string `json:"updated"`
}

func (p *Pacer) loadLocked() {
	if p.stateDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(p.stateDir, StateFile))
	if err != nil {
		return
	}
	var s state
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	p.penalty, p.blocks, p.lastHit = time.Duration(s.PenaltyMS)*time.Millisecond, s.Blocks, s.LastBlock
}

// saveLocked persists best-effort: pacing state is advisory, and a full
// disk must not fail the call it paces.
func (p *Pacer) saveLocked() {
	if p.stateDir == "" {
		return
	}
	raw, _ := json.Marshal(state{PenaltyMS: p.penalty.Milliseconds(), Blocks: p.blocks,
		LastBlock: p.lastHit, Updated: p.clock.Now().UTC().Format(time.RFC3339)})
	_ = os.WriteFile(filepath.Join(p.stateDir, StateFile), raw, 0o600)
}

// Status is the pacer's state for stats surfaces.
type Status struct {
	IntervalS float64 `json:"interval_s"`
	PenaltyS  float64 `json:"penalty_s"`
	SpacingS  float64 `json:"spacing_s"`
	Blocks    int     `json:"blocks"`
	LastBlock string  `json:"last_block,omitempty"`
}

// Status reports the pacer. It re-reads the persisted state, so any
// process shows what the last model-calling process experienced.
func (p *Pacer) Status() Status {
	if p == nil {
		return Status{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked()
	return Status{
		IntervalS: p.interval.Seconds(),
		PenaltyS:  p.penalty.Seconds(),
		SpacingS:  (p.interval + p.penalty).Seconds(),
		Blocks:    p.blocks,
		LastBlock: p.lastHit,
	}
}

// Retries reports how many retry attempts a blocked call gets.
func (p *Pacer) Retries() int {
	if p == nil {
		return 0
	}
	return p.retries
}

// Wait blocks until the caller's next call slot: calls are spaced by
// the base interval plus the current adaptive penalty, and concurrent
// callers queue rather than stampede. It returns ctx's error if ctx
// ends first.
//
// Each caller RESERVES the next slot (last + gap) under the mutex, then
// sleeps UNLOCKED until its slot, so a sleeping waiter never blocks
// other users of the pacer. The reserved gap is a snapshot: if Penalize
// widens the spacing while a waiter sleeps, the waiter sees the
// generation change on wake and re-queues at the new gap, so an
// in-flight burst cannot keep hammering a blocked upstream at the old
// cadence. A Recover while sleeping leaves the old, wider slot in
// place; over-spacing is always safe.
func (p *Pacer) Wait(ctx context.Context) error {
	if p == nil {
		return ctx.Err()
	}
	for {
		p.mu.Lock()
		gap := p.interval + p.penalty
		now := p.clock.Now()
		if gap <= 0 {
			if p.last.Before(now) {
				p.last = now // advance only: never rewind past a live reservation
			}
			p.mu.Unlock()
			return ctx.Err()
		}
		gen := p.penGen
		slot := p.last.Add(gap)
		if slot.Before(now) {
			slot = now
		}
		p.last = slot
		p.mu.Unlock()
		if err := p.clock.Sleep(ctx, slot.Sub(now)); err != nil {
			return err
		}
		p.mu.Lock()
		requeue := p.penGen != gen
		p.mu.Unlock()
		if !requeue {
			return nil
		}
	}
}

// Sleep waits d on the pacer's clock, or until ctx ends. A nil pacer
// uses the real clock.
func (p *Pacer) Sleep(ctx context.Context, d time.Duration) error {
	if p == nil {
		return realClock{}.Sleep(ctx, d)
	}
	return p.clock.Sleep(ctx, d)
}

// Penalize widens the spacing after a detected block (doubles, from a
// 5s floor, capped at 2 minutes), persists, and says so through Logf:
// these lines are where outages get diagnosed.
func (p *Pacer) Penalize(reason string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.penalty < penaltyStep {
		p.penalty = penaltyStep
	} else {
		p.penalty *= 2
	}
	p.penalty = min(p.penalty, maxPenalty)
	p.blocks++
	p.penGen++ // wake-and-requeue signal for waiters sleeping on old slots
	p.lastHit = p.clock.Now().UTC().Format(time.RFC3339)
	p.saveLocked()
	p.logf("upstream rate-block detected (%s) — call spacing widened to %s (block #%d)",
		Clip(strings.TrimSpace(reason), 160), p.interval+p.penalty, p.blocks)
}

// Recover narrows the spacing again after a clean success, announcing
// only the return to baseline (per-success lines would be noise).
func (p *Pacer) Recover() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.penalty == 0 {
		return
	}
	p.penalty /= 2
	if p.penalty < time.Second {
		p.penalty = 0
		p.logf("upstream healthy again — call spacing back to base %s", p.interval)
	}
	p.saveLocked()
}

// Logf passes an operator message to the pacer's Logf.
func (p *Pacer) Logf(format string, args ...any) {
	if p != nil {
		p.logf(format, args...)
	}
}

// RetryDelay is the backoff before retry attempt n (0-based): about 15s,
// 30s, 60s and so on plus up to 5s of jitter, capped at 2 minutes.
func RetryDelay(attempt int) time.Duration {
	d := maxPenalty // 15s<<3 already reaches it; larger shifts would overflow
	if attempt < 3 {
		d = 15 * time.Second << max(attempt, 0)
	}
	return d + rand.N(5*time.Second)
}

// Blocked classifies a response as a transient upstream block worth
// retrying: a rate-limit or server-side status, an HTML body where JSON
// belongs (Cloudflare block pages), or a proxy error message carrying
// one (LiteLLM wraps the upstream's HTML inside its JSON error). It
// searches all of body, so pass it a body that is not a parsed answer:
// a model's reply may mention Cloudflare (package openai checks JSON
// bodies only through their error message).
func Blocked(status int, body string) bool {
	if status == 429 || status >= 500 {
		return true
	}
	return BlockedMessage(body)
}

// BlockedMessage detects a block page smuggled inside an error string.
func BlockedMessage(msg string) bool {
	t := strings.TrimSpace(msg)
	if strings.HasPrefix(t, "<") {
		return true
	}
	low := strings.ToLower(t)
	return strings.Contains(low, "<html") || strings.Contains(low, "cloudflare") ||
		strings.Contains(low, "attention required")
}

// Clip bounds provider error text for logs: a Cloudflare block page is
// about 8KB of HTML that once flooded a journal line by line.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}
