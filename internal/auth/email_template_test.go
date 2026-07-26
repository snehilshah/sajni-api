package auth

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

func TestMediaReleaseEmailTemplate(t *testing.T) {
	tpl, err := template.ParseFS(emailTemplatesFS, "email_templates/media_release.html")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = tpl.Execute(&out, map[string]any{
		"Name":        "Snehil",
		"MovieTitle":  "The Odyssey",
		"ReleaseDate": "Wednesday, July 15, 2026",
		"AppURL":      "https://www.ohmysajni.com",
		"CTAURL":      "https://www.ohmysajni.com/media?tab=movies",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"The Odyssey", "releases tomorrow", "/media?tab=movies"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("rendered release email does not contain %q", want)
		}
	}
}
