// Package finstats answers "where did this month's money go, and why is it
// so high?" for Finance → Analytics and the AI's finance_month tool, so both
// read the same figures.
//
// Spending is the app's one definition (db.SpendTypes / db.Spend): expenses
// and forgives add, refunds take away; a split bill counts only the user's
// part and lends are never spending. Totals count every slate, but
// comparisons with "usual" (the average of up to three earlier months, to
// the same day) and the unusual-expense check use Plain only, so a trip
// month doesn't make normal months look cheap.
package finstats

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"sajni/internal/db"
)

// Row is one income or spending transaction in the user's timezone.
type Row struct {
	ID         int64
	Type       string // expense | writeoff | refund | income
	Amount     float64
	Title      string
	Day        time.Time // local midnight
	CategoryID *int64
	Category   string
	Color      string
	Plain      bool
	Slate      string
}

// Spend is the row's signed spending (0 for income).
func (r Row) Spend() float64 {
	switch r.Type {
	case "expense", "writeoff":
		return r.Amount
	case "refund":
		return -r.Amount
	}
	return 0
}

type Point struct {
	Day   int      `json:"day"`
	This  *float64 `json:"this"` // nil after today
	Last  float64  `json:"last"`
	Usual *float64 `json:"usual"` // nil without earlier months
}

type DaySpend struct {
	Day   int     `json:"day"`
	Spent float64 `json:"spent"`
}

type Category struct {
	ID    *int64   `json:"id"`
	Name  string   `json:"name"`
	Color string   `json:"color"`
	Spent float64  `json:"spent"`
	Count int      `json:"count"`
	Share float64  `json:"share"` // % of the month's spending
	Usual *float64 `json:"usual"` // Plain, to the same day
	// Change is Plain spending to date minus Usual: what drives "why it's high".
	Change *float64 `json:"change"`
}

type Merchant struct {
	Name  string  `json:"name"`
	Spent float64 `json:"spent"`
	Count int     `json:"count"`
	Avg   float64 `json:"avg"`
}

type Txn struct {
	ID       int64   `json:"id"`
	Title    string  `json:"title"`
	Amount   float64 `json:"amount"`
	Date     string  `json:"date"`
	Category string  `json:"category"`
	Color    string  `json:"color"`
	Slate    string  `json:"slate,omitempty"` // only when not Plain
	Reason   string  `json:"reason,omitempty"`
}

type Month struct {
	Month  string   `json:"month"` // YYYY-MM
	Income float64  `json:"income"`
	Spent  float64  `json:"spent"`
	Saved  float64  `json:"saved"`
	Rate   *float64 `json:"rate"` // saved / income, %; nil without income
}

type Report struct {
	Month       string  `json:"month"`
	DaysInMonth int     `json:"days_in_month"`
	Day         int     `json:"day"` // days counted: today, or the whole month once it's over
	Current     bool    `json:"current"`
	Spent       float64 `json:"spent"`
	SlateSpent  float64 `json:"slate_spent"` // part of Spent outside Plain
	Income      float64 `json:"income"`
	Saved       float64 `json:"saved"`
	// Rate is Saved / Income in %; nil without income.
	Rate     *float64 `json:"rate"`
	DailyAvg float64  `json:"daily_avg"`
	// BaseMonths is how many earlier months "usual" averages (0–3).
	BaseMonths int `json:"base_months"`
	// Comparable is Plain spending to Day; Usual the same for earlier months.
	Comparable float64  `json:"comparable"`
	Usual      *float64 `json:"usual"`
	VsUsual    *float64 `json:"vs_usual"`
	LastMonth  float64  `json:"last_month"` // Plain, to the same day
	Projected  *float64 `json:"projected"`  // month end, current month only

	Cumulative []Point    `json:"cumulative"`
	Daily      []DaySpend `json:"daily"`
	Categories []Category `json:"categories"`
	Merchants  []Merchant `json:"merchants"`
	Largest    []Txn      `json:"largest"`
	Unusual    []Txn      `json:"unusual"`
	Trend      []Month    `json:"trend"`
}

