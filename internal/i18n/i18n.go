// Package i18n holds Whydah's messages in every language it speaks, for the
// UI and the terminal alike. Each language is a flat JSON file in locales/
// mapping a key to a message; "{name}" in a message is filled in from its
// arguments. A message that depends on a count has one key per plural form
// ("key.one", "key.other", and whichever others the language uses), picked
// by the "count" argument. English is complete; another language may leave
// keys out, and those fall back to English.
package i18n

import (
	"embed"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
)

//go:embed locales/*.json
var files embed.FS

// Default is the language every other falls back to.
const Default = "en"

var (
	loadOnce sync.Once
	catalogs map[string]map[string]string // language -> key -> message
)

func load() {
	catalogs = map[string]map[string]string{}
	entries, _ := files.ReadDir("locales")
	for _, e := range entries {
		data, err := files.ReadFile("locales/" + e.Name())
		if err != nil {
			continue
		}
		var m map[string]string
		if json.Unmarshal(data, &m) != nil {
			panic("i18n: locales/" + e.Name() + " isn't a flat JSON object of strings")
		}
		catalogs[strings.TrimSuffix(e.Name(), path.Ext(e.Name()))] = m
	}
}

// Language is one Whydah speaks, named in itself ("Deutsch", not "German").
type Language struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Languages lists the languages there are messages for, English first.
func Languages() []Language {
	loadOnce.Do(load)
	var out []Language
	for code, m := range catalogs {
		out = append(out, Language{code, firstNonEmpty(m["language.name"], code)})
	}
	sort.Slice(out, func(a, b int) bool {
		if (out[a].Code == Default) != (out[b].Code == Default) {
			return out[a].Code == Default
		}
		return out[a].Name < out[b].Name
	})
	return out
}

// Match picks the best language for the preferences given, most preferred
// first, as browsers ("fr-CA") or the LANG variable ("fr_CA.UTF-8") write
// them: the exact language, else the same one for another region, else English.
func Match(prefs ...string) string {
	loadOnce.Do(load)
	for _, p := range prefs {
		p = normalize(p)
		if p == "" {
			continue
		}
		if _, ok := catalogs[p]; ok {
			return p
		}
		base, _, _ := strings.Cut(p, "-")
		if _, ok := catalogs[base]; ok {
			return base
		}
	}
	return Default
}

func normalize(tag string) string {
	tag, _, _ = strings.Cut(strings.TrimSpace(tag), ".") // "fr_CA.UTF-8"
	tag, _, _ = strings.Cut(tag, ";")                    // "fr;q=0.8"
	tag = strings.ReplaceAll(tag, "_", "-")
	if tag == "C" || tag == "POSIX" || tag == "*" {
		return ""
	}
	base, region, ok := strings.Cut(tag, "-")
	if !ok {
		return strings.ToLower(base)
	}
	return strings.ToLower(base) + "-" + strings.ToUpper(region)
}

// Messages returns every message for lang, with English for any it lacks:
// what the UI needs to show itself in that language.
func Messages(lang string) map[string]string {
	loadOnce.Do(load)
	out := map[string]string{}
	for _, l := range []string{Default, baseOf(lang), lang} {
		for k, v := range catalogs[l] {
			out[k] = v
		}
	}
	return out
}

// T returns the message for key in lang with its arguments filled in. With
// a "count" argument, it uses the key's plural form for that number. A key
// that doesn't exist comes back as itself, so a missing message is visible.
func T(lang, key string, args map[string]string) string {
	loadOnce.Do(load)
	if n, err := strconv.Atoi(args["count"]); err == nil {
		if msg, ok := lookup(lang, key+"."+pluralForm(lang, n)); ok {
			return fill(msg, args)
		}
		if msg, ok := lookup(lang, key+".other"); ok {
			return fill(msg, args)
		}
	}
	if msg, ok := lookup(lang, key); ok {
		return fill(msg, args)
	}
	return key
}

func lookup(lang, key string) (string, bool) {
	for _, l := range []string{lang, baseOf(lang), Default} {
		if msg, ok := catalogs[l][key]; ok {
			return msg, true
		}
	}
	return "", false
}

func baseOf(lang string) string {
	base, _, _ := strings.Cut(lang, "-")
	return base
}

func fill(msg string, args map[string]string) string {
	if len(args) == 0 || !strings.Contains(msg, "{") {
		return msg
	}
	pairs := make([]string, 0, 2*len(args))
	for k, v := range args {
		pairs = append(pairs, "{"+k+"}", v)
	}
	return strings.NewReplacer(pairs...).Replace(msg)
}

// pluralForm is the CLDR plural category of n in lang, for the languages
// whose rules are simple; the UI uses the browser's own rules
// (Intl.PluralRules), so this only matters for the terminal. Add a
// language's rule here when adding a language that needs one.
func pluralForm(lang string, n int) string {
	switch baseOf(lang) {
	case "ja", "zh", "ko", "vi", "th", "id":
		return "other"
	case "fr", "pt":
		if n == 0 || n == 1 {
			return "one"
		}
	default:
		if n == 1 {
			return "one"
		}
	}
	return "other"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
