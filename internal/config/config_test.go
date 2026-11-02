package config

import (
	"testing"

	"whydah/internal/i18n"
)

func TestCheckNameTemplate(t *testing.T) {
	for tmpl, want := range map[string]string{
		"{title}":                   "",
		"{uploader}/{date} {title}": "",
		"{site}/{id}":               "",
		"{title} {resolution}":      "error.name_token",
		"{uploader}":                "error.name_unique",
		"../{title}":                "error.name_path",
		"/tmp/{title}":              "error.name_path",
		"~/{title}":                 "error.name_path",
		`a\..\..\{title}`:           "error.name_path",
	} {
		err := CheckNameTemplate(tmpl)
		got := ""
		if e, ok := i18n.As(err); ok {
			got = e.Key
		}
		if got != want {
			t.Errorf("CheckNameTemplate(%q) = %v, want %s", tmpl, err, want)
		}
	}
	if s := normalize(Settings{NameTemplate: "{nope}"}); s.NameTemplate != "{title}" {
		t.Errorf("an invalid template was kept: %q", s.NameTemplate)
	}
}
