package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"sajni/internal/ai"
	"sajni/internal/auth"
	"sajni/internal/db"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/api -run Capture
//
// The messages are the owner's real ones from the 13 Nov 2026 recon
// (finance/recon-2026-11-13.md); the AI parse is canned.
func TestCaptureMemory(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000c9"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'captures@test', 'T')`, uid)
	d.Exec(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE)`, uid)
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	var kotak, wallet, rd int64
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,'Kotak','savings','2851, kotak') RETURNING id`, uid).Scan(&kotak)
	d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type, match_hints) VALUES ($1,'Amazon Wallet','cash','Amazon Pay') RETURNING id`, uid).Scan(&wallet)
	d.QueryRow(`INSERT INTO fin_investments (user_id, name, type, account_id) VALUES ($1,'Savings RD','rd',$2) RETURNING id`, uid, kotak).Scan(&rd)
	d.Exec(`INSERT INTO fin_transactions (user_id, account_id, type, amount, description, txn_at, investment_id, slate_id)
		VALUES ($1,$2,'transfer_out',10000,'Savings RD (auto)','2026-11-12T00:00:00+05:30',$3,(SELECT id FROM fin_slates WHERE user_id=$1))`, uid, kotak, rd)

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
	postRef := func(text, source, sender, ref string, force bool) captureResp {
		t.Helper()
		var buf bytes.Buffer
		json.NewEncoder(&buf).Encode(map[string]any{"text": text, "source": source, "sender": sender, "channel": sender, "sender_label": sender, "ref": ref, "force": force})
		req := httptest.NewRequest("POST", "/api/finance/captures", &buf)
		req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var out captureResp
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code >= 300 {
			t.Fatalf("capture: %d %s", rec.Code, rec.Body.String())
		}
		return out
	}
	post := func(text, source, sender string, force bool) captureResp {
		t.Helper()
		return postRef(text, source, sender, "", force)
	}
	rows := func() (n int) {
		d.QueryRow(`SELECT count(*) FROM fin_transactions WHERE user_id = $1 AND investment_id IS NULL`, uid).Scan(&n)
		return
	}
	expect := func(name string, got captureResp, outcome string, wantRows int) {
		t.Helper()
		if got.Outcome != outcome || rows() != wantRows {
			t.Fatalf("%s: outcome %q (%s), rows %d; want %q, %d", name, got.Outcome, got.Reason, rows(), outcome, wantRows)
		}
	}
	day := "2026-11-13"

	// The NEFT to Papa: SMS, the bank's email at once, and its second email 41 minutes later.
	sms := parse("Sent Rs.2724.00 from Kotak Bank AC XXXXXX2851 to RONAK ARVIND SHAH on 13-11-26.UTR:KKBKH26317963185.",
		ai.ParsedTxn{Amount: 2724, Type: "expense", Description: "Ronak Arvind Shah", Date: day, Time: "02:15", AccountHint: "2851", RefID: "KKBKH26317963185"})
	first := post(sms, "sms", "JD-KOTAKB-S", false)
	expect("NEFT SMS", first, "created", 1)
	mail := "Fund transfer to an account at another Bank\nDebit Account Number: XXXXXX2851 \nAmount: INR. 2724.00 \nUnique Transaction Reference Number (UTR): KKBKH26317963185"
	expect("NEFT email, same UTR", postRef(mail, "notification", "com.google.android.gm/bankalerts/fund transfer to an account at another bank", "KKBKH26317963185", false), "absorbed", 1)
	later := parse("InterBank Transfer Credit Alert\nRs.2724 has been credited to destination account as per your request for NEFT Transaction on 13-11-2026 at 02:39:15",
		ai.ParsedTxn{Amount: 2724, Type: "expense", Description: "NEFT", Date: day, Time: "02:39", AccountHint: "Kotak"})
	expect("NEFT credited email, no UTR", post(later, "notification", "com.google.android.gm/bankalerts/interbank transfer credit alert", false), "absorbed", 1)

	// Two real ₹22 payments to one stall minutes apart: each has its own SMS.
	a := parse("Sent Rs.22.00 from Kotak Bank A/c X2851 to Bhola Khan on 12-11-26. UPI Ref 631300000001",
		ai.ParsedTxn{Amount: 22, Type: "expense", Description: "Bhola Khan", Date: "2026-11-12", Time: "09:12", AccountHint: "2851", RefID: "631300000001"})
	b := parse("Sent Rs.22.00 from Kotak Bank A/c X2851 to Bhola Khan on 12-11-26. UPI Ref 631300000002",
		ai.ParsedTxn{Amount: 22, Type: "expense", Description: "Bhola Khan", Date: "2026-11-12", Time: "09:15", AccountHint: "2851", RefID: "631300000002"})
	expect("first chai", post(a, "sms", "JD-KOTAKB-S", false), "created", 2)
	expect("second chai", post(b, "sms", "JD-KOTAKB-S", false), "created", 3)
	// The same with no reference at all: the SMS channel already reported the first.
	c1 := parse("Rs.25 debited A/c X2851 to Salman 11-11", ai.ParsedTxn{Amount: 25, Type: "expense", Description: "Salman", Date: "2026-11-11", Time: "19:16", AccountHint: "2851"})
	c2 := parse("Rs.25 debited A/c X2851 to Salman 11-11 again", ai.ParsedTxn{Amount: 25, Type: "expense", Description: "Salman", Date: "2026-11-11", Time: "19:20", AccountHint: "2851"})
	post(c1, "sms", "JD-KOTAKB-S", false)
	expect("second ref-less SMS", post(c2, "sms", "JD-KOTAKB-S", false), "created", 5)

	// Alefiya's ₹1,322: Kotak SMS, then GPay's notification without a reference.
	in := parse("Received Rs.1322.00 in your Kotak Bank AC 2851 from ALEFIYA ABBAS RAMPUR on 12-11-26.UPI Ref:111167559631",
		ai.ParsedTxn{Amount: 1322, Type: "income", Description: "Alefiya Abbas Rampur", Date: "2026-11-12", Time: "22:27", AccountHint: "2851", RefID: "111167559631"})
	expect("credit SMS", post(in, "sms", "JX-KOTAKB-S", false), "created", 6)
	gpay := parse("ALEFIYA ABBAS RAMPURAWALA paid you ₹1,322.00\nUPI Payment", ai.ParsedTxn{Amount: 1322, Type: "income", Description: "Alefiya", Date: "2026-11-12", Time: "22:27"})
	expect("GPay copy", post(gpay, "notification", "com.google.android.apps.nbu.paisa.user", false), "absorbed", 6)

	// The RD: SMS and email confirm the instalment Sajni posted.
	si := parse("Standing Instruction for INR 10000.00 from XXXX2851 to bene a/c No: 9749911837 has been processed successfully on 12-NOV-2026",
		ai.ParsedTxn{Amount: 10000, Type: "expense", Description: "Standing Instruction", Date: "2026-11-12", Time: "07:17", AccountHint: "2851"})
	expect("RD SMS", post(si, "sms", "JM-KOTAKB-S", false), "confirmed", 6)
	siMail := parse("Standing Instruction – Success\nDebit Account Number: XXXX2851, 12-NOV-2026, Rs.10000",
		ai.ParsedTxn{Amount: 10000, Type: "expense", Description: "Bene A/c 9749911837", Date: "2026-11-12", Time: "07:29", AccountHint: "2851"})
	expect("RD email", post(siMail, "notification", "com.google.android.gm/bankalerts/standing instruction – success", false), "confirmed", 6)

	// Uber: Amazon Pay's debit and Uber's receipt (now read as the part Amazon Pay paid).
	amz := parse("Payment of ₹82.13 to Uber India via Amazon Pay balance", ai.ParsedTxn{Amount: 82.13, Type: "expense", Description: "Uber India", Date: "2026-11-09", Time: "20:56", AccountHint: "Amazon Pay"})
	uber := parse("Your Thursday trip with Uber · Total ₹87.67 · Uber Cash ₹5.54 + Amazon Pay ₹82.13", ai.ParsedTxn{Amount: 82.13, Type: "expense", Description: "Uber", Date: "2026-11-09", Time: "20:56", AccountHint: "Amazon Pay"})
	expect("Amazon Pay debit", post(amz, "notification", "com.google.android.gm/amazon pay india/payment", false), "created", 7)
	expect("Uber receipt", post(uber, "notification", "com.google.android.gm/uber receipts/your thursday trip", false), "absorbed", 7)

	// Not money moving; no account; "Add anyway"; the same text twice.
	promo := parse("Wohoo! You got ₹0.2 cashback", ai.ParsedTxn{})
	expect("POP reward", post(promo, "notification", "com.popclub.android", false), "ignored", 7)
	atm := parse("ICICI Bank Acc XX928 debited Rs. 2,000.00 on 12-Nov-26 CAM*70203HRY*", ai.ParsedTxn{Amount: 2000, Type: "expense", Description: "CAM 70203HRY", Date: "2026-11-12", Time: "22:12", AccountHint: "928"})
	expect("unknown account", post(atm, "sms", "JD-ICICIT-S", false), "review", 7)
	expect("same text again", post(sms, "sms", "JD-KOTAKB-S", false), "absorbed", 7)
	expect("add anyway", post(gpay, "notification", "com.google.android.apps.nbu.paisa.user", true), "review", 7)

	// Deleted on purpose, then a late copy with the same reference: stays deleted.
	d.Exec(`DELETE FROM fin_transactions WHERE id = $1`, first.TxnID)
	expect("late copy of a deleted row", postRef(mail+" ", "notification", "com.google.android.gm/bankalerts/x", "KKBKH26317963185", false), "absorbed", 6)

	// The transaction list says where each row was heard of.
	req := httptest.NewRequest("GET", "/api/finance/transactions?limit=50", nil)
	req = req.WithContext(context.WithValue(req.Context(), auth.ContextKey{}, uid))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var list []struct {
		ID      int64   `json:"id"`
		Amount  float64 `json:"amount"`
		SeenVia string  `json:"seen_via"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list: %v %.200s", err, rec.Body.String())
	}
	found := false
	for _, tx := range list {
		found = found || tx.Amount == 1322
		if tx.Amount == 1322 && tx.SeenVia != "com.google.android.apps.nbu.paisa.user · JX-KOTAKB-S" {
			t.Fatalf("seen_via for Alefiya: %q", tx.SeenVia)
		}
	}
	if !found {
		t.Fatal("Alefiya's credit missing from the list")
	}
}
