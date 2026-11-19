package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Needs a scratch Postgres. Capture before December, from the owner's own
// November: the ₹99 Clash of Clans payment in both orders, card bills paid
// with POP's discount, the RD maturing late, two identical ₹22 notifications,
// and account hints that matched too loosely.
func TestCaptureSafety(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000d3"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'safety@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	acct := func(name, typ, hints string) (id int64) {
		t.Helper()
		if err := d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,$2,$3,$4) RETURNING id`, uid, name, typ, hints).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return
	}
	salary := acct("Salary", "salary", "2851, kotak")
	wallet := acct("Amazon Wallet", "cash", "8169558980@amazonpay, Amazon Pay, Amazon Pay Balance, Apay")
	icici := acct("Amazon Pay CC", "credit_card", "Amazon ICICI, 8004")
	rupay := acct("RuPay", "credit_card", "7744, slice, POP")
	var cashback int64
	d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind) VALUES ($1,'Cashback','income') RETURNING id`, uid).Scan(&cashback)

	canned := map[string]ai.ParsedTxn{}
	parse := func(text string, p ai.ParsedTxn) string { canned[text] = p; return text }
	captureParserSwapped = true
	old := captureParser
	captureParser = func(_ context.Context, _ Deps, _, text, _ string) (ai.ParsedTxn, int, error) {
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
		return rec
	}
	post := func(text, source, sender, at string) captureResp {
		t.Helper()
		rec := call("POST", "/api/finance/captures", map[string]any{"text": text, "source": source, "sender": sender, "channel": sender, "sender_label": sender, "received_at": at})
		var out captureResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code >= 300 {
			t.Fatalf("capture: %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	type row struct {
		typ, title string
		amount     float64
		account    int64
	}
	rowsSince := func(after int64) (out []row) {
		r, err := d.Query(`SELECT type, description, amount, account_id FROM fin_transactions WHERE user_id = $1 AND id > $2 ORDER BY id`, uid, after)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for r.Next() {
			var x row
			r.Scan(&x.typ, &x.title, &x.amount, &x.account)
			out = append(out, x)
		}
		return
	}
	mark := func() (id int64) {
		d.QueryRow(`SELECT COALESCE(MAX(id),0) FROM fin_transactions`).Scan(&id)
		return
	}

	// ₹99 on 15 Nov, the wallet's SMS first: one row on Amazon Wallet,
	// titled by the Google Play receipt.
	day := "2026-11-15"
	sms := parse("Apay Wallet: INR 99.00 debited for Google Play on 15-11-26. Ref 668580326877",
		ai.ParsedTxn{Amount: 99, Type: "expense", Description: "Google Play", Date: day, Time: "07:09", AccountHint: "Apay Wallet", RefID: "668580326877"})
	receipt := parse("Your Google Play order receipt from Nov 15, 2026\nSupercell Clash of Clans ₹99.00\nPayment method: UPI: Amazon",
		ai.ParsedTxn{Amount: 99, Type: "expense", Description: "Supercell", Date: day, Time: "07:09", AccountHint: "UPI: Amazon", RefID: "GPA33694086081882809", Receipt: true})
	m := mark()
	if c := post(sms, "sms", "JD-JUSPAY-S", day+"T07:09:30+05:30"); c.Outcome != "created" {
		t.Fatalf("wallet SMS: %+v", c)
	}
	if c := post(receipt, "notification", "com.google.android.gm/googleplay/your google play order receipt", day+"T07:10:00+05:30"); c.Outcome != "absorbed" {
		t.Fatalf("receipt after the SMS: %+v", c)
	}
	if got := rowsSince(m); len(got) != 1 || got[0].account != wallet || got[0].title != "Supercell" {
		t.Fatalf("₹99, SMS first: %+v", got)
	}

	// The other order: the receipt waits as a review; the SMS files it and
	// settles that review.
	sms2 := parse("Apay Wallet: INR 99.00 debited for Google Play on 16-11-26. Ref 668580326999",
		ai.ParsedTxn{Amount: 99, Type: "expense", Description: "Google Play", Date: "2026-11-16", Time: "07:09", AccountHint: "Apay Wallet", RefID: "668580326999"})
	receipt2 := parse("Your Google Play order receipt from Nov 16, 2026\nSupercell Clash of Clans ₹99.00\nPayment method: UPI: Amazon",
		ai.ParsedTxn{Amount: 99, Type: "expense", Description: "Supercell", Date: "2026-11-16", Time: "07:09", AccountHint: "UPI: Amazon", RefID: "GPA33694086081882999", Receipt: true})
	m = mark()
	first := post(receipt2, "notification", "com.google.android.gm/googleplay/your google play order receipt", "2026-11-16T07:09:10+05:30")
	if first.Outcome != "review" || first.CaptureID == 0 || len(rowsSince(m)) != 0 {
		t.Fatalf("receipt first: %+v", first)
	}
	second := post(sms2, "sms", "JD-JUSPAY-S", "2026-11-16T07:09:40+05:30")
	if second.Outcome != "created" || len(second.Resolved) != 1 || second.Resolved[0] != first.CaptureID {
		t.Fatalf("SMS after the receipt: %+v", second)
	}
	if got := rowsSince(m); len(got) != 1 || got[0].account != wallet || got[0].title != "Supercell" {
		t.Fatalf("₹99, receipt first: %+v", got)
	}

	// The ICICI bill on 3 Nov: Kotak paid ₹9,374.77, POP took ₹18 off, the
	// card got ₹9,392.77. A transfer plus ₹18 cashback on the card.
	m = mark()
	debit := parse("Sent Rs.9374.77 from Kotak Bank AC X2851 to ICICI Credit Card on 03-11-26. UPI Ref 530712345678",
		ai.ParsedTxn{Amount: 9374.77, Type: "expense", Description: "ICICI Credit Card", Date: "2026-11-03", Time: "08:57", AccountHint: "2851", RefID: "530712345678", OwnMoney: true})
	credit := parse("Payment of INR 9,392.77 has been received on your Amazon Pay ICICI Bank Credit Card XX8004 on 03-Nov-26.",
		ai.ParsedTxn{Amount: 9392.77, Type: "income", Description: "Payment Received", Date: "2026-11-03", Time: "09:40", AccountHint: "XX8004"})
	post(debit, "sms", "JD-KOTAKB-S", "2026-11-03T08:57:10+05:30")
	if c := post(credit, "sms", "JD-ICICIB-S", "2026-11-03T09:40:10+05:30"); c.Outcome != "created" || c.Transfer != "Salary → Amazon Pay CC" {
		t.Fatalf("card credit: %+v", c)
	}
	want := []row{{"transfer_out", "Transfer to Amazon Pay CC", 9374.77, salary}, {"transfer_in", "Transfer to Amazon Pay CC", 9374.77, icici}, {"income", "Discount on card bill", 18, icici}}
	if got := rowsSince(m); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("card bill, bank first: %+v", got)
	}

	// The card's message first this time (a Slice bill, ₹10 off).
	m = mark()
	credit2 := parse("Payment of Rs 14068.11 received towards your slice card XX7744. Thank you!",
		ai.ParsedTxn{Amount: 14068.11, Type: "income", Description: "Slice", Date: "2026-11-03", Time: "09:26", AccountHint: "7744"})
	debit2 := parse("Sent Rs.14058.11 from Kotak Bank AC X2851 to Slice Small Finance on 03-11-26. UPI Ref 530799999999",
		ai.ParsedTxn{Amount: 14058.11, Type: "expense", Description: "Slice Small Finance", Date: "2026-11-03", Time: "09:27", AccountHint: "2851", RefID: "530799999999", OwnMoney: true})
	post(credit2, "sms", "JD-SLICEIT-S", "2026-11-03T09:26:10+05:30")
	if c := post(debit2, "sms", "JD-KOTAKB-S", "2026-11-03T09:27:10+05:30"); c.Outcome != "created" || c.Transfer != "Salary → RuPay" {
		t.Fatalf("bank debit after the card: %+v", c)
	}
	got := rowsSince(m)
	var in, out, disc float64
	for _, r := range got {
		switch r.typ {
		case "transfer_in":
			in = r.amount
		case "transfer_out":
			out = r.amount
		case "income":
			disc = r.amount
		}
	}
	if len(got) != 3 || in != 14058.11 || out != 14058.11 || disc != 10 {
		t.Fatalf("card bill, card first: %+v", got)
	}

	// The RD matures on Saturday 12 Dec; the bank credits it on Monday evening.
	var rd int64
	d.QueryRow(`INSERT INTO fin_investments (user_id, name, type, account_id, invested_amount, current_value, maturity_date)
		VALUES ($1,'Savings RD','rd',$2,150000,150000,'2026-12-12') RETURNING id`, uid, salary).Scan(&rd)
	mat := parse("Rs.156000.00 credited to Kotak Bank AC X2851 on 14-12-26 towards RD maturity",
		ai.ParsedTxn{Amount: 156000, Type: "income", Description: "RD Maturity", Date: "2026-12-14", Time: "19:00", AccountHint: "2851"})
	if c := post(mat, "sms", "JD-KOTAKB-S", "2026-12-14T19:00:10+05:30"); c.Matured != "Savings RD" {
		t.Fatalf("RD two days late: %+v", c)
	}

	// The Kotak app words every ₹22 to Salman the same: a day apart, two rows.
	m = mark()
	chai := parse("₹22 sent to SALMAN MOHAMMA\nAmount debited from XX2851",
		ai.ParsedTxn{Amount: 22, Type: "expense", Description: "Salman Mohamma", Date: "2026-11-12", AccountHint: "XX2851"})
	post(chai, "notification", "com.msf.kbank.mobile", "2026-11-12T13:00:00+05:30")
	d.Exec(`UPDATE fin_captures SET created_at = created_at - INTERVAL '1 day' WHERE user_id = $1 AND text = $2`, uid, chai)
	post(chai, "notification", "com.msf.kbank.mobile", "2026-11-13T13:00:00+05:30")
	if got := rowsSince(m); len(got) != 2 {
		t.Fatalf("two ₹22 a day apart: %+v", got)
	}

	// Kotak's other card names digits that aren't one of the owner's accounts:
	// asked, not filed on Salary; "Don't track" then silences it.
	card := parse("INR 450.00 spent on Kotak Credit Card XX9148 at IOCL on 16-11-26",
		ai.ParsedTxn{Amount: 450, Type: "expense", Description: "IOCL", Date: "2026-11-16", Time: "18:00", AccountHint: "Kotak Credit Card XX9148"})
	if c := post(card, "sms", "JD-KOTAKB-S", "2026-11-16T18:00:10+05:30"); c.Outcome != "review" {
		t.Fatalf("card 9148: %+v", c)
	}
	d.Exec(`INSERT INTO fin_capture_ignores (user_id, hint, label) VALUES ($1, 'kotak credit card xx9148', 'Kotak card ••9148')`, uid)
	card2 := parse("INR 120.00 spent on Kotak Credit Card XX9148 at IOCL on 17-11-26",
		ai.ParsedTxn{Amount: 120, Type: "expense", Description: "IOCL", Date: "2026-11-17", Time: "18:00", AccountHint: "Kotak Credit Card XX9148"})
	if c := post(card2, "sms", "JD-KOTAKB-S", "2026-11-17T18:00:10+05:30"); c.Outcome != "ignored" {
		t.Fatalf("card 9148 after Don't track: %+v", c)
	}
	for hint, want := range map[string]int64{"Poptech": 0, "via POP UPI": rupay, "Amazon Pay ICICI": wallet, "kotak": salary, "8169558980@amazonpay": wallet} {
		var got int64
		if id := matchAccountByHint(d, uid, hint); id != nil {
			got = *id
		}
		if got != want {
			t.Fatalf("hint %q → %d, want %d", hint, got, want)
		}
	}

	// Filing a review from its notification links the capture.
	odd := parse("INR 310.00 debited via Zeta card", ai.ParsedTxn{Amount: 310, Type: "expense", Description: "Zeta", Date: "2026-11-17", Time: "12:00", AccountHint: "Zeta"})
	rv := post(odd, "sms", "JD-ZETA-S", "2026-11-17T12:00:10+05:30")
	rec := call("POST", "/api/finance/transactions", map[string]any{"account_id": salary, "type": "expense", "amount": 310, "description": "Zeta", "txn_at": "2026-11-17T12:00:00+05:30", "capture_id": rv.CaptureID})
	var made struct{ ID int64 }
	json.Unmarshal(rec.Body.Bytes(), &made)
	var outcome string
	var linked int64
	d.QueryRow(`SELECT outcome, COALESCE(txn_id,0) FROM fin_captures WHERE id = $1`, rv.CaptureID).Scan(&outcome, &linked)
	if rec.Code != 201 || outcome != "created" || linked != made.ID {
		t.Fatalf("filed review: %d %s; capture %s → %d", rec.Code, rec.Body.String(), outcome, linked)
	}

	// 19 Nov, ₹8,850 from Salary to Kotak Neo for trading: the Kotak app
	// (no name), Kotak Neo's email naming the source "XXXXXX2851", then the
	// bank's SMS. One transfer, Salary → Kotak Neo.
	neo := acct("Kotak Neo", "investment", "Kotak Neo, Kotak Securities")
	app := parse("₹8,850 debited\nAmount debited from XX2851. Check out details.",
		ai.ParsedTxn{Amount: 8850, Type: "expense", Date: "2026-11-19", AccountHint: "2851"})
	mail := parse("Kotak Neo\nPayment successful for your Kotak neo account\nYour fund transfer of Rs. 8850.00 has been successfully credited to your Kotak Neo account.\nBank name: Kotak Mahindra Bank\nAccount number: XXXXXX2851\nReference number: SqTQKr1795079293",
		ai.ParsedTxn{Amount: 8850, Type: "income", Description: "Kotak Neo", Date: "2026-11-19", Time: "14:38", AccountHint: "2851", RefID: "SqTQKr1795079293", OwnMoney: true})
	bank := parse("Rs.8,850.00 has been debited from your Kotak Bank A/c XX2851 towards KSec trading account via Kotak Direct Access on 19-NOV-26 vide Ref No.0061473528",
		ai.ParsedTxn{Amount: 8850, Type: "expense", Description: "Ksec Trading Account", Date: "2026-11-19", AccountHint: "2851", RefID: "0061473528", OwnMoney: true})
	m = mark()
	if c := post(app, "notification", "com.kotak.bank.mobile", "2026-11-19T14:38:21+05:30"); c.Outcome != "created" {
		t.Fatalf("Kotak app: %+v", c)
	}
	if c := post(mail, "notification", "com.google.android.gm/kotak neo/payment successful for your kotak neo account", "2026-11-19T14:38:22+05:30"); c.Outcome != "created" || c.Transfer != "Salary → Kotak Neo" {
		t.Fatalf("Kotak Neo's email: %+v", c)
	}
	if c := post(bank, "sms", "JD-KOTAKB-S", "2026-11-19T15:07:41+05:30"); c.Outcome != "absorbed" {
		t.Fatalf("Kotak's SMS: %+v", c)
	}
	if got := rowsSince(m); len(got) != 2 || got[0] != (row{"transfer_out", "Transfer to Kotak Neo", 8850, salary}) || got[1] != (row{"transfer_in", "Transfer to Kotak Neo", 8850, neo}) {
		t.Fatalf("₹8,850 to Kotak Neo: %+v", got)
	}

	// A first copy that names no one takes the next copy's name.
	m = mark()
	post(parse("₹500 debited\nAmount debited from XX2851.", ai.ParsedTxn{Amount: 500, Type: "expense", Date: "2026-11-19", AccountHint: "2851"}),
		"notification", "com.kotak.bank.mobile", "2026-11-19T18:00:00+05:30")
	post(parse("Sent Rs.500.00 from Kotak Bank A/c X2851 to RAMESH KUMAR on 19-11-26. UPI Ref 632300000001.", ai.ParsedTxn{Amount: 500, Type: "expense", Description: "Ramesh Kumar", Date: "2026-11-19", AccountHint: "2851", RefID: "632300000001"}),
		"sms", "JD-KOTAKB-S", "2026-11-19T18:00:20+05:30")
	if got := rowsSince(m); len(got) != 1 || got[0].title != "Ramesh Kumar" {
		t.Fatalf("unnamed first copy: %+v", got)
	}
}
