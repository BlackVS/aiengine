package pace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock advances only when slept on; onSleep, when set, runs before
// each nonzero sleep (a test's hook into "while the waiter sleeps").
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func()
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	hook := c.onSleep
	c.mu.Unlock()
	if d > 0 && hook != nil {
		hook()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	if d > 0 {
		c.now = c.now.Add(d)
	}
	return nil
}

func TestBlockedClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, `{"data":[]}`, false},
		{429, `{}`, true},
		{503, `whatever`, true},
		{200, `<!DOCTYPE html><html>blocked</html>`, true},
		{400, `{"error":{"message":"bad request"}}`, false},
	}
	for _, c := range cases {
		if got := Blocked(c.status, c.body); got != c.want {
			t.Errorf("Blocked(%d, %.20q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
	if !BlockedMessage("upstream said: <html><title>Attention Required! | Cloudflare</title>...") {
		t.Error("HTML smuggled in an error message must classify as a block")
	}
	if BlockedMessage("connection refused") {
		t.Error("ordinary errors are not blocks")
	}
}

func TestPenaltyAdaptsAndPersists(t *testing.T) {
	dir := t.TempDir()
	var logged []string
	p := New(Options{Interval: 50 * time.Millisecond, StateDir: dir, Clock: newFakeClock(),
		Logf: func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }})

	p.Penalize("test block")
	p.Penalize("test block")
	p.mu.Lock()
	pen, b := p.penalty, p.blocks
	p.mu.Unlock()
	if pen != 10*time.Second || b != 2 {
		t.Fatalf("after two blocks: penalty=%v blocks=%d; want 10s/2", pen, b)
	}
	if len(logged) != 2 || !strings.Contains(logged[1], "block #2") {
		t.Fatalf("block messages: %q", logged)
	}

	// A fresh "process" (a new Pacer on the same directory) inherits the penalty.
	st := New(Options{Interval: 50 * time.Millisecond, StateDir: dir}).Status()
	if st.PenaltyS != 10 || st.Blocks != 2 || st.LastBlock != "2026-10-24T12:00:00Z" {
		t.Fatalf("persisted state not reloaded: %+v", st)
	}

	// Successes decay it back to zero.
	for range 8 {
		p.Recover()
	}
	if s := p.Status(); s.PenaltyS != 0 {
		t.Fatalf("penalty should decay to 0, got %+v", s)
	}
	if !strings.Contains(logged[len(logged)-1], "healthy again") {
		t.Fatalf("return to baseline not announced: %q", logged)
	}
	raw, err := os.ReadFile(filepath.Join(dir, StateFile))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil || s.PenaltyMS != 0 || s.Blocks != 2 {
		t.Fatalf("state file: %s err=%v", raw, err)
	}
}

func TestPenaltyCapped(t *testing.T) {
	p := New(Options{})
	for range 10 {
		p.Penalize("x")
	}
	if s := p.Status(); s.PenaltyS != maxPenalty.Seconds() {
		t.Fatalf("penalty not capped: %+v", s)
	}
}

func TestClip(t *testing.T) {
	if got := Clip(strings.Repeat("x", 500), 100); len(got) > 120 || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("clip: %q", got)
	}
}

func TestRetryDelay(t *testing.T) {
	for attempt, base := range []time.Duration{15 * time.Second, 30 * time.Second, 60 * time.Second, maxPenalty, maxPenalty, maxPenalty} {
		if attempt == 5 {
			attempt = 70 // a shift this wide would overflow
		}
		d := RetryDelay(attempt)
		if d < base || d >= base+5*time.Second {
			t.Errorf("RetryDelay(%d) = %s, want [%s, %s)", attempt, d, base, base+5*time.Second)
		}
	}
}

// The nil Pacer is the documented "unpaced" value: every method works.
func TestNilPacer(t *testing.T) {
	var p *Pacer
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.Penalize("x")
	p.Recover()
	p.Logf("x")
	if p.Retries() != 0 || p.Status() != (Status{}) {
		t.Fatal("nil pacer must report nothing")
	}
}

