package theme

import "testing"

func TestDecodeGeneratedTheme(t *testing.T) {
	seeds, name, err := decodeGenerated(`{
		"name":" Moss & Bone ",
		"primary":"#2d5a4f",
		"secondary":"#607d6d",
		"tertiary":"#a66a4b",
		"neutral":"#727a75"
	}`)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Moss & Bone" {
		t.Fatalf("name = %q", name)
	}
	if seeds.Primary != "#2D5A4F" || seeds.Neutral != "#727A75" {
		t.Fatalf("seeds were not normalized: %+v", seeds)
	}
}

func TestDecodeGeneratedThemeRejectsUnstructuredOutput(t *testing.T) {
	_, _, err := decodeGenerated("```json\n{\"name\":\"Moss\"}\n```")
	if err == nil {
		t.Fatal("expected fenced output to be rejected")
	}
}

func TestSanitizePrompt(t *testing.T) {
	got := sanitizePrompt("  forest\x00 morning  ")
	if got != "forest morning" {
		t.Fatalf("sanitizePrompt() = %q", got)
	}
}

func TestValidateSeedsAllowsOlderThemeWithoutNeutral(t *testing.T) {
	err := ValidateSeeds(&Seeds{
		Primary:   "#2D5A4F",
		Secondary: "#607D6D",
		Tertiary:  "#A66A4B",
	})
	if err != nil {
		t.Fatal(err)
	}
}
