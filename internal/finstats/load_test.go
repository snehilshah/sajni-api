package finstats

import (
	"context"
	"testing"
	"time"

	"sajni/internal/db"
	"sajni/internal/dbtest"
)

// Load: blank categories join the user's Others, and a debt from before
// Sajni doesn't move the start of history.
func TestLoad(t *testing.T) {
	dsn := dbtest.URL(t)
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	uid := "01900000-0000-7000-8000-0000000000f5"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	_, err = d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'finstats@test', 'T')`, uid)
	must(err)
	var plain, bank, others int64
	must(d.QueryRow(`INSERT INTO fin_slates (user_id, name, is_plain) VALUES ($1, 'Plain', TRUE) RETURNING id`, uid).Scan(&plain))
	must(d.QueryRow(`INSERT INTO fin_accounts (user_id, name, type) VALUES ($1,'Bank','savings') RETURNING id`, uid).Scan(&bank))
	d.Exec(`DELETE FROM fin_categories WHERE user_id = $1`, uid)
	must(d.QueryRow(`INSERT INTO fin_categories (user_id, name, kind, color) VALUES ($1,'Others','expense','#123456') RETURNING id`, uid).Scan(&others))
	add := func(typ string, cat any, at string) {
		t.Helper()
		_, err := d.Exec(`INSERT INTO fin_transactions (user_id, account_id, category_id, type, amount, description, txn_at, slate_id) VALUES ($1,$2,$3,$4,100,'x',$5,$6)`,
			uid, bank, cat, typ, at, plain)
		must(err)
	}
	add("owed", nil, "2026-09-30T12:00:00+05:30")
	add("expense", nil, "2026-10-01T20:40:00+05:30")
	add("expense", others, "2026-10-02T10:00:00+05:30")

	ist, _ := time.LoadLocation("Asia/Kolkata")
	rows, first, err := Load(context.Background(), d, uid, time.Date(2026, 10, 1, 0, 0, 0, 0, ist))
	must(err)
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, ist); !first.Equal(want) {
		t.Fatalf("first = %v, want %v", first, want)
	}
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, r := range rows {
		if r.CategoryID == nil || *r.CategoryID != others || r.Category != "Others" || r.Color != "#123456" {
			t.Fatalf("not one Others: %+v", r)
		}
	}
}
