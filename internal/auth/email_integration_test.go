package auth

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sajni/internal/db"
)

func TestEmailCodeConcurrentConsumption(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	database, err := db.New(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`DELETE FROM email_codes`); err != nil {
		t.Fatal(err)
	}
	service := &Service{DB: database}
	ctx := context.Background()

	insert := func(id, email, code string) {
		t.Helper()
		if _, err := database.Exec(`INSERT INTO email_codes(id,email,code_hash,expires_at) VALUES($1,$2,$3,$4)`, id, email, hashCode(code), time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	insert("00000000-0000-7000-8000-000000000001", "success@example.com", "123456")
	var successes atomic.Int32
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := service.consumeEmailCode(ctx, "success@example.com", "123456"); err == nil {
				successes.Add(1)
			}
		}()
	}
	group.Wait()
	if got := successes.Load(); got != 1 {
		t.Fatalf("successful consumptions = %d, want 1", got)
	}

	insert("00000000-0000-7000-8000-000000000002", "failure@example.com", "654321")
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = service.consumeEmailCode(ctx, "failure@example.com", "000000")
		}()
	}
	group.Wait()
	var attempts int
	if err := database.QueryRow(`SELECT attempts FROM email_codes WHERE email=$1`, "failure@example.com").Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != emailCodeAttempts {
		t.Fatalf("attempts = %d, want %d", attempts, emailCodeAttempts)
	}
}
