package habitperiod

import "testing"

func TestCleanName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"  Workout ", "Workout", false},
		{"", "", true},
		{"   ", "", true},
		{"12345678901234567890", "12345678901234567890", false},
		{"123456789012345678901", "", true},
		{"योगा और ध्यान रोज़ सुबह", "", true}, // 21 runes
		{"ध्यान", "ध्यान", false},
	}
	for _, c := range cases {
		got, err := CleanName(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("CleanName(%q) = %q, %v; want %q, err=%v", c.in, got, err, c.want, c.wantErr)
		}
	}
}
