package ai

import "testing"

// cleanTxnMessage strips the fraud-report URL and "Not you …" tail banks append
// — pure token saving with no transaction data lost. Uses the real-world Kotak
// SMS from the bug report.
func TestCleanTxnMessage(t *testing.T) {
	in := "Sent Rs.20.00 from Kotak Bank AC X2851 to paytmqr1ayfzccx02@paytm on 02-06-26.UPI Ref 651916230149. Not you, https://kotak.com/KBANKT/Fraud"
	out := cleanTxnMessage(in)

	// The data the parser needs survives.
	for _, keep := range []string{"Rs.20.00", "Kotak", "X2851", "02-06-26", "651916230149"} {
		if !contains(out, keep) {
			t.Errorf("cleanTxnMessage dropped %q\n got: %q", keep, out)
		}
	}
	// The junk is gone.
	for _, gone := range []string{"http", "Not you", "Fraud"} {
		if contains(out, gone) {
			t.Errorf("cleanTxnMessage kept junk %q\n got: %q", gone, out)
		}
	}
}

// Stripping must never empty the message — the model always needs something.
func TestCleanTxnMessageNeverEmpties(t *testing.T) {
	in := "Not you? https://bank.example/fraud"
	if out := cleanTxnMessage(in); out == "" {
		t.Fatalf("cleanTxnMessage emptied the text; want original fallback")
	}
}

// The HH:MM gate keeps only well-formed 24h times; everything else becomes ""
// so the handler falls back to the current time.
func TestClockHHM(t *testing.T) {
	ok := []string{"00:00", "9:05", "14:30", "23:59"}
	bad := []string{"", "24:00", "12:60", "2:5", "14.30", "noon", "1430"}
	for _, s := range ok {
		if !reClockHHM.MatchString(s) {
			t.Errorf("reClockHHM rejected valid %q", s)
		}
	}
	for _, s := range bad {
		if reClockHHM.MatchString(s) {
			t.Errorf("reClockHHM accepted invalid %q", s)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
