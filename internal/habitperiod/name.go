package habitperiod

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// NameMax caps habit names so one always fits a single pill (web roster,
// Android rail, home-screen widget). Clients enforce it too.
const NameMax = 20

// CleanName trims a habit name and rejects empty or over-long ones.
func CleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("Give the habit a name.")
	}
	if utf8.RuneCountInString(name) > NameMax {
		return "", fmt.Errorf("Habit names can be at most %d characters.", NameMax)
	}
	return name, nil
}
