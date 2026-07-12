package api

import "testing"

func TestCategoryNameKey(t *testing.T) {
	cases := map[string]string{
		" Groceries ": "groceries",
		"GROCERIES":   "groceries",
		"Other":       "others",
		" others ":    "others",
	}
	for input, want := range cases {
		if got := categoryNameKey(input); got != want {
			t.Errorf("categoryNameKey(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestDefaultCategoryNamesAreKindSpecificAndCaseInsensitive(t *testing.T) {
	if !isDefaultCategoryName("expense", " food & dining ") {
		t.Error("expense default not recognized case-insensitively")
	}
	if !isDefaultCategoryName("income", "SALARY") {
		t.Error("income default not recognized case-insensitively")
	}
	if !isDefaultCategoryName("income", "other") || !isDefaultCategoryName("expense", "OTHERS") {
		t.Error("Other/Others alias should match both default kinds")
	}
	if isDefaultCategoryName("income", "Food & Dining") {
		t.Error("expense default must remain valid as a distinct income category")
	}
}
