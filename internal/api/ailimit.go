package api

import (
	"sync"
	"time"
)

// AILimiter enforces a per-user 1-hour rolling window across all
// AI-backed endpoints (chat, palette, expense categorize, etc.).
//
// A user is throttled if EITHER cap is hit within the trailing hour:
//   - aiMaxMessages : protects against burst spam
//   - aiMaxTokens   : protects against expensive long-context calls
//
// Cheap calls (categorize ≈ 50 tok) cost almost nothing toward the
// token budget, so legit users hitting them at typing speed are fine.
// Heavy chat turns (≈ 1–3k tok each) are gated by the token cap before
// the message cap, which is the desired behaviour.
// Caps are bumped 4× from their original (60 / 100k) values. The
// switch to gemini-3.1-flash-lite makes each token meaningfully
// cheaper, so the budget can absorb the new ceiling without changing
// monthly spend.
const (
	aiWindow      = time.Hour
	aiMaxMessages = 240
	aiMaxTokens   = 400_000
)

type aiLimiter struct {
	mu  sync.Mutex
	log map[string][]aiEvent
}

type aiEvent struct {
	ts     time.Time
	tokens int
}

func newAILimiter() *aiLimiter {
	return &aiLimiter{log: map[string][]aiEvent{}}
}

// check reports whether the user has budget left in the current window.
// retryAfter is the time until the oldest in-window event ages out
// (0 when allowed).
func (l *aiLimiter) check(uid string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := l.prune(uid)
	tokens := 0
	for _, e := range events {
		tokens += e.tokens
	}
	if len(events) >= aiMaxMessages || tokens >= aiMaxTokens {
		oldest := events[0].ts
		return false, time.Until(oldest.Add(aiWindow))
	}
	return true, 0
}

// record adds usage AFTER a successful AI call. tokens may be the
// usage reported by the model, or a conservative estimate when usage
// metadata is unavailable.
func (l *aiLimiter) record(uid string, tokens int) {
	if tokens < 0 {
		tokens = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(uid)
	l.log[uid] = append(l.log[uid], aiEvent{ts: time.Now(), tokens: tokens})
}

// prune drops events older than the window. Caller must hold l.mu.
func (l *aiLimiter) prune(uid string) []aiEvent {
	cutoff := time.Now().Add(-aiWindow)
	events := l.log[uid]
	i := 0
	for i < len(events) && events[i].ts.Before(cutoff) {
		i++
	}
	if i > 0 {
		events = events[i:]
		l.log[uid] = events
	}
	return events
}
