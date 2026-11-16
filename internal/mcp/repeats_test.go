package mcp

import (
	"strings"
	"testing"
)

func TestRepeatsRunAChangeOnce(t *testing.T) {
	var r repeats
	a := dedupeKey("u", "create_task", map[string]any{"title": "Milk", "priority": "high"})
	b := dedupeKey("u", "create_task", map[string]any{"priority": "high", "title": "Milk"})
	if a != b {
		t.Fatal("argument order changed the key")
	}
	if a == dedupeKey("u2", "create_task", map[string]any{"title": "Milk", "priority": "high"}) {
		t.Fatal("another user shares the key")
	}
	if _, again := r.claim(a); again {
		t.Fatal("first call treated as a repeat")
	}
	r.finish(a, toolText(`{"id":7}`, false))
	res, again := r.claim(a)
	if !again {
		t.Fatal("resend ran again")
	}
	c := res["content"].([]map[string]any)
	if len(c) != 2 || c[0]["text"] != `{"id":7}` || !strings.Contains(c[1]["text"].(string), "wasn't repeated") {
		t.Fatalf("repeat answer: %v", res)
	}
	// A failed change may be tried again.
	k := dedupeKey("u", "create_task", map[string]any{"title": "Eggs"})
	r.claim(k)
	r.finish(k, toolText("Error: boom", true))
	if _, again := r.claim(k); again {
		t.Fatal("a failed change blocked its retry")
	}
}
