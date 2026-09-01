package api

import (
	"fmt"
	"math"
	"time"

	"sajni/internal/db"
)

// investmentValuation contains the fields needed to value an investment.
// RD and FD values are estimates derived from their fixed rate; market-linked
// and legacy instruments keep the value reported by the user.
type investmentValuation struct {
	ID             int64
	Name           string
	Type           string
	InvestedAmount float64
	CurrentValue   float64
	CycleAmount    float64
	Frequency      string
	StartDate      *string
	MaturityDate   *string
	ExpectedReturn float64
}

type investmentBreakdown struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Amount float64 `json:"amount"`
}

func effectiveInvestmentValue(i investmentValuation, now time.Time) float64 {
	if i.Type != "rd" && i.Type != "fd" {
		return roundMoney(i.CurrentValue)
	}
	if i.InvestedAmount <= 0 || i.ExpectedReturn <= 0 || i.StartDate == nil || *i.StartDate == "" {
		return roundMoney(i.InvestedAmount)
	}

	start, err := time.Parse("2006-01-02", *i.StartDate)
	if err != nil {
		return roundMoney(i.InvestedAmount)
	}
	asOf := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if i.MaturityDate != nil && *i.MaturityDate != "" {
		if maturity, err := time.Parse("2006-01-02", *i.MaturityDate); err == nil && maturity.Before(asOf) {
			asOf = maturity
		}
	}
	if !asOf.After(start) {
		return roundMoney(i.InvestedAmount)
	}

	rate := i.ExpectedReturn / 100
	if i.Type == "fd" || i.CycleAmount <= 0 {
		return roundMoney(compoundDeposit(i.InvestedAmount, rate, start, asOf))
	}

	// Treat the recorded invested amount as authoritative. Apply interest to
	// each scheduled contribution from the start date; any unmatched balance
	// is preserved without inventing a contribution date for it.
	remaining := i.InvestedAmount
	value := 0.0
	due := start
	anchorDay := start.Day()
	for remaining > 0 && !due.After(asOf) {
		deposit := math.Min(i.CycleAmount, remaining)
		value += compoundDeposit(deposit, rate, due, asOf)
		remaining -= deposit
		due = advanceDebitDate(due, i.Frequency, anchorDay)
	}
	return roundMoney(value + remaining)
}

// compoundDeposit follows the usual cumulative bank-deposit shape: completed
// quarters compound, then the remaining broken period accrues simple daily
// interest on the compounded balance.
func compoundDeposit(principal, annualRate float64, from, to time.Time) float64 {
	if principal <= 0 || annualRate <= 0 || !to.After(from) {
		return principal
	}
	value := principal
	cursor := from
	anchorDay := from.Day()
	for {
		next := advanceRecurringMonth(cursor, 3, anchorDay)
		if next.After(to) {
			break
		}
		value *= 1 + annualRate/4
		cursor = next
	}
	days := to.Sub(cursor).Hours() / 24
	if days > 0 {
		value *= 1 + annualRate*days/365
	}
	return value
}

func roundMoney(value float64) float64 {
	return math.Round(value*100) / 100
}

func loadInvestmentValues(d *db.DB, uid string, now time.Time) (float64, []investmentBreakdown, error) {
	rows, err := d.Query(`SELECT id, name, type, invested_amount, current_value, monthly_amount,
		frequency, start_date::text, maturity_date::text, expected_return
		FROM fin_investments WHERE user_id = $1 ORDER BY created_at ASC`, uid)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	total := 0.0
	breakdown := []investmentBreakdown{}
	for rows.Next() {
		var i investmentValuation
		if err := rows.Scan(&i.ID, &i.Name, &i.Type, &i.InvestedAmount, &i.CurrentValue, &i.CycleAmount,
			&i.Frequency, &i.StartDate, &i.MaturityDate, &i.ExpectedReturn); err != nil {
			return 0, nil, fmt.Errorf("scan investment value: %w", err)
		}
		amount := effectiveInvestmentValue(i, now)
		total += amount
		breakdown = append(breakdown, investmentBreakdown{
			ID: i.ID, Name: i.Name, Type: i.Type, Amount: amount,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, nil, fmt.Errorf("iterate investment values: %w", err)
	}
	return roundMoney(total), breakdown, nil
}

func refreshStoredFixedInvestmentValue(d *db.DB, uid string, id int64, now time.Time) error {
	var i investmentValuation
	i.ID = id
	if err := d.QueryRow(`SELECT name, type, invested_amount, current_value, monthly_amount,
		frequency, start_date::text, maturity_date::text, expected_return
		FROM fin_investments WHERE id = $1 AND user_id = $2`, id, uid).Scan(
		&i.Name, &i.Type, &i.InvestedAmount, &i.CurrentValue, &i.CycleAmount,
		&i.Frequency, &i.StartDate, &i.MaturityDate, &i.ExpectedReturn,
	); err != nil {
		return err
	}
	if i.Type != "rd" && i.Type != "fd" {
		return nil
	}
	_, err := d.Exec(`UPDATE fin_investments SET current_value = $1 WHERE id = $2 AND user_id = $3`,
		effectiveInvestmentValue(i, now), id, uid)
	return err
}