// With an injected clock the spacing is exact: the first call fires at
// once, each later one waits the full gap after the previous slot.
func TestWaitSpacingInjectedClock(t *testing.T) {
	clk := newFakeClock()
	start := clk.Now()
	p := New(Options{Interval: 2 * time.Second, Clock: clk})
	for range 3 {
		if err := p.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := clk.Now().Sub(start); got != 4*time.Second {
		t.Fatalf("three calls at a 2s gap took %s, want 4s", got)
	}
	if !slices.Equal(clk.sleeps, []time.Duration{0, 2 * time.Second, 2 * time.Second}) {
		t.Fatalf("sleeps: %v", clk.sleeps)
	}
}

// Penalize while a waiter sleeps re-queues it at the widened gap (the
// injected-clock twin of TestPenalizeWidensQueuedWaiters).
func TestWaitRequeuesAfterPenalizeInjectedClock(t *testing.T) {
	clk := newFakeClock()
	p := New(Options{Interval: 100 * time.Millisecond, Clock: clk})
	if err := p.Wait(context.Background()); err != nil { // first slot: now
		t.Fatal(err)
	}
	var once sync.Once
	clk.onSleep = func() { once.Do(func() { p.Penalize("test block") }) }
	start := clk.Now()
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := clk.Now().Sub(start); got < penaltyStep {
		t.Fatalf("queued waiter returned after %s: the old-cadence slot survived a Penalize", got)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	p := New(Options{Interval: time.Hour})
	if err := p.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	begin := time.Now()
	if err := p.Wait(ctx); err == nil {
		t.Fatal("Wait must return the context's error")
	}
	if e := time.Since(begin); e > 5*time.Second {
		t.Fatalf("Wait ignored its context for %s", e)
	}
}

// TestWaitDoesNotHoldMutexWhileSleeping: a sleeping waiter must not
// block other users of the pacer (Status, Penalize, another reservation).
func TestWaitDoesNotHoldMutexWhileSleeping(t *testing.T) {
	p := New(Options{Interval: 400 * time.Millisecond})
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { p.Wait(context.Background()) }) // second one sleeps ~400ms
	}
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	p.Status() // locks the mutex internally
	if e := time.Since(start); e > 150*time.Millisecond {
		t.Fatalf("Status blocked %s behind a sleeping Wait", e)
	}
	wg.Wait()
}

// TestWaitSpacingConcurrent: N concurrent callers return spaced by at
// least the gap, in some order: the pacing contract on the real clock.
func TestWaitSpacingConcurrent(t *testing.T) {
	const gap = 120 * time.Millisecond
	p := New(Options{Interval: gap})
	done := make(chan time.Time, 3)
	for range 3 {
		go func() { p.Wait(context.Background()); done <- time.Now() }()
	}
	var times []time.Time
	for range 3 {
		times = append(times, <-done)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < gap-30*time.Millisecond {
			t.Fatalf("returns %d and %d only %s apart (gap %s)", i-1, i, d, gap)
		}
	}
}

// TestPenalizeWidensQueuedWaiters: a Penalize while waiters sleep must
// widen THEIR remaining spacing, not just future reservations, or an
// in-flight burst keeps hammering a blocked upstream at the old cadence.
func TestPenalizeWidensQueuedWaiters(t *testing.T) {
	p := New(Options{Interval: 100 * time.Millisecond})
	start := time.Now()
	done := make(chan time.Duration, 2)
	for range 2 {
		go func() { p.Wait(context.Background()); done <- time.Since(start) }()
	}
	time.Sleep(30 * time.Millisecond) // let them reserve their slots
	p.Penalize("test block")          // penalty jumps to the 5s floor
	var returns []time.Duration
	for range 2 {
		returns = append(returns, <-done)
	}
	slices.Sort(returns)
	if returns[1] < 1*time.Second {
		t.Fatalf("queued waiter returned at %s: old-cadence slot survived a Penalize", returns[1])
	}
}