const (
	baseMonths   = 3
	trendMonths  = 12
	historyMonth = 6 // per-category history for the unusual check
)

// ParseMonth reads "YYYY-MM" ("" = the month of now) in loc. A future month
// is an error.
func ParseMonth(s string, now time.Time) (time.Time, error) {
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if s == "" {
		return cur, nil
	}
	m, err := time.ParseInLocation("2006-01", s, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("month must be YYYY-MM")
	}
	if m.After(cur) {
		return time.Time{}, fmt.Errorf("that month hasn't happened yet")
	}
	return m, nil
}

// Load reads the rows Compute needs: the month and the trend window before it.
func Load(ctx context.Context, d *db.DB, uid string, month time.Time) ([]Row, time.Time, error) {
	loc := month.Location()
	from := month.AddDate(0, -(trendMonths - 1), 0)
	to := month.AddDate(0, 1, 0)
	rows, err := d.QueryContext(ctx, `SELECT t.id, t.type, t.amount, t.description,
			(t.txn_at AT TIME ZONE $4)::date::text, t.category_id, COALESCE(c.name, 'Others'),
			COALESCE(c.color, '#6B7280'), COALESCE(s.is_plain, TRUE), COALESCE(s.name, '')
		FROM fin_transactions t
		LEFT JOIN fin_categories c ON c.id = t.category_id
		LEFT JOIN fin_slates s ON s.id = t.slate_id
		WHERE t.user_id = $1 AND t.type IN ('income','expense','writeoff','refund')
		  AND (t.txn_at AT TIME ZONE $4)::date >= $2::date AND (t.txn_at AT TIME ZONE $4)::date < $3::date
		ORDER BY t.txn_at, t.id`, uid, from.Format("2006-01-02"), to.Format("2006-01-02"), loc.String())
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		var day string
		if err := rows.Scan(&r.ID, &r.Type, &r.Amount, &r.Title, &day, &r.CategoryID, &r.Category, &r.Color, &r.Plain, &r.Slate); err != nil {
			return nil, time.Time{}, err
		}
		r.Day, _ = time.ParseInLocation("2006-01-02", day, loc)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, err
	}
	// The first day the user recorded anything bounds "usual": months before
	// they started aren't months of ₹0.
	var first time.Time
	var firstDay *string
	if err := d.QueryRowContext(ctx, `SELECT MIN((txn_at AT TIME ZONE $2)::date)::text FROM fin_transactions WHERE user_id = $1`, uid, loc.String()).Scan(&firstDay); err != nil {
		return nil, time.Time{}, err
	}
	if firstDay != nil {
		first, _ = time.ParseInLocation("2006-01-02", *firstDay, loc)
	}
	return out, first, nil
}

func monthKey(t time.Time) string { return t.Format("2006-01") }

func round(v float64) float64 { return math.Round(v*100) / 100 }

func ptr(v float64) *float64 { r := round(v); return &r }

func daysIn(m time.Time) int { return m.AddDate(0, 1, -1).Day() }

// started: the user had begun recording by the end of month m.
func started(m, first time.Time) bool { return first.IsZero() || m.AddDate(0, 1, 0).After(first) }

// MerchantKey groups a transaction title: case and spacing don't matter.
func MerchantKey(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}

