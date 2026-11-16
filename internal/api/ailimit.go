package api

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/auth"
	"sajni/internal/db"
)

// aiLimiter holds each user to a spending budget across every Gemini call
// (chat, palette, capture parsing, categorising, themes…), measured from the
// model's own usage numbers (ai.Service.OnUsage), not guessed from text.
//
//   - Hourly, in memory: at most aiMaxCalls calls and aiHourlyUSD of spend in
//     the trailing hour (bursts, a runaway client).
//   - Daily, in Postgres (ai_spend): at most the daily budget per UTC day, so
//     it holds across restarts and instances. AI_DAILY_BUDGET_USD overrides
//     the default.
//
// A real chat question costs about $0.01–0.04, a capture parse about $0.001.
const (
	aiWindow          = time.Hour
	aiMaxCalls        = 240
	aiHourlyUSD       = 0.50
	aiDailyDefaultUSD = 2.00
)

type aiLimiter struct {
	db       *db.DB // nil: hourly limits only (tests)
	dailyUSD float64

	mu  sync.Mutex
	log map[string][]aiEvent
}

type aiEvent struct {
	ts   time.Time
	cost float64
}

// NewAILimiter builds the shared limiter; main wires Record into
// ai.Service.OnUsage.
func NewAILimiter(d *db.DB) *aiLimiter {
	daily := aiDailyDefaultUSD
	if v, err := strconv.ParseFloat(os.Getenv("AI_DAILY_BUDGET_USD"), 64); err == nil && v > 0 {
		daily = v
	}
	return &aiLimiter{db: d, dailyUSD: daily, log: map[string][]aiEvent{}}
}

func newAILimiter() *aiLimiter { return NewAILimiter(nil) }

// check reports whether the user may make another AI call now. retryAfter
// is how long until they may (0 when allowed).
func (l *aiLimiter) check(ctx context.Context, uid string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	events := l.prune(uid)
	var spent float64
	for _, e := range events {
		spent += e.cost
	}
	if len(events) >= aiMaxCalls || spent >= aiHourlyUSD {
		wait := time.Until(events[0].ts.Add(aiWindow))
		l.mu.Unlock()
		return false, wait
	}
	l.mu.Unlock()

	if l.db != nil {
		var micros int64
		err := l.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_micros),0) FROM ai_spend WHERE user_id=$1 AND day=(NOW() AT TIME ZONE 'UTC')::date`, uid).Scan(&micros)
		if err == nil && float64(micros)/1e6 >= l.dailyUSD {
			now := time.Now().UTC()
			midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			return false, midnight.Sub(now)
		}
	}
	return true, 0
}

// Record adds one model call's cost. uid may be empty; then it's the
// signed-in user on ctx, and calls with no user (background work) aren't
// budgeted.
func (l *aiLimiter) Record(ctx context.Context, uid, kind string, costUSD float64) {
	if uid == "" {
		uid, _ = auth.UserIDFromContext(ctx)
	}
	if uid == "" || costUSD <= 0 {
		return
	}
	l.mu.Lock()
	l.prune(uid)
	l.log[uid] = append(l.log[uid], aiEvent{ts: time.Now(), cost: costUSD})
	l.mu.Unlock()

	if l.db == nil {
		return
	}
	// The request may be ending; the spend must still land.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := l.db.ExecContext(dctx, `
		INSERT INTO ai_spend (user_id, day, cost_micros, calls) VALUES ($1, (NOW() AT TIME ZONE 'UTC')::date, $2, 1)
		ON CONFLICT (user_id, day) DO UPDATE SET cost_micros = ai_spend.cost_micros + EXCLUDED.cost_micros, calls = ai_spend.calls + 1`,
		uid, int64(costUSD*1e6+0.5)); err != nil {
		log.Warn().Err(err).Str("kind", kind).Msg("ai spend record")
	}
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
	if len(events) == 0 {
		delete(l.log, uid)
	}
	return events
}

// aiLimitMessage is the 429 text: when they can try again, in plain words.
func aiLimitMessage(retryAfter time.Duration) string {
	if retryAfter > 2*time.Hour {
		return "You've reached today's AI limit. Try again tomorrow."
	}
	mins := int(retryAfter.Minutes()) + 1
	return "You've reached the AI limit for now. Try again in " + strconv.Itoa(mins) + " min."
}
