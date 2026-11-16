package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestReminderPushText(t *testing.T) {
	cases := []struct{ msg, notes, title, body string }{
		{"Call mum", "", "Reminder", "Call mum"},
		{"Call mum", "  ask about Diwali  ", "Call mum", "ask about Diwali"},
		{strings.Repeat("long ", 10), "notes", "Reminder", strings.TrimSpace(strings.Repeat("long ", 10)) + "\n\nnotes"},
	}
	for _, c := range cases {
		title, body := reminderPushText(c.msg, c.notes)
		if title != c.title || body != c.body {
			t.Errorf("reminderPushText(%q,%q) = %q,%q; want %q,%q", c.msg, c.notes, title, body, c.title, c.body)
		}
	}
	// 4,000 bytes of Devanagari notes: cut under the cap on a rune boundary.
	_, body := reminderPushText("Pay rent", strings.Repeat("नमस्ते ", 300))
	if len(body) > reminderBodyBytes || !utf8.ValidString(body) || !strings.HasSuffix(body, "…") {
		t.Errorf("clipped body: %d bytes, valid=%v", len(body), utf8.ValidString(body))
	}
}
