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
