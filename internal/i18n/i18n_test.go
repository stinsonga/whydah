package i18n

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		prefs []string
		want  string
	}{
		{[]string{"en-US", "fr"}, "en"},
		{[]string{"en_GB.UTF-8"}, "en"},
		{[]string{"qaa-YY", "en;q=0.5"}, "en"}, // one we don't have (qaa is reserved for private use), then one we do
		{[]string{"", "C"}, "en"},
		{nil, "en"},
	} {
		if got := Match(c.prefs...); got != c.want {
			t.Errorf("Match(%q) = %q, want %q", c.prefs, got, c.want)
		}
	}
	if normalize("pt_br.UTF-8") != "pt-BR" || normalize("ZH") != "zh" {
		t.Error("normalize")
	}
}

func TestT(t *testing.T) {
	if got := T("en", "error.sign_in", map[string]string{"site": "example.com"}); got != "example.com only shows this to people who are signed in." {
		t.Errorf("filled message = %q", got)
	}
	if got := T("en-AU", "error.drm", nil); !strings.Contains(got, "DRM") { // a region falls back to its language
		t.Errorf("en-AU = %q", got)
	}
	if got := T("qaa", "error.drm", nil); !strings.Contains(got, "DRM") { // an unknown language falls back to English
		t.Errorf("qaa = %q", got)
	}
	one := T("en", "error.photos_partial", map[string]string{"count": "1"})
	many := T("en", "error.photos_partial", map[string]string{"count": "3"})
	if one != "Saved 1 photo, but some couldn't be saved." || many != "Saved 3 photos, but some couldn't be saved." {
		t.Errorf("plurals: %q, %q", one, many)
	}
	if got := T("en", "no.such.key", nil); got != "no.such.key" {
		t.Errorf("missing key = %q", got)
	}
}

func TestError(t *testing.T) {
	e := New("error.forbidden", "site", "example.com").WithHint("hint.forbidden").WithDetail("HTTP Error 403: Forbidden")
	err := fmt.Errorf("download: %w", e)
	got, ok := As(err)
	if !ok || got != e {
		t.Fatal("As didn't find the Error")
	}
	if !strings.HasPrefix(e.Error(), "example.com refused the download. A refit") || !strings.HasSuffix(e.Error(), "(HTTP Error 403: Forbidden)") {
		t.Errorf("Error() = %q", e.Error())
	}
	plain := From(errors.New("something odd"))
	if plain.Key != "error.other" || plain.In("en") != "something odd" {
		t.Errorf("From a plain error = %+v", plain)
	}
}

// TestCatalogs checks the messages against the code: every key the Go code
// or the UI uses is in English, English has nothing unused, and every other
// language has only English's keys, with the same {placeholders}.
func TestCatalogs(t *testing.T) {
	loadOnce.Do(load)
	en := catalogs[Default]
	used := usedKeys(t)
	for _, k := range used {
		if _, ok := en[k]; !ok && en[k+".other"] == "" {
			t.Errorf("%s is used but has no English message", k)
		}
	}
	for k := range en {
		base := regexp.MustCompile(`\.(zero|one|two|few|many|other)$`).ReplaceAllString(k, "")
		if k != "language.name" && !contains(used, k) && !contains(used, base) {
			t.Errorf("%s isn't used anywhere", k)
		}
	}
	placeholder := regexp.MustCompile(`\{\w+\}`)
	for lang, m := range catalogs {
		if lang == Default {
			continue
		}
		for k, msg := range m {
			ref, ok := en[k]
			if !ok {
				if base := regexp.MustCompile(`\.(zero|one|two|few|many)$`).ReplaceAllString(k, ""); base != k && en[base+".other"] != "" {
					ref = en[base+".other"] // a plural form English doesn't have
				} else {
					t.Errorf("%s: %s isn't an English key", lang, k)
					continue
				}
			}
			if got, want := sorted(placeholder.FindAllString(msg, -1)), sorted(placeholder.FindAllString(ref, -1)); got != want && !strings.HasPrefix(k, "error.photos") {
				t.Errorf("%s: %s has placeholders %s, English has %s", lang, k, got, want)
			}
		}
	}
}

// usedKeys finds the message keys in the source: i18n.New("..."),
// WithHint("..."), quoted "error.*", "hint.*" and "ui.*" keys in Go, and
// t("...") and data-t="..." in the UI.
func usedKeys(t *testing.T) []string {
	root := filepath.Join("..", "..")
	goKey := regexp.MustCompile(`"((?:error|hint|ui)\.[a-z_.]+)"`)
	uiKey := regexp.MustCompile(`\bt\("([a-z_.]+)"|data-t="([a-z_.]+)"`)
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && (d.Name() == "dist" || d.Name() == "bin" || strings.HasPrefix(d.Name(), ".")) {
			return filepath.SkipDir
		}
		isGo := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		if !isGo && !strings.HasSuffix(path, "index.html") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		re := uiKey
		if isGo {
			re = goKey
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			for _, k := range m[1:] {
				if k != "" {
					seen[k] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sorted(list []string) string {
	sort.Strings(list)
	return strings.Join(list, " ")
}
