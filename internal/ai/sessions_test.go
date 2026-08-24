package ai

import (
	"reflect"
	"testing"

	"google.golang.org/genai"
)

func TestIsPartValid(t *testing.T) {
	tests := []struct {
		name string
		part *genai.Part
		want bool
	}{
		{"nil part", nil, false},
		{"empty part", &genai.Part{}, false},
		{"thought only without text", &genai.Part{Thought: true}, false},
		{"thought with text", &genai.Part{Thought: true, Text: "reasoning"}, false},
		{"thought signature only", &genai.Part{ThoughtSignature: []byte("sig")}, false},
		{"valid text", &genai.Part{Text: "hello"}, true},
		{"valid function call", &genai.Part{FunctionCall: &genai.FunctionCall{Name: "list_tasks"}}, true},
		{"valid function response", &genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "list_tasks", Response: map[string]any{"ok": true}}}, true},
		{"valid inline data", &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("abc")}}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPartValid(tt.part); got != tt.want {
				t.Errorf("isPartValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSanitizeHistory(t *testing.T) {
	t.Run("empty and nil", func(t *testing.T) {
		if got := SanitizeHistory(nil); got != nil && len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
		if got := SanitizeHistory([]*genai.Content{}); len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
		if got := SanitizeHistory([]*genai.Content{nil, {Role: "user", Parts: nil}}); len(got) != 0 {
			t.Errorf("expected empty, got %v", got)
		}
	})

	t.Run("filters thought and empty parts from stream bug", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					{Text: "hello"},
				},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "get_context"}},
					{Thought: true, Text: ""},
					{ThoughtSignature: []byte("sig")}, // this was causing the 400 error in contents[1].parts[2]
					{},
				},
			},
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "get_context", Response: map[string]any{"ok": true}}},
				},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "chunk 1 "},
					{Text: "chunk 2"},
					{Thought: true, Text: "internal reasoning"},
				},
			},
		}

		out := SanitizeHistory(in)
		if len(out) != 4 {
			t.Fatalf("expected 4 contents, got %d", len(out))
		}

		// Model round 1 should only have the function call part
		if len(out[1].Parts) != 1 {
			t.Fatalf("expected 1 part in content 1, got %d", len(out[1].Parts))
		}
		if out[1].Parts[0].FunctionCall == nil || out[1].Parts[0].FunctionCall.Name != "get_context" {
			t.Errorf("expected FunctionCall get_context, got %v", out[1].Parts[0])
		}

		// Model round 2 should have merged text and dropped thought
		if len(out[3].Parts) != 1 {
			t.Fatalf("expected 1 part in content 3, got %d", len(out[3].Parts))
		}
		if out[3].Parts[0].Text != "chunk 1 chunk 2" {
			t.Errorf("expected merged text 'chunk 1 chunk 2', got %q", out[3].Parts[0].Text)
		}
	})

	t.Run("strips orphan function response at head", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					{FunctionResponse: &genai.FunctionResponse{Name: "list_tasks", Response: map[string]any{"ok": true}}},
				},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "You have 1 task."},
				},
			},
		}

		out := SanitizeHistory(in)
		if len(out) != 1 {
			t.Fatalf("expected 1 content, got %d", len(out))
		}
		if out[0].Role != "model" || out[0].Parts[0].Text != "You have 1 task." {
			t.Errorf("unexpected content: %v", out[0])
		}
	})

	t.Run("strips dangling function call at tail", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role:  "user",
				Parts: []*genai.Part{{Text: "check tasks"}},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{FunctionCall: &genai.FunctionCall{Name: "list_tasks"}},
				},
			},
		}

		out := SanitizeHistory(in)
		if len(out) != 1 {
			t.Fatalf("expected 1 content, got %d", len(out))
		}
		if out[0].Role != "user" || out[0].Parts[0].Text != "check tasks" {
			t.Errorf("unexpected content: %v", out[0])
		}
	})

	t.Run("strips dangling function call in middle but keeps text", func(t *testing.T) {
		in := []*genai.Content{
			{
				Role:  "user",
				Parts: []*genai.Part{{Text: "check tasks"}},
			},
			{
				Role: "model",
				Parts: []*genai.Part{
					{Text: "Let me check."},
					{FunctionCall: &genai.FunctionCall{Name: "list_tasks"}},
				},
			},
			{
				Role:  "user",
				Parts: []*genai.Part{{Text: "nevermind"}},
			},
		}

		out := SanitizeHistory(in)
		if len(out) != 3 {
			t.Fatalf("expected 3 contents, got %d", len(out))
		}
		if len(out[1].Parts) != 1 || out[1].Parts[0].Text != "Let me check." {
			t.Errorf("expected model turn to preserve text and strip dangling function call, got %v", out[1].Parts)
		}
	})
}

func TestDeriveTitle(t *testing.T) {
	tests := []struct {
		name     string
		messages []*genai.Content
		want     string
	}{
		{
			name: "short message",
			messages: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "Hello there"}}},
			},
			want: "Hello there",
		},
		{
			name: "long message capped at 8 words",
			messages: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{{Text: "One two three four five six seven eight nine ten"}}},
			},
			want: "One two three four five six seven eight",
		},
		{
			name: "skips empty or nil parts",
			messages: []*genai.Content{
				{Role: "user", Parts: []*genai.Part{nil, {Text: ""}, {Text: "Valid title"}}},
			},
			want: "Valid title",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveTitle(tt.messages)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("deriveTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}
