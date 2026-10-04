package ai

import "testing"

func TestMatchOption(t *testing.T) {
	opts := []string{"note", "question", "todo"}
	for reply, want := range map[string]string{
		"question":                     "question",
		"Question":                     "question",
		"Kind: question":               "question",
		"**todo**":                     "todo",
		"question - it asks something": "question",
	} {
		if got, ok := matchOption(reply, opts); !ok || got != want {
			t.Errorf("matchOption(%q) = %q, %v; want %q", reply, got, ok, want)
		}
	}
	for _, reply := range []string{"", "unsure", "questions"} {
		if got, ok := matchOption(reply, opts); ok {
			t.Errorf("matchOption(%q) = %q; want no match", reply, got)
		}
	}
}
