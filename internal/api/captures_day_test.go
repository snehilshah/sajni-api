package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
)

// The owner's 13 Nov 2026, the capture endpoint's first full day: an Amazon
// Pay top-up filed as a spend and an income, ICICI's card email read 12
// hours early and filed twice, the Messages app re-posting SMS, Zomato
// Money prompts, Kotak's ActivMoney FD maturing as ₹20k of "income", and
// UPI Lite loads to the owner's own name.
func TestCaptureDay(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000d4"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'snehilshah27@test', 'therealsajni')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	acct := func(name, typ, hints string) (id int64) {
		d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,$2,$3,$4) RETURNING id`, uid, name, typ, hints).Scan(&id)
		return
	}
	kotak := acct("Salary", "salary", "2851, kotak")
	icici := acct("ICICI Savings", "savings", "928, icici")
	wallet := acct("Amazon Wallet", "cash", "Amazon Pay")
	acct("Amazon Pay CC", "credit_card", "8004")
	lite := acct("UPI Lite", "cash", "")
	cash := acct("Cash", "cash", "")
	d.Exec(`INSERT INTO fin_categories (user_id, name, kind, color) VALUES ($1,'Interest','income','#000')`, uid)

	canned := map[string]ai.ParsedTxn{}
	parse := func(text string, p ai.ParsedTxn) string { canned[text] = p; return text }
	captureParserSwapped = true
	old := captureParser
	captureParser = func(_ context.Context, _ Deps, text, _ string) (ai.ParsedTxn, int, error) {
		return canned[text], 1, nil
	}
	defer func() { captureParser, captureParserSwapped = old, false }()

	mux := http.NewServeMux()
	registerFinanceRoutes(mux, Deps{DB: d, AILimiter: newAILimiter()})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
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
		return rec
	}
	post := func(text, channel, at string) captureResp {
		t.Helper()
		source := "notification"
		if channel == "sms" {
			source = "sms"
		}
		var out captureResp
		json.Unmarshal(call("POST", "/api/finance/captures", map[string]any{
			"text": text, "source": source, "sender": channel, "channel": channel, "sender_label": channel,
			"received_at": "2026-11-13T" + at + ":00+05:30",
		}).Body.Bytes(), &out)
		return out
	}
	want := func(name string, got captureResp, outcome string) {
		t.Helper()
		if got.Outcome != outcome {
			t.Fatalf("%s: outcome %q (%s), want %q", name, got.Outcome, got.Reason, outcome)
		}
	}
	day := "2026-11-13"

	// Amazon Pay top-up: ICICI's SMS, then Amazon's email → one transfer.
	sms := parse("ICICI Bank Acct XX928 debited for Rs 500.00 on 13-Nov-26; Amazon Pay Bala credited. UPI:631772992137.",
		ai.ParsedTxn{Amount: 500, Type: "expense", Description: "Amazon Pay", Date: day, AccountHint: "928", RefID: "631772992137", OwnMoney: true})
	want("top-up SMS", post(sms, "sms", "15:29"), "created")
	mail := parse("Amazon Pay India Updates ₹500.00 added to your Amazon Pay Balance.",
		ai.ParsedTxn{Amount: 500, Type: "income", Description: "Amazon Pay", Date: day, AccountHint: "Amazon Pay", OwnMoney: true})
	got := post(mail, "com.google.android.gm/amazon pay/₹. added to your amazon pay balance", "15:30")
	want("top-up email", got, "created")
	if got.Transfer != "ICICI Savings → Amazon Wallet" {
		t.Fatalf("top-up transfer = %q", got.Transfer)
	}
	var outs, ins int
	d.QueryRow(`SELECT count(*) FILTER (WHERE type='transfer_out' AND account_id=$2), count(*) FILTER (WHERE type='transfer_in' AND account_id=$3)
		FROM fin_transactions WHERE user_id=$1 AND amount=500`, uid, icici, wallet).Scan(&outs, &ins)
	if outs != 1 || ins != 1 {
		t.Fatalf("top-up legs: out %d in %d", outs, ins)
	}
	// A ₹500 purchase and an unrelated ₹500 credit are not a transfer.
	buy := parse("ICICI Acct XX928 debited Rs 300.00 to Swiggy", ai.ParsedTxn{Amount: 300, Type: "expense", Description: "Swiggy", Date: day, Time: "16:00", AccountHint: "928"})
	gift := parse("Received Rs.300.00 in your Kotak Bank AC 2851 from AMAN", ai.ParsedTxn{Amount: 300, Type: "income", Description: "Aman", Date: day, Time: "16:05", AccountHint: "2851"})
	want("Swiggy", post(buy, "sms", "16:00"), "created")
	if g := post(gift, "com.google.android.apps.nbu.paisa.user", "16:05"); g.Outcome != "created" || g.Transfer != "" {
		t.Fatalf("unrelated credit paired: %+v", g)
	}

	// ICICI's card: SMS at 23:40, its email says "at 11:40:02" (no AM/PM).
	card := parse("INR 216.00 spent using ICICI Bank Card XX8004 on 13-Nov-26 on AMAZON PAY IN G.",
		ai.ParsedTxn{Amount: 216, Type: "expense", Description: "Amazon Pay", Date: day, AccountHint: "8004"})
	want("card SMS", post(card, "sms", "23:40"), "created")
	cardMail := parse("Your ICICI Bank Credit Card XX8004 has been used for a transaction of INR 216.00 on Nov 13, 2026 at 11:40:02.",
		ai.ParsedTxn{Amount: 216, Type: "expense", Description: "Amazon Pay In Grocery", Date: day, Time: "11:40", AccountHint: "8004"})
	want("card email", post(cardMail, "com.google.android.gm/credit_cards/transaction alert", "23:40"), "absorbed")

	// Zomato Money: asked once; the Messages app's copy isn't asked again.
	zm := parse("Payment of Rs. 6.38 from Zomato Money Balance is successful.",
		ai.ParsedTxn{Amount: 6.38, Type: "expense", Description: "Zomato", Date: day, AccountHint: "Zomato Money"})
	want("Zomato Money", post(zm, "sms", "15:56"), "review")
	want("Messages copy", post("JD-ZOMATO-S "+zm, "com.google.android.apps.messaging", "17:37"), "absorbed")
	// "Don't track Zomato Money": the next one is ignored.
	call("POST", "/api/finance/captures/ignores", map[string]string{"hint": "Zomato Money", "label": "Zomato Money"})
	zm2 := parse("Rs. 16.59 added to Zomato Money", ai.ParsedTxn{Amount: 16.59, Type: "income", Description: "Zomato Money", Date: day, AccountHint: "Zomato Money"})
	want("untracked wallet", post(zm2, "sms", "16:37"), "ignored")

	// ActivMoney FD: ₹20,493 back, of which ₹493 is interest.
	fd := parse("FD matured! ₹20,493 credited to XX2851. Check out details.",
		ai.ParsedTxn{Amount: 20493, Type: "income", Description: "FD Maturity", Date: day, AccountHint: "2851"})
	got = post(fd, "com.kotak.bank.mobile", "08:20")
	want("FD", got, "created")
	var interest float64
	d.QueryRow(`SELECT amount FROM fin_transactions WHERE id = $1`, got.TxnID).Scan(&interest)
	if interest != 493 || got.Interest != 493 {
		t.Fatalf("FD interest = %v (%v)", interest, got.Interest)
	}
	fdSMS := parse("Your Fixed Deposit has matured and Rs.20493 is credited to a/c 2851",
		ai.ParsedTxn{Amount: 20493, Type: "income", Description: "FD Maturity", Date: day, AccountHint: "2851"})
	want("FD SMS copy", post(fdSMS, "sms", "08:21"), "absorbed")

	// UPI Lite: the owner's own name on a Kotak debit. Turned into a
	// transfer once, the next load is filed as one.
	load := parse("Sent Rs.100.00 from Kotak Bank A/c X2851 to SNEHIL RONAK SHAH on 13-11-26. UPI Ref 668336516268.",
		ai.ParsedTxn{Amount: 100, Type: "expense", Description: "Snehil Ronak Shah", Date: day, Time: "09:10", AccountHint: "2851", RefID: "668336516268"})
	got = post(load, "sms", "09:10")
	want("first load", got, "created")
	call("POST", "/api/finance/transactions/"+strconv.FormatInt(got.TxnID, 10)+"/transfer", map[string]any{"account_id": lite, "description": "Transfer to UPI Lite"})
	load2 := parse("Sent Rs.200.00 from Kotak Bank A/c X2851 to SNEHIL RONAK SHAH on 14-11-26. UPI Ref 668336516999.",
		ai.ParsedTxn{Amount: 200, Type: "expense", Description: "Snehil Ronak Shah", Date: "2026-11-14", Time: "09:10", AccountHint: "2851", RefID: "668336516999"})
	got = post(load2, "sms", "09:10")
	if got.Outcome != "created" || got.Transfer != "Salary → UPI Lite" {
		t.Fatalf("second load: %+v", got)
	}
	_ = kotak

	// Rahul sent back cash by UPI: a transfer once, but not a rule (his
	// next payment may be anything).
	rahul := parse("Received Rs.200.00 in your Kotak Bank AC 2851 from RAHUL DHANSINGH SHAH",
		ai.ParsedTxn{Amount: 200, Type: "income", Description: "Rahul Dhansingh Shah", Date: day, Time: "18:39", AccountHint: "2851"})
	got = post(rahul, "sms", "18:39")
	call("POST", "/api/finance/transactions/"+strconv.FormatInt(got.TxnID, 10)+"/transfer", map[string]any{"account_id": cash})
	rahul2 := parse("Received Rs.150.00 in your Kotak Bank AC 2851 from RAHUL DHANSINGH SHAH",
		ai.ParsedTxn{Amount: 150, Type: "income", Description: "Rahul Dhansingh Shah", Date: day, Time: "20:00", AccountHint: "2851"})
	if got = post(rahul2, "sms", "20:00"); got.Transfer != "" {
		t.Fatalf("one conversion became a rule: %+v", got)
	}
	// Rahul's money left Cash.
	var cashOut string
	d.QueryRow(`SELECT type FROM fin_transactions WHERE user_id=$1 AND account_id=$2 AND amount=200`, uid, cash).Scan(&cashOut)
	if cashOut != "transfer_out" {
		t.Fatalf("Rahul's cash leg = %q", cashOut)
	}
}
