package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeMediaPatch(t *testing.T, raw string) (map[string]any, error) {
	t.Helper()
	var patch mediaPatch
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&patch); err != nil {
		return nil, err
	}
	return patch.values()
}

func TestMediaPatchPreservesNullAndAbsent(t *testing.T) {
	values, err := decodeMediaPatch(t, `{"rating":null,"notes":null}`)
	if err != nil {
		t.Fatal(err)
	}
	if value, exists := values["rating"]; !exists || value != nil {
		t.Fatalf("rating = %#v, exists=%v", value, exists)
	}
	if value := values["notes"]; value != "" {
		t.Fatalf("notes = %#v, want empty clear value", value)
	}
	if _, exists := values["year"]; exists {
		t.Fatal("absent year was included")
	}
}

func TestMediaPatchValidation(t *testing.T) {
	tests := []string{
		`{}`,
		`{"unknown":1}`,
		`{"release_date":"not-a-date"}`,
		`{"rating":11}`,
		`{"episodes_watched":-1}`,
		`{"status":"watched-ish"}`,
		`{"type":"podcast"}`,
		`{"season_episodes":[10,-1]}`,
		`{"rating":"ten"}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			if _, err := decodeMediaPatch(t, raw); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
