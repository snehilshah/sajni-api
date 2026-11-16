package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
)

// aiLimiter holds each user to a spending budget across every Gemini call
// (chat, palette, capture parsing, categorising, themes…), measured from the
// model's own usage numbers (ai.Service.OnUsage), not guessed from text.
//
//   - Hourly, in memory: at most aiMaxCalls calls and aiHourlyUSD of spend in
//     the trailing hour (bursts, a runaway client).
//     The dollar limit counts only chat and palette questions, so a long chat
//     can't hold up bank-message capture; capture is bounded by the call cap.
//   - Daily, in Postgres (ai_spend): at most the daily budget per UTC day, so
//     it holds across restarts and instances; and a ceiling on everyone
//     together, since sign-up is open. AI_DAILY_BUDGET_USD and
//     AI_GLOBAL_DAILY_BUDGET_USD override the defaults.
//
// Every model call passes through ai.Service.Gate (Gate below), so themes,
// insight narration and Projects enrichment are held to the same budget.
// A real chat question costs about $0.01–0.04, a capture parse about $0.001.
const (
	aiWindow           = time.Hour
	aiMaxCalls         = 240
	aiHourlyUSD        = 0.50
	aiDailyDefaultUSD  = 2.00
	aiGlobalDefaultUSD = 6.00
)

type aiLimiter struct {
	db        *db.DB // nil: hourly limits only (tests)
	dailyUSD  float64
	globalUSD float64

	mu  sync.Mutex
	log map[string][]aiEvent
}

type aiEvent struct {
	ts    time.Time
	cost  float64
	agent bool // a chat or palette question
}

// agentKind reports whether a call kind is a chat or palette question.
func agentKind(kind string) bool { return kind == "chat" || kind == "palette" }

// NewAILimiter builds the shared limiter; main wires Record into
// ai.Service.OnUsage.
func NewAILimiter(d *db.DB) *aiLimiter {
	daily := aiDailyDefaultUSD
	if v, err := strconv.ParseFloat(os.Getenv("AI_DAILY_BUDGET_USD"), 64); err == nil && v > 0 {
		daily = v
	}
	global := aiGlobalDefaultUSD
	if v, err := strconv.ParseFloat(os.Getenv("AI_GLOBAL_DAILY_BUDGET_USD"), 64); err == nil && v > 0 {
		global = v
	}
	return &aiLimiter{db: d, dailyUSD: daily, globalUSD: global, log: map[string][]aiEvent{}}
}

func newAILimiter() *aiLimiter { return NewAILimiter(nil) }

// check reports whether the user may ask a chat or palette question now.
// retryAfter is how long until they may (0 when allowed).
func (l *aiLimiter) check(ctx context.Context, uid string) (allowed bool, retryAfter time.Duration) {
	return l.allow(ctx, uid, true)
}

// checkCheap is check for parsing and categorising: the call cap and the
// daily budgets, not the hourly dollars.
func (l *aiLimiter) checkCheap(ctx context.Context, uid string) (allowed bool, retryAfter time.Duration) {
	return l.allow(ctx, uid, false)
}

func (l *aiLimiter) allow(ctx context.Context, uid string, agent bool) (bool, time.Duration) {
	if uid != "" {
		l.mu.Lock()
		events := l.prune(uid)
		var spent float64
		for _, e := range events {
			if e.agent {
				spent += e.cost
			}
		}
		if len(events) >= aiMaxCalls || (agent && spent >= aiHourlyUSD) {
			wait := time.Until(events[0].ts.Add(aiWindow))
			if agent && spent >= aiHourlyUSD {
				// Wait for the oldest question to leave the window.
				for _, e := range events {
					if e.agent {
						wait = time.Until(e.ts.Add(aiWindow))
						break
					}
				}
			}
			l.mu.Unlock()
			return false, wait
		}
		l.mu.Unlock()
	}

	if l.db != nil {
		var mine, all int64
		err := l.db.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(cost_micros) FILTER (WHERE user_id::text=$1),0), COALESCE(SUM(cost_micros),0)
			FROM ai_spend WHERE day=(NOW() AT TIME ZONE 'UTC')::date`, uid).Scan(&mine, &all)
		if err == nil && (float64(mine)/1e6 >= l.dailyUSD || float64(all)/1e6 >= l.globalUSD) {
			now := time.Now().UTC()
			midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
			return false, midnight.Sub(now)
		}
	}
	return true, 0
}

// Gate is ai.Service.Gate: every model call asks it first.
func (l *aiLimiter) Gate(ctx context.Context, uid, kind string) error {
	if uid == "" {
		uid, _ = auth.UserIDFromContext(ctx)
	}
	if ok, wait := l.allow(ctx, uid, agentKind(kind)); !ok {
		return &ai.LimitError{RetryAfter: wait, Message: aiLimitMessage(wait)}
	}
	return nil
}

// Record adds one model call and its cost (0 for a failed call, which still
// counts toward the call cap). uid may be empty; then it's the signed-in
// user on ctx, and calls with no user at all aren't budgeted.
func (l *aiLimiter) Record(ctx context.Context, uid, kind string, costUSD float64) {
	if uid == "" {
		uid, _ = auth.UserIDFromContext(ctx)
	}
	if uid == "" {
		return
	}
	l.mu.Lock()
	l.prune(uid)
	l.log[uid] = append(l.log[uid], aiEvent{ts: time.Now(), cost: costUSD, agent: agentKind(kind)})
	l.mu.Unlock()

	if l.db == nil || costUSD <= 0 {
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
		return "You've reached today's AI limit. It resets in " + strconv.Itoa(int(retryAfter.Hours())+1) + " h."
	}
	mins := int(retryAfter.Minutes()) + 1
	return "You've reached the AI limit for now. Try again in " + strconv.Itoa(mins) + " min."
}

// aiFailed answers a failed AI call: 429 with Retry-After when the budget
// refused it, else 500.
func aiFailed(w http.ResponseWriter, err error) {
	var limit *ai.LimitError
	if errors.As(err, &limit) {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(limit.RetryAfter.Seconds()))))
		errJSON(w, http.StatusTooManyRequests, limit.Message)
		return
	}
	errJSON(w, http.StatusInternalServerError, err.Error())
}
