package finstats

import (
	"strings"
	"testing"
	"time"
)

func TestCompute(t *testing.T) {
	ist, _ := time.LoadLocation("Asia/Kolkata")
	day := func(s string) time.Time { d, _ := time.ParseInLocation("2006-01-02", s, ist); return d }
	food, rent := int64(1), int64(2)
	var id int64
	row := func(date, typ string, amount float64, title string, cat *int64, plain bool) Row {
		id++
		name := "Others"
		if cat == &food {
			name = "Food"
		} else if cat == &rent {
			name = "Rent"
		}
		return Row{ID: id, Type: typ, Amount: amount, Title: title, Day: day(date), CategoryID: cat, Category: name, Plain: plain, Slate: map[bool]string{false: "Goa"}[plain]}
	}
	var rows []Row
	// July–September: rent ₹10,000 on the 1st, four ₹500 lunches by the 15th,
	// salary ₹50,000.
	for _, m := range []string{"2026-07", "2026-08", "2026-09"} {
		rows = append(rows, row(m+"-01", "income", 50000, "Salary", nil, true))
		rows = append(rows, row(m+"-01", "expense", 10000, "Rent", &rent, true))
		for _, d := range []string{"03", "06", "09", "12"} {
			rows = append(rows, row(m+"-"+d, "expense", 500, "Cafe X", &food, true))
		}
	}
	// October to the 16th: rent, the usual lunches, one ₹4,000 dinner, a
	// ₹2,500 first-time purchase, a ₹200 refund on Food, and a Goa trip.
	rows = append(rows,
		row("2026-10-01", "income", 50000, "Salary", nil, true),
		row("2026-10-01", "expense", 10000, "Rent", &rent, true),
		row("2026-10-03", "expense", 500, "Cafe X", &food, true),
		row("2026-10-06", "expense", 500, "cafe  x", &food, true),
		row("2026-10-08", "expense", 4000, "Fancy dinner", &food, true),
		row("2026-10-10", "expense", 2500, "Headphones", nil, true),
		row("2026-10-11", "refund", 200, "Cafe X refund", &food, true),
		row("2026-10-12", "expense", 8000, "Goa flights", nil, false),
	)
	now := time.Date(2026, 10, 16, 21, 0, 0, 0, ist)
	month, err := ParseMonth("", now)
	if err != nil {
		t.Fatal(err)
	}
	r := Compute(rows, month, now, day("2026-07-01"))

	if r.Month != "2026-10" || r.Day != 16 || !r.Current || r.BaseMonths != 3 {
		t.Fatalf("header: %+v", r)
	}
	// 10000 + 500 + 500 + 4000 + 2500 − 200 + 8000
	if r.Spent != 25300 || r.SlateSpent != 8000 || r.Income != 50000 || r.Saved != 24700 {
		t.Fatalf("totals: spent %v slate %v income %v saved %v", r.Spent, r.SlateSpent, r.Income, r.Saved)
	}
	if r.Rate == nil || *r.Rate != 49.4 {
		t.Fatalf("rate: %v", r.Rate)
	}
	// Usual by the 16th: rent + four lunches = 12,000. Plain this month: 17,300.
	if r.Usual == nil || *r.Usual != 12000 || r.Comparable != 17300 || *r.VsUsual != 5300 {
		t.Fatalf("usual: %v comparable %v vs %v", r.Usual, r.Comparable, r.VsUsual)
	}
	// Nothing more usually happens after the 16th.
	if r.Projected == nil || *r.Projected != 25300 {
		t.Fatalf("projected: %v", r.Projected)
	}
	if len(r.Cumulative) != 31 || r.Cumulative[15].This == nil || r.Cumulative[16].This != nil || *r.Cumulative[15].This != 17300 {
		t.Fatalf("cumulative: %+v", r.Cumulative[14:17])
	}

	// Food drives it: 4,800 this month to date vs 2,000 usual.
	var foodCat *Category
	for i := range r.Categories {
		if r.Categories[i].Name == "Food" {
			foodCat = &r.Categories[i]
		}
	}
	if foodCat == nil || foodCat.Spent != 4800 || *foodCat.Change != 2800 {
		t.Fatalf("food: %+v", foodCat)
	}
	// Others (headphones + the Goa flights, 10,500) outweighs Rent.
	if r.Categories[0].Name != "Others" || r.Categories[1].Name != "Rent" {
		t.Fatalf("biggest category first: %+v", r.Categories[0])
	}

	// Titles group case- and space-insensitively.
	var cafe *Merchant
	for i := range r.Merchants {
		if MerchantKey(r.Merchants[i].Name) == "cafe x" {
			cafe = &r.Merchants[i]
		}
	}
	if cafe == nil || cafe.Count != 2 || cafe.Spent != 1000 {
		t.Fatalf("merchants: %+v", r.Merchants)
	}

	if len(r.Largest) != 5 || r.Largest[0].Title != "Rent" || r.Largest[1].Slate != "Goa" {
		t.Fatalf("largest: %+v", r.Largest)
	}
	// The dinner is 8× a usual lunch; the headphones are new; the trip is a
	// slate, so it's never "unusual".
	if len(r.Unusual) != 2 || r.Unusual[0].Title != "Fancy dinner" || !strings.Contains(r.Unusual[0].Reason, "8.0×") ||
		r.Unusual[1].Title != "Headphones" || !strings.HasPrefix(r.Unusual[1].Reason, "New") {
		t.Fatalf("unusual: %+v", r.Unusual)
	}

	if len(r.Trend) != 4 || r.Trend[0].Month != "2026-07" || r.Trend[0].Spent != 12000 || r.Trend[3].Spent != 25300 {
		t.Fatalf("trend: %+v", r.Trend)
	}

	// A past month counts all its days and has no projection.
	sep, _ := ParseMonth("2026-09", now)
	p := Compute(rows, sep, now, day("2026-07-01"))
	if p.Current || p.Day != 30 || p.Projected != nil || p.BaseMonths != 2 || p.Spent != 12000 {
		t.Fatalf("past month: %+v", p)
	}
	// A first month: no usual, but a big one-off still stands out.
	oct := []Row{
		row("2026-10-02", "expense", 3669, "Cloudflare Domain", nil, true),
		row("2026-10-03", "expense", 400, "Lunch", &food, true),
		row("2026-10-04", "expense", 9000, "Rent", &rent, true),
		row("2026-10-05", "expense", 500, "Lunch", &food, true),
	}
	f := Compute(oct, month, now, day("2026-10-02"))
	if f.BaseMonths != 0 || f.Usual != nil || len(f.Trend) != 1 || len(f.Unusual) != 2 ||
		f.Unusual[0].Title != "Rent" || !strings.HasPrefix(f.Unusual[0].Reason, "Big one-off: 66%") {
		t.Fatalf("first month: %+v", f)
	}
	if _, err := ParseMonth("2026-11", now); err == nil {
		t.Fatal("a future month should be refused")
	}
}

func TestMoney(t *testing.T) {
	for in, want := range map[float64]string{0: "0.00", 999: "999.00", 1000.5: "1,000.50", 125000: "1,25,000.00", 12345678.9: "1,23,45,678.90", -42.25: "-42.25"} {
		if got := money(in); got != want {
			t.Fatalf("money(%v) = %s, want %s", in, got, want)
		}
	}
}
