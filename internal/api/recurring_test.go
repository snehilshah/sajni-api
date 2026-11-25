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
	// Which cycle a payment pays, from the due dates alone.
	for _, c := range []struct {
		next, freq string
		anchor     int
		day, want  string // want "" = none (an earlier cycle)
	}{
		{"2026-12-05", "monthly", 5, "2027-01-06", "2027-01-05"},    // Dec missed; nearer 5 Jan
		{"2026-12-05", "monthly", 5, "2026-12-06", "2026-12-05"},    // a day late
		{"2026-12-16", "monthly", 16, "2026-12-03", "2026-12-16"},   // electricity paid when it arrives
		{"2026-12-16", "monthly", 16, "2026-11-17", "2026-12-16"},   // the day after the last due date
		{"2026-12-16", "monthly", 16, "2026-11-16", ""},             // the last due date: that cycle's
		{"2026-12-16", "monthly", 16, "2026-12-29", "2026-12-16"},   // 13 days late, nearer 16 Dec
		{"2026-12-16", "monthly", 16, "2027-01-03", "2027-01-16"},   // nearer 16 Jan: December stays missed
		{"2027-01-16", "bimonthly", 16, "2026-12-01", "2027-01-16"}, // gas: the whole two months
		{"2027-01-16", "bimonthly", 16, "2026-11-16", ""},
	} {
		got, ok := db.RecurringCycleOn(day(c.next), c.freq, c.anchor, day(c.day))
		if s := got.Format("2006-01-02"); (c.want == "" && ok) || (c.want != "" && (!ok || s != c.want)) {
			t.Errorf("%s %s, paid %s: %s %v, want %q", c.next, c.freq, c.day, s, ok, c.want)
		}
	}
	for _, c := range []struct {
		name, text string
		want       bool
	}{
		{"MSEDCL Electricity", "Electricity bill payment", true},
		{"MSEDCL Electricity", "INR 3,930 spent on MSEDCL MUMBAI", true},
		{"Mahanagar Gas", "Paid to MAHANAGAR GAS LTD", true},
		{"MSEDCL Electricity", "Paid to SHREYANS VIPUL CHHEDA", false},
		{"Electricity Bill", "Water bill payment", false}, // "bill" alone says nothing
	} {
		if got := db.MentionsRecurringName(c.name, c.text); got != c.want {
			t.Errorf("%q in %q: %v", c.name, c.text, got)
		}
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
	// MSEDCL Electricity: any card (whichever has the offer), due the 16th,
	// paid when the bill arrives. The concert on the same card, in the same
	// stretch and amount range, isn't it; the card's SMS alone names no one;
	// Amazon Pay's mail naming MSEDCL then ticks it; made Papa's lend, it
	// stays ticked.
	var card int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,'Amazon Pay CC','credit_card','8004') RETURNING id`, uid).Scan(&card)
	elec := int64(call("POST", "/api/finance/billers", map[string]any{"name": "MSEDCL Electricity", "kind": "bill", "amount": 3930, "next_due_date": "2026-12-16"})["id"].(float64))
	capture := func(text, source, channel, at string, p ai.ParsedTxn) captureResp {
		canned[text] = p
		b, _ := json.Marshal(call("POST", "/api/finance/captures", map[string]any{"text": text, "source": source, "sender": channel, "channel": channel, "sender_label": channel, "received_at": at}))
		var c captureResp
		json.Unmarshal(b, &c)
		return c
	}
	concert := capture("INR 3,342.00 spent using ICICI Bank Card XX8004 on 02-Dec-26 on SHREYANS VIPUL. Avl Limit: INR 1,00,000.", "sms", "sms", "2026-12-02T12:20:30+05:30",
		ai.ParsedTxn{Amount: 3342, Type: "expense", Description: "Shreyans Vipul", Date: "2026-12-02", Time: "12:20", AccountHint: "8004"})
	cardSMS := capture("INR 3,500.00 spent using ICICI Bank Card XX8004 on 03-Dec-26 on AMAZON PAY IN U. Avl Limit: INR 96,000.", "sms", "sms", "2026-12-03T08:31:30+05:30",
		ai.ParsedTxn{Amount: 3500, Type: "expense", Description: "Amazon Pay In U", Date: "2026-12-03", Time: "08:31", AccountHint: "8004"})
	if concert.Recurring != "" || cardSMS.Outcome != "created" || cardSMS.Recurring != "" || due(elec) != "2026-12-16" {
		t.Fatalf("before the mail: concert %+v, card %+v, next %s", concert, cardSMS, due(elec))
	}
	mail := capture("Amazon Pay: Your payment of ₹3,500 for MSEDCL electricity bill was successful. Paid with ICICI card 8004.", "notification", "com.google.android.gm/amazon pay/electricity bill payment", "2026-12-03T08:32:10+05:30",
		ai.ParsedTxn{Amount: 3500, Type: "expense", Description: "MSEDCL", Date: "2026-12-03", Time: "08:31", AccountHint: "8004"})
	if mail.Outcome != "absorbed" || mail.TxnID != cardSMS.TxnID || mail.Recurring != "MSEDCL Electricity" || mail.RecurringDue != "2026-12-16" || due(elec) != "2027-01-16" {
		t.Fatalf("mail copy: %+v, next %s", mail, due(elec))
	}
	call("POST", "/api/finance/lends/paid-for", map[string]any{"borrower": "Papa", "transaction_ids": []int64{cardSMS.TxnID}, "due_date": ""})
	var typ string
	var linked bool
	d.QueryRow(`SELECT t.type, EXISTS (SELECT 1 FROM fin_biller_payment_txns l WHERE l.txn_id = t.id) FROM fin_transactions t WHERE t.id = $1`, cardSMS.TxnID).Scan(&typ, &linked)
	if typ != "lend" || !linked {
		t.Fatalf("Papa's lend: %s, linked %v", typ, linked)
	}

	if n := recurringNudge("Salary", 67772, true, time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 5, 0, 0, 0, 0, time.UTC)); n.Title != "Salary isn't recorded yet" || n.Body != "₹67,772.00 was expected today. Add it once it's done." {
		t.Fatalf("nudge: %+v", n)
	}
}

// Slice 2: how a row reads, undoing a wrong match, and "make it recurring?"
// suggestions from two months of ChatGPT Plus (not a daily ₹22 ride).
func TestRecurringStatesUnlinkAndSuggestions(t *testing.T) {
	day := func(s string) time.Time { v, _ := time.Parse("2006-01-02", s); return v }
	at := func(s string) time.Time { v, _ := time.Parse("2006-01-02 15:04", s); return v }
	for _, c := range []struct {
		next, lastDue, lastPaid, now, state, date string
	}{
		{"2026-12-05", "2026-11-05", "2026-11-06", "2026-11-20 10:00", "paid", "2026-11-06"},
		{"2026-12-05", "2026-11-05", "2026-11-06", "2026-12-03 10:00", "due", "2026-12-05"},
		{"2026-12-05", "", "", "2026-12-05 19:59", "due", "2026-12-05"},
		{"2026-12-05", "", "", "2026-12-05 20:00", "not_recorded", "2026-12-05"},
		{"2026-12-05", "2026-11-05", "2026-11-06", "2026-12-07 09:00", "not_recorded", "2026-12-05"},
	} {
		if s, d := db.RecurringState(day(c.next), c.lastDue, c.lastPaid, at(c.now)); s != c.state || d != c.date {
			t.Errorf("%s on %s: %s %s, want %s %s", c.next, c.now, s, d, c.state, c.date)
		}
	}

	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000e5"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name, timezone) VALUES ($1, 'recurring2@test', 'T', 'Asia/Kolkata')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var rupay, salary int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'RuPay','credit_card') RETURNING id`, uid).Scan(&rupay)
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Salary','salary') RETURNING id`, uid).Scan(&salary)
	deps := Deps{DB: d, AILimiter: newAILimiter()}
	mux := http.NewServeMux()
	registerFinanceRoutes(mux, deps)
	registerBillerRoutes(mux, deps)
	call := func(method, path string, body any) []byte {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(body)
		req := httptest.NewRequest(method, path, &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	now := time.Now().In(ist)
	thisMonth := time.Date(now.Year(), now.Month(), 1, 12, 0, 0, 0, ist)
	add := func(acct int64, title string, amount float64, when time.Time) {
		t.Helper()
		call("POST", "/api/finance/transactions?force=true", map[string]any{"account_id": acct, "type": "expense", "amount": amount, "description": title, "txn_at": when.Format(time.RFC3339)})
	}
	// ChatGPT Plus on the 1st of last month and this month; a ₹22 ride
	// several times a month.
	add(rupay, "ChatGPT Plus", 1950, thisMonth.AddDate(0, -1, 0))
	add(rupay, "ChatGPT Plus", 1950, thisMonth)
	for _, dd := range []int{0, 1, 2} {
		add(rupay, "Vashi Station", 22, thisMonth.AddDate(0, -1, dd))
		add(rupay, "Vashi Station", 22, thisMonth.AddDate(0, 0, dd))
	}
	var sugg []db.RecurringSuggestion
	json.Unmarshal(call("GET", "/api/finance/billers/suggestions", nil), &sugg)
	if len(sugg) != 1 || sugg[0].Title != "ChatGPT Plus" || sugg[0].AccountID != rupay || len(sugg[0].Months) != 2 || sugg[0].Day != 1 {
		t.Fatalf("suggestions: %+v", sugg)
	}
	call("POST", "/api/finance/billers/suggestions/dismiss", map[string]any{"key": sugg[0].Key})
	sugg = nil
	json.Unmarshal(call("GET", "/api/finance/billers/suggestions", nil), &sugg)
	if len(sugg) != 0 {
		t.Fatalf("after dismiss: %+v", sugg)
	}

	// Unlink: a wrong match is undone and the cycle is due again.
	var item struct{ ID int64 }
	json.Unmarshal(call("POST", "/api/finance/billers", map[string]any{"name": "Rent", "amount": 9000, "next_due_date": now.Format("2006-01-02"), "account_id": salary}), &item)
	call("POST", fmt.Sprintf("/api/finance/billers/%d/pay", item.ID), map[string]any{"due_date": now.Format("2006-01-02")})
	var pays []struct{ ID int64 }
	json.Unmarshal(call("GET", fmt.Sprintf("/api/finance/billers/%d/payments", item.ID), nil), &pays)
	var listed []billerResp
	json.Unmarshal(call("GET", "/api/finance/billers", nil), &listed)
	if len(pays) != 1 || len(listed) != 1 || listed[0].State != "paid" {
		t.Fatalf("paid: %+v %+v", pays, listed)
	}
	var undo map[string]any
	json.Unmarshal(call("DELETE", fmt.Sprintf("/api/finance/billers/%d/payments/%d", item.ID, pays[0].ID), nil), &undo)
	var rows int
	d.QueryRow(`SELECT COUNT(*) FROM fin_transactions WHERE user_id = $1 AND description = 'Rent'`, uid).Scan(&rows)
	if undo["next_due_date"] != now.Format("2006-01-02") || rows != 1 {
		t.Fatalf("unlink: %v, rows %d", undo, rows)
	}

	// The nudge carries what its action fills the sheet with.
	data := recurringNudgeData(context.Background(), deps, item.ID, uid)
	if data["txn_amount"] != "9000.00" || data["txn_type"] != "expense" || data["txn_account_id"] != fmt.Sprint(salary) || data["txn_title"] != "Rent" {
		t.Fatalf("nudge data: %v", data)
	}
}
