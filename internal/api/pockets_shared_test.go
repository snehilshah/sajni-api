package api

import (
	"testing"
)

type shareDraft = struct {
	MemberID int64   `json:"member_id"`
	Amount   float64 `json:"amount"`
}

func TestResolveSharesEqualRemainder(t *testing.T) {
	// ₹100 over 3: 33.34 + 33.33 + 33.33, remainder paise to lowest ids.
	shares, msg := resolveShares("equal", 10000, []shareDraft{{MemberID: 3}, {MemberID: 1}, {MemberID: 2}})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	want := map[int64]int64{1: 3334, 2: 3333, 3: 3333}
	var sum int64
	for id, p := range shares {
		if p != want[id] {
			t.Errorf("member %d: got %d want %d", id, p, want[id])
		}
		sum += p
	}
	if sum != 10000 {
		t.Errorf("shares sum %d, want 10000", sum)
	}
}

func TestResolveSharesExact(t *testing.T) {
	if _, msg := resolveShares("exact", 10000, []shareDraft{{1, 60}, {2, 39.99}}); msg == "" {
		t.Error("bad sum accepted")
	}
	shares, msg := resolveShares("exact", 10000, []shareDraft{{1, 60}, {2, 40}})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if shares[1] != 6000 || shares[2] != 4000 {
		t.Errorf("got %v", shares)
	}
	if _, msg := resolveShares("exact", 10000, []shareDraft{{1, 100}, {1, 0}}); msg == "" {
		t.Error("duplicate participant accepted")
	}
	if _, msg := resolveShares("equal", 10000, nil); msg == "" {
		t.Error("empty participants accepted")
	}
}

func TestSimplifySettlements(t *testing.T) {
	// A(+150) owed by B(-150): one transfer.
	got := simplifySettlements(map[int64]int64{1: 15000, 2: -15000})
	if len(got) != 1 || got[0].FromMember != 2 || got[0].ToMember != 1 || got[0].Amount != 150 {
		t.Errorf("simple pair: %+v", got)
	}

	// Chain collapse: A+100, B-40, C-60 → two transfers, largest debtor first.
	got = simplifySettlements(map[int64]int64{1: 10000, 2: -4000, 3: -6000})
	if len(got) != 2 {
		t.Fatalf("chain: want 2 transfers, got %+v", got)
	}
	if got[0].FromMember != 3 || got[0].Amount != 60 || got[1].FromMember != 2 || got[1].Amount != 40 {
		t.Errorf("chain order: %+v", got)
	}

	// Settled pocket → nothing to do.
	if got = simplifySettlements(map[int64]int64{1: 0, 2: 0}); len(got) != 0 {
		t.Errorf("settled: %+v", got)
	}

	// n−1 bound: 4 members, 3 transfers max.
	got = simplifySettlements(map[int64]int64{1: 5000, 2: 5000, 3: -7000, 4: -3000})
	if len(got) > 3 {
		t.Errorf("bound: %d transfers", len(got))
	}
	var sum int64
	for _, s := range got {
		sum += paise(s.Amount)
	}
	if sum != 10000 {
		t.Errorf("transfers move %d paise, want 10000", sum)
	}
}
