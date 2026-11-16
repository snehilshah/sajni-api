package ai

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every write tool's meta kind must read as a plain label, and start with a
// prefix both clients route to a refresh (web InvalidateBridge ROUTES,
// Android OnDataInvalidate).
func TestMetaKindsHaveLabelsAndRoutes(t *testing.T) {
	routed := []string{"task_", "reminder_", "plan_", "habit_", "event_", "memo_", "note_", "folder_",
		"media_", "journal_", "bookmark_", "transaction_", "lend_", "biller_", "account_", "slate_",
		"investment_", "budget_", "insight_", "theme_", "thinking_", "capture_", "avatar_", "owed_"}
	files, _ := filepath.Glob("*.go")
	lit := regexp.MustCompile(`"kind":\s+"([a-z_]+)"`)
	dyn := regexp.MustCompile(`kind\s*:?=\s*"([a-z]+_[a-z_]+)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, re := range []*regexp.Regexp{lit, dyn} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				seen[m[1]] = true
			}
		}
	}
	if len(seen) < 40 {
		t.Fatalf("found only %d kinds; the scan is broken", len(seen))
	}
	for kind := range seen {
		label := KindLabel(kind)
		if strings.Contains(label, "_") || label == strings.ReplaceAll(kind, "_", " ") {
			t.Errorf("%s has no readable label (%q)", kind, label)
		}
		ok := false
		for _, p := range routed {
			ok = ok || strings.HasPrefix(kind, p)
		}
		if !ok {
			t.Errorf("%s matches no client refresh prefix", kind)
		}
	}
}
