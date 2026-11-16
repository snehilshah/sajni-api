package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

func TestAILimiterHourlySpend(t *testing.T) {
	l := newAILimiter()
	ctx := context.Background()
	if ok, _ := l.check(ctx, "u1"); !ok {
		t.Fatal("fresh user refused")
	}
	l.Record(ctx, "u1", "chat", 0.30)
	if ok, _ := l.check(ctx, "u1"); !ok {
		t.Fatal("refused under the hourly budget")
	}
	l.Record(ctx, "u1", "chat", 0.25)
	ok, wait := l.check(ctx, "u1")
	if ok || wait <= 0 || wait > time.Hour {
		t.Fatalf("over the hourly budget: ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.check(ctx, "u2"); !ok {
		t.Fatal("another user refused")
	}
	// No uid: taken from the signed-in user on ctx; none there → not counted.
	l.Record(context.WithValue(ctx, auth.ContextKey{}, "u3"), "", "chat", 0.6)
	if ok, _ := l.check(ctx, "u3"); ok {
		t.Fatal("spend without uid not attributed to the ctx user")
	}
	l.Record(ctx, "", "categorize", 5)
	// Chat spending doesn't hold up capture parsing; the gate says why.
	if ok, _ := l.checkCheap(ctx, "u1"); !ok {
		t.Fatal("capture parse refused by chat spending")
	}
	var limit *ai.LimitError
	if err := l.Gate(ctx, "u1", "palette"); !errors.As(err, &limit) || limit.RetryAfter <= 0 {
		t.Fatalf("gate let a palette question through: %v", err)
	}
	if err := l.Gate(ctx, "u1", "parse_text"); err != nil {
		t.Fatalf("gate refused a parse: %v", err)
	}
	// Failed calls (no cost) still count toward the call cap.
	for range aiMaxCalls {
		l.Record(ctx, "u4", "parse_text", 0)
	}
	if ok, _ := l.checkCheap(ctx, "u4"); ok {
		t.Fatal("call cap ignores failed calls")
	}
	if !strings.Contains(aiLimitMessage(5*time.Hour+30*time.Minute), "6 h") {
		t.Fatal(aiLimitMessage(5*time.Hour + 30*time.Minute))
	}
	if !strings.Contains(aiLimitMessage(10*time.Minute), "11 min") {
		t.Fatal(aiLimitMessage(10 * time.Minute))
	}
}

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api
func TestAILimiterDailyBudget(t *testing.T) {
	dsn := dbtest.URL(t)
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000f8"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'ailimit@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	ctx := context.Background()
	l := NewAILimiter(d)
	l.dailyUSD = 0.10
	l.Record(ctx, uid, "parse_text", 0.04)
	l.Record(ctx, uid, "parse_text", 0.04)
	if ok, _ := l.check(ctx, uid); !ok {
		t.Fatal("refused under the daily budget")
	}
	// A restart forgets the hour but not the day.
	l2 := NewAILimiter(d)
	l2.dailyUSD = 0.10
	l2.Record(ctx, uid, "chat", 0.03)
	if ok, wait := l2.check(ctx, uid); ok || wait <= 0 {
		t.Fatalf("over the daily budget: ok=%v wait=%v", ok, wait)
	}
	var calls int
	d.QueryRow(`SELECT calls FROM ai_spend WHERE user_id=$1`, uid).Scan(&calls)
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	// Everyone together: a ceiling for the whole service.
	l3 := NewAILimiter(d)
	l3.globalUSD = 0.10
	if ok, _ := l3.checkCheap(ctx, "01900000-0000-7000-8000-0000000000f9"); ok {
		t.Fatal("global daily ceiling not applied to another user")
	}
}
