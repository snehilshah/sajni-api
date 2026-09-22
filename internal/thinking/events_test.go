package thinking

import (
	"errors"
	"strings"
	"testing"
)

func TestStateChangeComments(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		closed  bool
		comment string
		want    string
		wantErr error
	}{
		{"todo without comment", "todo", true, "", "", nil},
		{"todo with comment", "todo", true, "  shipped  ", "shipped", nil},
		{"contradiction needs explanation", "contradiction", true, "  ", "", ErrCommentRequired},
		{"contradiction explained", "contradiction", true, "  both refer to different dates  ", "both refer to different dates", nil},
		{"reopen without comment", "contradiction", false, "", "", nil},
		{"other card cannot close", "fact", true, "reason", "", ErrNotActionable},
		{"long explanation", "contradiction", true, strings.Repeat("x", MaxCommentLength+1), "", ErrCommentTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateStateChange(tt.kind, tt.closed, tt.comment)
			if got != tt.want || !errors.Is(err, tt.wantErr) {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