// Compute builds the report for month (local first day) from rows. now is
// the user's clock; first is the day they began recording (zero = unknown).
func Compute(rows []Row, month, now, first time.Time) Report {
	loc := month.Location()
	cur := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	rep := Report{Month: monthKey(month), DaysInMonth: daysIn(month), Current: month.Equal(cur)}
	rep.Day = rep.DaysInMonth
	if rep.Current {
		rep.Day = now.Day()
	}
	D := rep.Day

	// Earlier months that "usual" averages: up to three, none before the
	// user's first record.
	var base []time.Time
	for i := 1; i <= baseMonths; i++ {
		m := month.AddDate(0, -i, 0)
		if !started(m, first) {
			break
		}
		base = append(base, m)
	}
	rep.BaseMonths = len(base)
	inBase := map[string]bool{}
	for _, m := range base {
		inBase[monthKey(m)] = true
	}
	lastKey := monthKey(month.AddDate(0, -1, 0))
	thisKey := rep.Month

	// Plain spending by month and day, for cumulative lines and "usual".
	plainByDay := map[string][]float64{} // month → [day]spend
	dayArr := func(k string, m time.Time) []float64 {
		if a, ok := plainByDay[k]; ok {
			return a
		}
		a := make([]float64, daysIn(m)+1)
		plainByDay[k] = a
		return a
	}
	dayArr(thisKey, month)
	dayArr(lastKey, month.AddDate(0, -1, 0))
	for _, m := range base {
		dayArr(monthKey(m), m)
	}

	type catAgg struct {
		c        Category
		plainToD float64
		baseToD  float64
	}
	cats := map[string]*catAgg{}
	catKey := func(r Row) string {
		if r.CategoryID == nil {
			return "none"
		}
		return fmt.Sprint(*r.CategoryID)
	}
	trend := map[string]*Month{}
	merchants := map[string]*Merchant{}
	daily := make([]float64, rep.DaysInMonth+1)
	var thisRows []Row
	// Per category, Plain expense sizes in the months before this one, and
	// the titles seen before (first-time spends).
	histStart := month.AddDate(0, -historyMonth, 0)
	sizes := map[string][]float64{}
	seenBefore := map[string]bool{}

	for _, r := range rows {
		k := monthKey(r.Day)
		mStart := time.Date(r.Day.Year(), r.Day.Month(), 1, 0, 0, 0, 0, loc)
		tm := trend[k]
		if tm == nil {
			tm = &Month{Month: k}
			trend[k] = tm
		}
		if r.Type == "income" {
			tm.Income += r.Amount
		} else {
			tm.Spent += r.Spend()
		}
		if r.Type == "income" {
			if k == thisKey {
				rep.Income += r.Amount
			}
			continue
		}
		s := r.Spend()
		if r.Plain {
			if a, ok := plainByDay[k]; ok && r.Day.Day() < len(a) {
				a[r.Day.Day()] += s
			}
		}
		ck := catKey(r)
		if k == thisKey {
			thisRows = append(thisRows, r)
			rep.Spent += s
			if !r.Plain {
				rep.SlateSpent += s
			}
			daily[r.Day.Day()] += s
			ca := cats[ck]
			if ca == nil {
				ca = &catAgg{c: Category{ID: r.CategoryID, Name: r.Category, Color: r.Color}}
				cats[ck] = ca
			}
			ca.c.Spent += s
			if r.Type != "refund" {
				ca.c.Count++
			}
			if r.Plain && r.Day.Day() <= D {
				ca.plainToD += s
			}
			if r.Type == "expense" {
				mk := MerchantKey(r.Title)
				m := merchants[mk]
				if m == nil {
					m = &Merchant{}
					merchants[mk] = m
				}
				m.Name = strings.TrimSpace(r.Title) // latest spelling wins
				m.Spent += r.Amount
				m.Count++
			}
			continue
		}
		if mStart.Before(month) {
			if inBase[k] && r.Plain && r.Day.Day() <= D {
				ca := cats[ck]
				if ca == nil {
					ca = &catAgg{c: Category{ID: r.CategoryID, Name: r.Category, Color: r.Color}}
					cats[ck] = ca
				}
				ca.baseToD += s
			}
			if r.Type == "expense" {
				seenBefore[MerchantKey(r.Title)] = true
				if r.Plain && !r.Day.Before(histStart) {
					sizes[ck] = append(sizes[ck], r.Amount)
				}
			}
		}
	}

	rep.Spent = round(rep.Spent)
	rep.SlateSpent = round(rep.SlateSpent)
	rep.Income = round(rep.Income)
	rep.Saved = round(rep.Income - rep.Spent)
	if rep.Income > 0 {
		rep.Rate = ptr(rep.Saved / rep.Income * 100)
	}
	if D > 0 {
		rep.DailyAvg = round(rep.Spent / float64(D))
	}

	// Cumulative Plain lines: this month to today, last month, usual.
	cum := func(a []float64, upto int) []float64 {
		out := make([]float64, rep.DaysInMonth+1)
		run := 0.0
		for d := 1; d <= rep.DaysInMonth; d++ {
			if d < len(a) && d <= upto {
				run += a[d]
			}
			out[d] = run
		}
		return out
	}
	thisCum := cum(plainByDay[thisKey], D)
	lastCum := cum(plainByDay[lastKey], rep.DaysInMonth)
	usualCum := make([]float64, rep.DaysInMonth+1)
	usualFull := 0.0
	for _, m := range base {
		c := cum(plainByDay[monthKey(m)], rep.DaysInMonth)
		for d := 1; d <= rep.DaysInMonth; d++ {
			usualCum[d] += c[d] / float64(len(base))
		}
		full := 0.0
		for _, v := range plainByDay[monthKey(m)] {
			full += v
		}
		usualFull += full / float64(len(base))
	}
	for d := 1; d <= rep.DaysInMonth; d++ {
		p := Point{Day: d, Last: round(lastCum[d])}
		if d <= D {
			p.This = ptr(thisCum[d])
		}
		if len(base) > 0 {
			p.Usual = ptr(usualCum[d])
		}
		rep.Cumulative = append(rep.Cumulative, p)
	}
	rep.Comparable = round(thisCum[D])
	rep.LastMonth = round(lastCum[min(D, rep.DaysInMonth)])
	if len(base) > 0 {
		rep.Usual = ptr(usualCum[D])
		rep.VsUsual = ptr(thisCum[D] - usualCum[D])
	}
	if rep.Current {
		var proj float64
		if len(base) > 0 {
			proj = rep.Spent + math.Max(0, usualFull-usualCum[D])
		} else if D > 0 {
			proj = rep.Spent / float64(D) * float64(rep.DaysInMonth)
		}
		rep.Projected = ptr(proj)
	}
	for d := 1; d <= D; d++ {
		rep.Daily = append(rep.Daily, DaySpend{Day: d, Spent: round(daily[d])})
	}

	// Categories, biggest first; "usual" only when there are earlier months.
	for _, ca := range cats {
		if ca.c.Count == 0 && ca.c.Spent == 0 && ca.baseToD == 0 {
			continue
		}
		c := ca.c
		c.Spent = round(c.Spent)
		if rep.Spent > 0 {
			c.Share = round(c.Spent / rep.Spent * 100)
		}
		if len(base) > 0 {
			c.Usual = ptr(ca.baseToD / float64(len(base)))
			c.Change = ptr(ca.plainToD - ca.baseToD/float64(len(base)))
		}
		rep.Categories = append(rep.Categories, c)
	}
	sort.Slice(rep.Categories, func(i, j int) bool {
		if rep.Categories[i].Spent != rep.Categories[j].Spent {
			return rep.Categories[i].Spent > rep.Categories[j].Spent
		}
		return rep.Categories[i].Name < rep.Categories[j].Name
	})

	for _, m := range merchants {
		m.Spent = round(m.Spent)
		m.Avg = round(m.Spent / float64(m.Count))
		rep.Merchants = append(rep.Merchants, *m)
	}
	sort.Slice(rep.Merchants, func(i, j int) bool {
		if rep.Merchants[i].Spent != rep.Merchants[j].Spent {
			return rep.Merchants[i].Spent > rep.Merchants[j].Spent
		}
		return rep.Merchants[i].Name < rep.Merchants[j].Name
	})
	if len(rep.Merchants) > 8 {
		rep.Merchants = rep.Merchants[:8]
	}

	toTxn := func(r Row) Txn {
		t := Txn{ID: r.ID, Title: r.Title, Amount: round(r.Amount), Date: r.Day.Format("2006-01-02"), Category: r.Category, Color: r.Color}
		if !r.Plain {
			t.Slate = r.Slate
		}
		return t
	}
	spends := make([]Row, 0, len(thisRows))
	for _, r := range thisRows {
		if r.Type == "expense" || r.Type == "writeoff" {
			spends = append(spends, r)
		}
	}
	sort.SliceStable(spends, func(i, j int) bool { return spends[i].Amount > spends[j].Amount })
	for _, r := range spends[:min(5, len(spends))] {
		rep.Largest = append(rep.Largest, toTxn(r))
	}

	// Unusual: a Plain expense well above its category's typical size, or a
	// big spend somewhere new. Floors keep small change out of it.
	newFloor := math.Max(1000, 0.05*usualFull)
	seenThisMonth := map[string]bool{}
	for _, r := range spends {
		if r.Type != "expense" || !r.Plain {
			continue
		}
		mk := MerchantKey(r.Title)
		reason := ""
		if h := sizes[catKey(r)]; len(h) >= 4 {
			med := median(h)
			if med > 0 && r.Amount >= 2.5*med && r.Amount-med >= 300 {
				reason = fmt.Sprintf("%.1f× your usual %s spend (₹%s)", r.Amount/med, r.Category, money(med))
			}
		}
		// Without enough history for the category, a single spend that is a
		// big slice of the month, and bigger than anything like it before,
		// still stands out (a yearly domain renewal, an appliance).
		if h := sizes[catKey(r)]; reason == "" && len(h) < 4 && r.Amount >= 1000 && rep.Spent > 0 && r.Amount >= 0.15*rep.Spent {
			similar := false
			for _, x := range h {
				similar = similar || x >= 0.5*r.Amount
			}
			if !similar {
				reason = fmt.Sprintf("Big one-off: %.0f%% of this month's spending", r.Amount/rep.Spent*100)
			}
		}
		// "New" needs a month of history before this one to mean anything.
		if reason == "" && !first.IsZero() && month.AddDate(0, -1, 0).After(first) &&
			!seenBefore[mk] && !seenThisMonth[mk] && r.Amount >= newFloor {
			reason = "New: nothing like it in the last year"
		}
		seenThisMonth[mk] = true
		if reason != "" {
			t := toTxn(r)
			t.Reason = reason
			rep.Unusual = append(rep.Unusual, t)
		}
	}
	if len(rep.Unusual) > 5 {
		rep.Unusual = rep.Unusual[:5]
	}

	// Trend: the twelve months ending this one, from the first with data.
	for i := trendMonths - 1; i >= 0; i-- {
		m := month.AddDate(0, -i, 0)
		if !started(m, first) {
			continue
		}
		tm := trend[monthKey(m)]
		if tm == nil {
			tm = &Month{Month: monthKey(m)}
		}
		out := Month{Month: tm.Month, Income: round(tm.Income), Spent: round(tm.Spent)}
		out.Saved = round(out.Income - out.Spent)
		if out.Income > 0 {
			out.Rate = ptr(out.Saved / out.Income * 100)
		}
		rep.Trend = append(rep.Trend, out)
	}

	if rep.Categories == nil {
		rep.Categories = []Category{}
	}
	if rep.Merchants == nil {
		rep.Merchants = []Merchant{}
	}
	if rep.Largest == nil {
		rep.Largest = []Txn{}
	}
	if rep.Unusual == nil {
		rep.Unusual = []Txn{}
	}
	if rep.Daily == nil {
		rep.Daily = []DaySpend{}
	}
	return rep
}

func median(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// money formats a figure the app's way: Indian grouping, two decimals
// (1234.5 → "1,234.50"), never rounded to whole rupees.
func money(v float64) string {
	paise := int64(math.Round(math.Abs(v) * 100))
	s := fmt.Sprint(paise / 100)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	s = fmt.Sprintf("%s.%02d", s, paise%100)
	if v < 0 && paise > 0 {
		s = "-" + s
	}
	return s
}
