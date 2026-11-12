package ai

import (
	"context"
	"os"
	"testing"

	"sajni/internal/db"
)

// Needs a scratch Postgres: SAJNI_TEST_DATABASE_URL=postgres://… go test ./internal/ai
func TestListCapturesTool(t *testing.T) {
	dsn := os.Getenv("SAJNI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAJNI_TEST_DATABASE_URL not set")
	}
	d, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	uid := "01900000-0000-7000-8000-0000000000ca"
	d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	if _, err := d.Exec(`INSERT INTO users (id, email, name) VALUES ($1, 'capturetool@test', 'T')`, uid); err != nil {
		t.Fatal(err)
	}
	defer d.Exec(`DELETE FROM users WHERE id = $1`, uid)
	d.Exec(`INSERT INTO fin_captures (user_id, source, sender_label, text, amount, type, description, outcome, reason)
		VALUES ($1,'sms','JD-KOTAKB-S','Sent Rs.2724.00 to RONAK',2724,'expense','Ronak','created',''),
		       ($1,'notification','Gmail','Fund transfer 2724.00',2724,'expense','NEFT','absorbed','Same UTR as the bank SMS'),
		       ($1,'notification','POP','You got ₹0.2 cashback',0,'','','ignored','Not money that has moved.')`, uid)
	count := func(args map[string]any) int {
		t.Helper()
		data, _, err := listCapturesTool(context.Background(), d, uid, args)
		if err != nil {
			t.Fatal(err)
		}
		return len(data.(map[string]any)["captures"].([]map[string]any))
	}
	if n := count(map[string]any{}); n != 3 {
		t.Fatalf("all: %d", n)
	}
	if n := count(map[string]any{"search": "2724"}); n != 2 {
		t.Fatalf("search 2724: %d", n)
	}
	if n := count(map[string]any{"outcome": "Absorbed"}); n != 1 {
		t.Fatalf("absorbed: %d", n)
	}
}
