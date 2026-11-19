package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

func TestAdvanceDueKeepsTheDay(t *testing.T) {
	day := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	for _, c := range []struct {
		from, freq string
		anchor     int
		want       string
	}{
		{"2027-01-31", "monthly", 31, "2027-02-28"},
		{"2027-02-28", "monthly", 31, "2027-03-31"},
		{"2026-11-30", "quarterly", 30, "2027-02-28"},
		{"2026-12-05", "yearly", 5, "2027-12-05"},
		{"2026-12-05", "fortnightly", 5, "2026-12-19"},
	} {
		if got := db.AdvanceDue(day(c.from), c.freq, c.anchor).Format("2006-01-02"); got != c.want {
			t.Errorf("%s %s → %s, want %s", c.from, c.freq, got, c.want)
		}
	}
	// A payment's cycle: Home Contrib due 5 Dec, paid 6 Jan → the 5 Jan cycle.
	if got := db.RecurringCycleOn(day("2026-12-05"), "monthly", 5, day("2027-01-06")).Format("2006-01-02"); got != "2027-01-05" {
		t.Errorf("cycle on 6 Jan = %s", got)
	}
}

// Recurring items wait for their payment: Home Contrib and Salary from the
// owner's December, a double tap, Papa's ₹2,724 payback to the same payee,
// an auto-posted cycle giving way to the real one, and the 8 pm nudge.
func TestRecurringItemsTickThemselves(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000e4"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name, timezone) VALUES ($1, 'recurring@test', 'T', 'Asia/Kolkata')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var salary, homeCat, salaryCat int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,'Salary','salary','2851, kotak') RETURNING id`, uid).Scan(&salary)
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Home Contrib','expense') RETURNING id`, uid).Scan(&homeCat)
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Salary','income') RETURNING id`, uid).Scan(&salaryCat)

	canned := map[string]ai.ParsedTxn{}
	captureParserSwapped = true
	old := captureParser
	captureParser = func(_ context.Context, _ Deps, _, text, _ string) (ai.ParsedTxn, int, error) {
		return canned[text], 1, nil
	}
	defer func() { captureParser, captureParserSwapped = old, false }()
	deps := Deps{DB: d, AILimiter: newAILimiter()}
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, deps)
	registerBillerRoutes(mux, deps)
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return out
	}
	due := func(id int64) (s string) {
		d.QueryRow(`SELECT next_due_date::text FROM fin_billers WHERE id = $1`, id).Scan(&s)
		return
	}
	row := func(id int64) (title string, cat int64) {
		d.QueryRow(`SELECT description, COALESCE(category_id,0) FROM fin_transactions WHERE id = $1`, id).Scan(&title, &cat)
		return
	}

	home := int64(call("POST", "/api/finance/billers", map[string]any{"name": "Home Contrib", "amount": 22000, "next_due_date": "2026-12-05", "account_id": salary, "category_id": homeCat})["id"].(float64))
	sal := int64(call("POST", "/api/finance/billers", map[string]any{"name": "Salary", "kind": "bill", "income": true, "amount": 67772, "next_due_date": "2026-12-05", "account_id": salary, "category_id": salaryCat})["id"].(float64))

	// 6 Dec 01:26: the Kotak SMS for ₹22,000 to Papa ticks Home Contrib and
	// takes its name and category.
	sms := "Sent Rs.22,000.00 from Kotak Bank A/c X2851 to RONAK ARVIND SHAH on 06-12-26. UPI Ref 634000000001."
	canned[sms] = ai.ParsedTxn{Amount: 22000, Type: "expense", Description: "Ronak Arvind Shah", Date: "2026-12-06", Time: "01:26", AccountHint: "2851", RefID: "634000000001"}
	var c captureResp
	b, _ := json.Marshal(call("POST", "/api/finance/captures", map[string]any{"text": sms, "source": "sms", "received_at": "2026-12-06T01:26:30+05:30"}))
	json.Unmarshal(b, &c)
	if c.Outcome != "created" || c.Recurring != "Home Contrib" {
		t.Fatalf("Home Contrib by SMS: %+v", c)
	}
	if title, cat := row(c.TxnID); title != "Home Contrib" || cat != homeCat {
		t.Fatalf("captured row: %q in %d", title, cat)
	}
	if got := due(home); got != "2027-01-05" {
		t.Fatalf("Home Contrib next due %s", got)
	}

	// Papa's ₹2,724 payback to the same payee is not Home Contrib.
	pb := "Sent Rs.2,724.00 from Kotak Bank A/c X2851 to RONAK ARVIND SHAH on 13-12-26. UPI Ref 634000000002."
	canned[pb] = ai.ParsedTxn{Amount: 2724, Type: "expense", Description: "Ronak Arvind Shah", Date: "2026-12-13", Time: "10:00", AccountHint: "2851", RefID: "634000000002"}
	b, _ = json.Marshal(call("POST", "/api/finance/captures", map[string]any{"text": pb, "source": "sms", "received_at": "2026-12-13T10:00:30+05:30"}))
	c = captureResp{}
	json.Unmarshal(b, &c)
	if c.Recurring != "" {
		t.Fatalf("the payback ticked %s", c.Recurring)
	}

	// Two taps on ✓ for January pay January once.
	first := call("POST", fmt.Sprintf("/api/finance/billers/%d/pay", home), map[string]any{"due_date": "2027-01-05", "paid_date": "2027-01-05"})
	second := call("POST", fmt.Sprintf("/api/finance/billers/%d/pay", home), map[string]any{"due_date": "2027-01-05", "paid_date": "2027-01-05"})
	if first["already_paid"] != false || second["already_paid"] != true || due(home) != "2027-02-05" {
		t.Fatalf("double tap: %v %v, next %s", first, second, due(home))
	}
	// An older client sending no due date: the second tap within seconds too.
	d.Exec(`UPDATE fin_biller_payments SET created_at = NOW() - INTERVAL '1 minute' WHERE biller_id = $1`, home)
	call("POST", fmt.Sprintf("/api/finance/billers/%d/pay", home), map[string]any{"paid_date": "2027-02-05"})
	if again := call("POST", fmt.Sprintf("/api/finance/billers/%d/pay", home), map[string]any{"paid_date": "2027-02-05"}); again["already_paid"] != true || due(home) != "2027-03-05" {
		t.Fatalf("old client double tap: %v, next %s", again, due(home))
	}

	// Typed on 6 Mar with the owner's own title: it ticks, the title stays.
	made := call("POST", "/api/finance/transactions", map[string]any{"account_id": salary, "type": "expense", "amount": 22000, "description": "Home Contrib March", "category_id": homeCat, "txn_at": "2027-03-06T15:30:00+05:30"})
	if made["recurring"] != "Home Contrib" || due(home) != "2027-04-05" {
		t.Fatalf("typed: %v, next %s", made, due(home))
	}
	if title, _ := row(int64(made["id"].(float64))); title != "Home Contrib March" {
		t.Fatalf("typed title became %q", title)
	}

	// Salary varies: ₹70,000 on 5 Dec ticks it (and gets its category);
	// ₹50 of interest on the same day doesn't.
	small := call("POST", "/api/finance/transactions", map[string]any{"account_id": salary, "type": "income", "amount": 50, "description": "Interest", "txn_at": "2026-12-05T09:00:00+05:30"})
	pay := call("POST", "/api/finance/transactions", map[string]any{"account_id": salary, "type": "income", "amount": 70000, "description": "Salary November", "txn_at": "2026-12-05T11:00:00+05:30"})
	if small["recurring"] != nil || pay["recurring"] != "Salary" || due(sal) != "2027-01-05" {
		t.Fatalf("salary: %v %v, next %s", small, pay, due(sal))
	}
	if _, cat := row(int64(pay["id"].(float64))); cat != salaryCat {
		t.Fatalf("salary row category %d, want %d", cat, salaryCat)
	}

	// Posted by itself: created today with a due date 40 days back, nothing
	// is back-posted. Due today, the cron posts it; the real payment then
	// takes its place.
	ist, _ := time.LoadLocation("Asia/Kolkata")
	today := time.Now().In(ist).Format("2006-01-02")
	past := time.Now().In(ist).AddDate(0, 0, -40).Format("2006-01-02")
	back := int64(call("POST", "/api/finance/billers", map[string]any{"name": "Gym", "amount": 1500, "next_due_date": past, "account_id": salary, "auto_renew": true})["id"].(float64))
	auto := int64(call("POST", "/api/finance/billers", map[string]any{"name": "Netflix", "amount": 649, "next_due_date": today, "account_id": salary, "auto_renew": true})["id"].(float64))
	if _, _, err := ProcessBillerCron(context.Background(), deps); err != nil {
		t.Fatal(err)
	}
	var backPaid, autoPaid int
	d.QueryRow(`SELECT COUNT(*) FROM fin_biller_payments WHERE biller_id = $1`, back).Scan(&backPaid)
	d.QueryRow(`SELECT COUNT(*) FROM fin_biller_payments WHERE biller_id = $1 AND auto`, auto).Scan(&autoPaid)
	if backPaid != 0 || due(back) <= today || autoPaid != 1 {
		t.Fatalf("auto-post: back-posted %d (next %s), today's %d", backPaid, due(back), autoPaid)
	}
	var netflixRows int
	d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND amount = 649`, uid).Scan(&netflixRows)
	// (Typed, the sheet first asks "possible duplicate?"; Add anyway sends force.)
	real := call("POST", "/api/finance/transactions?force=true", map[string]any{"account_id": salary, "type": "expense", "amount": 649, "description": "Netflix", "txn_at": time.Now().In(ist).Format(time.RFC3339)})
	var after int
	var stillAuto bool
	d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND amount = 649`, uid).Scan(&after)
	d.QueryRow(`SELECT auto FROM fin_biller_payments WHERE biller_id = $1`, auto).Scan(&stillAuto)
	if netflixRows != 1 || after != 1 || real["recurring"] != "Netflix" || stillAuto {
		t.Fatalf("real payment over the posted one: before %d, after %d, %v, auto %v", netflixRows, after, real, stillAuto)
	}

	// Nothing recorded for yesterday's due date: one nudge, once.
	yesterday := time.Now().In(ist).AddDate(0, 0, -1).Format("2006-01-02")
	late := int64(call("POST", "/api/finance/billers", map[string]any{"name": "Claude", "amount": 1990, "next_due_date": yesterday, "account_id": salary})["id"].(float64))
	ProcessBillerCron(context.Background(), deps)
	ProcessBillerCron(context.Background(), deps)
	var missed int
	d.QueryRow(`SELECT COUNT(*) FROM fin_biller_alerts WHERE biller_id = $1 AND kind = 'missed' AND due_date = $2`, late, yesterday).Scan(&missed)
	if missed != 1 {
		t.Fatalf("missed alerts: %d", missed)
	}
	if n := recurringNudge("Salary", 67772, true, time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)); n.Title != "Salary isn't recorded yet" || n.Body != "₹67,772.00 was expected today. Add it once it's done." {
		t.Fatalf("nudge: %+v", n)
	}
}
