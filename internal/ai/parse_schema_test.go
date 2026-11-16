package ai

import "testing"

func TestTxnSchemaOffersOnlyTheUsersCategories(t *testing.T) {
	sc := txnSchema(&TxnCategories{Expense: []string{"Food", "Others", " "}, Income: []string{"Salary", "Food"}}, true)
	enum := sc.Properties["category"].Enum
	want := []string{"Others", "Food", "Salary"}
	if len(enum) != len(want) {
		t.Fatalf("enum %v", enum)
	}
	for i := range want {
		if enum[i] != want[i] {
			t.Fatalf("enum %v, want %v", enum, want)
		}
	}
	if sc.Properties["own_money"] == nil {
		t.Fatal("auto parse lost own_money")
	}
	if plain := txnSchema(nil, false); plain.Properties["category"] != nil || plain.Properties["own_money"] != nil {
		t.Fatal("a plain parse asks for a category or own_money")
	}
	c := &TxnCategories{Expense: []string{"Food", "Others"}, Income: []string{"Salary"}}
	if c.Pick("expense", "food") != "Food" || c.Pick("expense", "Others") != "" || c.Pick("expense", "Salary") != "" || c.Pick("income", "Salary") != "Salary" {
		t.Fatal("Pick")
	}
}
