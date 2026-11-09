package db

import "fmt"

// SpendTypes are the transaction types that move spending: expenses and
// forgiven lends add to it, refunds take away. Filter with
// `type IN `+SpendTypes and sum Spend(alias), never raw amount.
const SpendTypes = "('expense','writeoff','refund')"

// Spend is the signed spending of a row of fin_transactions aliased alias
// ("" for an unaliased table). Pair it with the SpendTypes filter.
func Spend(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("(CASE WHEN %[1]stype = 'refund' THEN -%[1]samount ELSE %[1]samount END)", alias)
}

// MoneyInTypes and MoneyOutTypes move an account's balance up and down.
// Writeoffs, owed rows and net-offs move no money, so they're in neither.
const (
	MoneyInTypes  = "('income','refund','transfer_in','lend_repayment','borrow')"
	MoneyOutTypes = "('expense','transfer_out','lend','borrow_repayment')"
)

// BalanceDelta is a fin_transactions row's signed effect on its account's
// balance (alias "" for an unaliased table).
func BalanceDelta(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("(CASE WHEN %[1]stype IN %[2]s THEN %[1]samount WHEN %[1]stype IN %[3]s THEN -%[1]samount ELSE 0 END)",
		alias, MoneyInTypes, MoneyOutTypes)
}
